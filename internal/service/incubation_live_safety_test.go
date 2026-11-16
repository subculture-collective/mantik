package service

import (
	"context"
	"git.subcult.tv/subculture-collective/mantik/internal/account"
	"git.subcult.tv/subculture-collective/mantik/internal/forward"
	"git.subcult.tv/subculture-collective/mantik/internal/storage"
	"git.subcult.tv/subculture-collective/mantik/internal/strategy"
	"testing"
	"time"
)

type review90FinalLoadStore struct {
	*storage.Memory
	finalLoads    int
	armed         bool
	afterCoverage func()
}

func (s *review90FinalLoadStore) CappedBook(ctx context.Context, id string) (account.CappedState, error) {
	if s.armed {
		s.finalLoads++
		if s.finalLoads == 2 {
			s.armed = false
			s.afterCoverage()
		}
	}
	return s.Memory.CappedBook(ctx, id)
}

func TestIncubationSafetyRegressionWithdrawnSafetyAtFinalLoad(t *testing.T) {
	for _, mode := range []string{"cancel", "failed-cycle"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, req, _, b := incubationFixture(t, false)
			store := &review90FinalLoadStore{Memory: r.Store.(*storage.Memory)}
			r.Store = store
			clock := &incubationSafetyFixture{at: req.DecidedAt, next: make(chan time.Time), waiting: make(chan struct{})}
			worker, err := NewIncubationSafetyWorker(r, clock, IncubationSafetyPolicy{Symbols: []string{"SPY"}, MaximumHolding: time.Hour, Fees: map[string]string{"SPY": "1"}}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); _ = worker.Run(workerCtx, clock) }()
			t.Cleanup(func() { cancel(); <-done })
			<-clock.waiting
			r.Safety = worker
			store.afterCoverage = func() {
				if mode == "cancel" {
					cancel()
					<-done
					return
				}
				clock.mu.Lock()
				clock.fail = true
				clock.mu.Unlock()
				clock.next <- req.ExecuteAt
				<-clock.waiting
			}
			r.Broker = boundaryHealthBroker{incubationTestBroker: b, prepare: func() { store.armed = true }}
			result, err := r.Run(ctx, req)
			state, loadErr := r.Store.CappedBook(ctx, r.BookID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			live := worker.coverage(r.Store, state, req.ExecuteAt, incubationMarks(req.Quotes))
			t.Logf("mode=%s final_loads=%d live=%s liveReady=%v cachedReady=%v posts=%d err=%v", mode, store.finalLoads, live.Code, live.Ready, result.Admission.Checks["worker_liveness"].Ready, b.posts, err)
			if b.posts != 0 {
				t.Fatal("new risk POST after safety coverage was withdrawn")
			}
		})
	}
}

func TestIncubationSafetyRegressionForwardDeadlineCoverage(t *testing.T) {
	ctx := context.Background()
	r, req, _, b, record := forwardRunnerFixture(t)
	out, err := r.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	entry := out.Distribution.Orders[0]
	filled := req.ExecuteAt.Add(time.Second)
	b.fill(t, entry.ID, "review90-forward-fill", entry.Quantity, "100", "0", filled, true)
	if _, err = r.Sync(ctx, filled.Add(time.Second), req.Quotes); err != nil {
		t.Fatal(err)
	}
	expiry := filled.Add(time.Duration(record.Declaration.MaxHoldingSeconds) * time.Second)
	r.Safety = startIncubationTestSafety(t, r, expiry.Add(-time.Second))
	state, err := r.Store.CappedBook(ctx, r.BookID)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := account.ForwardHoldingOpened(state, "trend", expiry)
	if err != nil {
		t.Fatal(err)
	}
	clocks := record.Clocks(expiry, record.SearchTotal, opened)
	check := r.Safety.coverage(r.Store, state, expiry, map[string]string{"SPY": "100"})
	t.Logf("forwardHoldExpired=%v workerCoverageReady=%v code=%s posts=%d", clocks.HoldExpired, check.Ready, check.Code, b.posts)
	if clocks.HoldExpired && check.Ready {
		t.Fatal("due forward holding protection accepted live coverage")
	}
}

