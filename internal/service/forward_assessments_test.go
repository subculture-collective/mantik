package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/forward"
	"git.subcult.tv/subculture-collective/mantik/internal/storage"
	"git.subcult.tv/subculture-collective/mantik/internal/strategy"
	"git.subcult.tv/subculture-collective/mantik/internal/strategytrial"
)

// Compiled service fixture with an isolated in-memory venue; no provider calls.
func qualificationRunnerFixture(t *testing.T) (IncubationRunner, IncubationRequest, *incubationTestStrategy, *incubationTestBroker, forward.Declaration, forward.AssessmentPlan) {
	t.Helper()
	ctx := context.Background()
	r, req, s, b := incubationFixture(t, false)
	bench := strategy.Spec{Name: "cash", Version: "1", Data: []string{strategy.KindDailyBars}}
	bid, _ := bench.ID()
	sid, _ := s.Spec().ID()
	d := forward.Declaration{ID: "qualified-runner", Family: "incubation-fixture", GateID: req.Strategies[0].GateID, BookID: r.BookID, SleeveID: req.Strategies[0].SleeveID, StrategyID: sid, Spec: s.Spec(), Benchmark: forward.Benchmark{StrategyID: bid, Spec: bench}, WindowStart: req.DecidedAt, WindowEnd: req.DecidedAt.Add(7 * 24 * time.Hour), OutcomeDeadline: req.DecidedAt.Add(8 * 24 * time.Hour), MaxHoldingSeconds: 60, EligibilityTTLSeconds: 7 * 86400, MinSessions: 6, MinIndependentDecisions: 6, MinimumNetExcess: "0.01", MaximumDrawdown: "0.15", MaxSearchTrials: 4}
	at := req.DecidedAt.Add(-time.Second / 2)
	if _, e := r.Store.(forward.Store).DeclareForward(ctx, d, func() time.Time { return at }); e != nil {
		t.Fatal(e)
	}
	fs := r.Store.(forward.AssessmentStore)
	p := forward.AssessmentPlan{ID: "runner-assessment", DeclarationID: d.ID, Policy: forward.AssessmentPolicy, TrainEnd: d.WindowStart, HoldoutStart: d.WindowStart.Add(2 * time.Minute), PurgeSeconds: 60, EmbargoSeconds: 60, AlphaBasisPoints: 500}
	if _, e := fs.DeclareForwardAssessment(ctx, p, func() time.Time { return at }); e != nil {
		t.Fatal(e)
	}
	req.Strategies[0].ForwardDeclarationID = d.ID
	return r, req, s, b, d, p
}
func TestQualificationRunnerCollectsDistinctEvidence(t *testing.T) {
	ctx := context.Background()
	r, req, s, b, d, p := qualificationRunnerFixture(t)
	bid := d.Benchmark.StrategyID
	sid := d.StrategyID
	at := d.WindowStart.Add(-time.Second / 2)
	fs := r.Store.(forward.AssessmentStore)
	for day := range 6 {
		for leg := range 2 {
			next := nextForwardCycle(req, fmt.Sprintf("qual-%d-%d", day, leg), p.HoldoutStart.Add(time.Duration(day)*24*time.Hour+time.Duration(leg*15)*time.Second))
			point := forward.BenchmarkObservation{StrategyID: bid, Index: "100", EvidenceClass: "prospective_market_unqualified", ObservedAt: next.ExecuteAt, ReceivedAt: next.ExecuteAt}
			point.Raw, _ = json.Marshal(struct {
				StrategyID, Index, EvidenceClass string
				ObservedAt, ReceivedAt           time.Time
			}{point.StrategyID, point.Index, point.EvidenceClass, point.ObservedAt, point.ReceivedAt})
			point.SourceSHA256 = forward.SourceDigest(point.Raw)
			next.Qualification = &forward.Collection{PlanID: p.ID, Benchmark: point}
			price := "100"
			if leg == 1 {
				price = "110"
				s.decide = func(strategy.View) (strategy.Decision, error) { return strategy.Decision{}, nil }
				q := next.Quotes["SPY"]
				q.Bid = "109.99"
				q.Ask = "110.01"
				next.Quotes["SPY"] = q
				next.Limits = map[string]string{"SPY": "109"}
			} else {
				s.decide = nil
			}
			r.now = func() time.Time { return next.ExecuteAt }
			healthyIncubationWorkers(t, r, next.ExecuteAt)
			out, e := r.Run(ctx, next)
			if e != nil || len(out.Distribution.Orders) != 1 {
				t.Fatal("collection", day, leg, e, len(out.Distribution.Orders))
			}
			if out.Plan.Qualification == nil || len(out.Plan.Qualification.DecisionSources) != 1 || out.QualificationCredit {
				t.Fatal("missing compiled sources")
			}
			o := out.Distribution.Orders[0]
			b.fill(t, o.ID, fmt.Sprintf("qual-fill-%d-%d", day, leg), o.Quantity, price, "0", next.ExecuteAt.Add(time.Second), true)
			if _, e = r.Sync(ctx, next.ExecuteAt.Add(2*time.Second), next.Quotes); e != nil {
				t.Fatal(e)
			}
		}
	}
	// The start ledger is captured at assessment, not cached at declaration.
	raw, _ := json.Marshal(s.Spec())
	if _, e := r.Store.(strategytrial.Store).StartStrategyTrial(ctx, strategytrial.Start{ID: "actual-search", Family: d.Family, StrategyID: sid, Spec: raw, Config: json.RawMessage(`{"contract_fixture":true}`), DataSHA256: incubationDigest("trial-data"), EvidenceClass: "synthetic_fixture", StartedAt: at}); e != nil {
		t.Fatal(e)
	}
	result, e := fs.AssessForward(ctx, p.ID, func() time.Time { return d.OutcomeDeadline })
	if e != nil || result.Status != "passed" || result.Sessions != 6 || s.calls != 12 {
		t.Fatal(result.Status, result.Reason, e, s.calls)
	}

	next := nextForwardCycle(req, "promoted-real-cycle", d.OutcomeDeadline.Add(time.Second))
	next.Qualification = nil
	s.decide = nil
	r.now = func() time.Time { return next.ExecuteAt }
	healthyIncubationWorkers(t, r, next.ExecuteAt)
	out, e := r.Run(ctx, next)
	if e != nil || len(out.Distribution.Orders) != 1 || out.Plan.Rebalance.Sleeves[0].Stage != "qualified" {
		t.Fatal("promoted runner refused allocation", e)
	}
	budget, ok := new(big.Rat).SetString(out.Distribution.Sleeves[0].Budget)
	if !ok || budget.Cmp(big.NewRat(100, 1)) <= 0 {
		t.Fatal("qualified pool did not take effect", out.Distribution.Sleeves)
	}
}

func TestForwardAssessmentHTTPRejectsImportedOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, token, auth, method, path, body string
		want                                  int
	}{
		{"unauthenticated", "configured", "", "POST", "/v1/forward-assessment-plans/missing/assess", "", 401},
		{"unconfigured", "", "Bearer configured", "POST", "/v1/forward-assessment-plans/missing/assess", "", 401},
		{"pass-boolean", "configured", "Bearer configured", "POST", "/v1/forward-assessment-plans/missing/assess", `{"passed":true}`, 422},
		{"imported-result", "configured", "Bearer configured", "POST", "/v1/forward-assessment-plans", `{"QualificationCredit":true}`, 422},
		{"query", "configured", "Bearer configured", "POST", "/v1/forward-assessment-plans/missing/assess?passed=true", "", 422},
		{"missing-plan", "configured", "Bearer configured", "POST", "/v1/forward-assessment-plans/missing/assess", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler(storage.NewMemory(), tc.token)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != tc.want {
				t.Fatal(out.Code, out.Body.String())
			}
		})
	}
}

func TestQualificationRejectsMissingVenueFee(t *testing.T) {
	ctx := context.Background()
	r, req, _, b, d, p := qualificationRunnerFixture(t)
	next := nextForwardCycle(req, "missing-fee", p.HoldoutStart)
	point := forward.BenchmarkObservation{StrategyID: d.Benchmark.StrategyID, Index: "100", EvidenceClass: "prospective_market_unqualified", ObservedAt: next.ExecuteAt, ReceivedAt: next.ExecuteAt}
	point.Raw, _ = json.Marshal(struct {
		StrategyID, Index, EvidenceClass string
		ObservedAt, ReceivedAt           time.Time
	}{point.StrategyID, point.Index, point.EvidenceClass, point.ObservedAt, point.ReceivedAt})
	point.SourceSHA256 = forward.SourceDigest(point.Raw)
	next.Qualification = &forward.Collection{PlanID: p.ID, Benchmark: point}
	r.now = func() time.Time { return next.ExecuteAt }
	healthyIncubationWorkers(t, r, next.ExecuteAt)
	out, e := r.Run(ctx, next)
	if e != nil {
		t.Fatal(e)
	}
	o := out.Distribution.Orders[0]
	b.fill(t, o.ID, "missing-fee-fill", o.Quantity, "100", "0", next.ExecuteAt.Add(time.Second), true)
	var raw map[string]any
	if e = json.Unmarshal(b.fills[0], &raw); e != nil {
		t.Fatal(e)
	}
	delete(raw, "commission")
	b.fills[0], e = json.Marshal(raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Sync(ctx, next.ExecuteAt.Add(2*time.Second), next.Quotes); e == nil || !strings.Contains(e.Error(), "no observed commission") {
		t.Fatal("missing fee became invented zero", e)
	}
}