func TestIncubationSafetyRegressionForwardDeadlineAllowsUnrelatedEntry(t *testing.T) {
	ctx := context.Background()
	r, req, strat, b, record := forwardRunnerFixture(t)
	d := record.Declaration
	d.ID = "review90-short-hold"
	d.MaxHoldingSeconds = 5
	short, err := r.Store.(forward.Store).DeclareForward(ctx, d, func() time.Time { return req.DecidedAt.Add(-time.Second / 2) })
	if err != nil {
		t.Fatal(err)
	}
	record = short
	req.Strategies[0].ForwardDeclarationID = d.ID
	strat.decide = func(strategy.View) (strategy.Decision, error) {
		return strategy.Decision{Targets: []strategy.Target{{Asset: "SPY", Weight: "0.1"}}}, nil
	}
	second := review90Second(t, r, req)
	second.ForwardDeclarationID = ""
	out, err := r.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	entry := out.Distribution.Orders[0]
	filled := req.ExecuteAt.Add(time.Second)
	b.fill(t, entry.ID, "review90-forward-real-fill", entry.Quantity, "100", "0", filled, true)
	if _, err = r.Sync(ctx, filled.Add(time.Second), req.Quotes); err != nil {
		t.Fatal(err)
	}
	expiry := filled.Add(time.Duration(record.Declaration.MaxHoldingSeconds) * time.Second)
	r.Safety = startIncubationTestSafety(t, r, expiry.Add(-4*time.Second))
	strat.decide = func(strategy.View) (strategy.Decision, error) { return strategy.Decision{}, nil }
	next := nextForwardCycle(req, "review90-after-hold-expiry", expiry.Add(-3*time.Second))
	next.Strategies = append(next.Strategies, second)
	now := next.ExecuteAt
	r.now = func() time.Time { return now }
	r.Broker = boundaryHealthBroker{incubationTestBroker: b, prepare: func() { now = expiry }}
	result, err := r.Run(ctx, next)
	newPosts := 0
	for _, o := range result.Orders {
		if o.Side == "buy" {
			newPosts++
		}
	}
	t.Logf("afterForwardExpiry newBuyPOSTs=%d totalPosts=%d workerReady=%v err=%v", newPosts, b.posts, result.Admission.Checks["worker_liveness"].Ready, err)
	if newPosts != 0 {
		t.Fatal("unrelated new risk POST while retained forward holding protection is due")
	}
}

func TestIncubationSafetyPendingFamilyStillDeliversExit(t *testing.T) {
	ctx := context.Background()
	r, req, strat, broker, record := forwardRunnerFixture(t)
	entry, err := r.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	order := entry.Distribution.Orders[0]
	filled := req.ExecuteAt.Add(time.Second)
	broker.fill(t, order.ID, "pending-safety-entry", order.Quantity, "100", "0", filled, true)
	if _, err = r.Sync(ctx, filled.Add(time.Second), req.Quotes); err != nil {
		t.Fatal(err)
	}
	if err = r.Store.(*storage.Memory).BeginStrategyTrialBatch(ctx, "pending-worker-family", record.Declaration.Family); err != nil {
		t.Fatal(err)
	}
	clock := r.Safety.source.(*incubationSafetyFixture)
	clock.mu.Lock()
	clock.step = time.Millisecond
	clock.mu.Unlock()
	clock.next <- filled.Add(3 * time.Second)
	<-clock.waiting
	if broker.posts != 2 || strat.calls != 1 {
		t.Fatal("pending family stranded worker protection", broker.posts, strat.calls)
	}
	state, err := r.Store.CappedBook(ctx, r.BookID)
	if err != nil {
		t.Fatal(err)
	}
	exits := 0
	for _, event := range state.Events {
		if event.Order != nil && event.Order.ReduceOnly {
			exits++
		}
	}
	if exits != 1 {
		t.Fatal("pending family did not retain exactly one protective order", exits)
	}
	if check := r.Safety.coverage(r.Store, state, clock.Now(), incubationMarks(req.Quotes)); check.Ready {
		t.Fatal("unfilled exit reported as completed protection", check)
	}
}
