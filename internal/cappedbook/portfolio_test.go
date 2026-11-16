package cappedbook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/account"
	"git.subcult.tv/subculture-collective/mantik/internal/allocation"
	"git.subcult.tv/subculture-collective/mantik/internal/risk"
	"git.subcult.tv/subculture-collective/mantik/internal/strategy"
	"git.subcult.tv/subculture-collective/mantik/internal/strategygate"
)

// portfolioFixture supplies a trusted fixture admission to isolate the order
// boundary. Backtest evidence/decision validation has its own gate tests.
func portfolioFixture(t *testing.T) (Client, *fakePaper, allocation.RebalanceRequest, map[string]string) {
	t.Helper()
	c, b, at := paperFixture(t)
	spec := strategy.Spec{Name: "fixture", Version: "1", Data: []string{strategy.KindDailyBars}}
	sid, e := spec.ID()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(spec)
	if e != nil {
		t.Fatal(e)
	}
	d := strategygate.Declaration{Account: "cash", ID: "gate", Family: "portfolio", StrategyID: sid, Spec: raw, Balance: "2000", Policy: "backtest-gate-v1", Benchmark: "cash", DataSHA256: strings.Repeat("a", 64)}
	store := c.Store.(strategygate.Store)
	record, e := store.DeclareStrategyGate(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	h, e := strategygate.Hash(d)
	if e != nil {
		t.Fatal(e)
	}
	_, e = store.RecordStrategyGate(context.Background(), strategygate.Decision{ID: d.ID, StrategyID: sid, DeclarationSHA256: h, FamilySHA256: strings.Repeat("b", 64), Policy: d.Policy, Status: "passed", Stage: "incubation", Minimum: "1", At: record.DeclaredAt, Checks: []strategygate.Check{{Code: "fixture_admission", Passed: true, Reason: "isolated_order_boundary_fixture"}}})
	if e != nil {
		t.Fatal(e)
	}
	r := allocation.RebalanceRequest{ID: "rebalance-one", At: at, Sleeves: []allocation.Sleeve{{ID: "trend", StrategyID: sid, Stage: "incubation", GateID: "gate", Risk: "0.02"}}, Targets: []allocation.SleeveTarget{{SleeveID: "trend", Decision: allocation.TargetDecision{Targets: []allocation.WeightTarget{{Asset: "SPY", Weight: "1"}}}}}, Limits: map[string]string{"SPY": "100"}, Fees: map[string]string{"SPY": "0"}}
	return c, b, r, map[string]string{"SPY": "100"}
}
func portfolioOrder(v allocation.SizedOrder) account.BookOrder {
	return account.BookOrder{ID: v.ID, AllocationID: v.AllocationID, SleeveID: v.SleeveID, StrategyID: v.StrategyID, Symbol: v.Symbol, Quantity: v.Quantity, Limit: v.Limit, Side: v.Side, Fee: v.Fee, At: v.At}
}

func TestPublicSubmitRequiresExactDistributorOrderBeforeAnyBrokerPost(t *testing.T) {
	c, b, r, marks := portfolioFixture(t)
	ctx := context.Background()
	if _, e := c.Submit(ctx, account.BookOrder{ID: "raw", Symbol: "SPY", Quantity: "1", Limit: "100", Side: "buy", Fee: "0", At: r.At}, marks); e == nil || b.posts != 0 {
		t.Fatal("raw target bypassed distributor", e)
	}
	d, e := c.Rebalance(ctx, r, marks)
	if e != nil || len(d.Orders) != 1 {
		t.Fatal(d, e)
	}
	o := portfolioOrder(d.Orders[0])
	changed := o
	changed.Quantity = "2"
	if _, e = c.Submit(ctx, changed, marks); e == nil || b.posts != 0 {
		t.Fatal("quantity bypassed distributor", e)
	}
	if _, e = c.Submit(ctx, o, marks); e != nil || b.posts != 1 {
		t.Fatal("valid sized order failed", e, b.posts)
	}
	r.ID = "second"
	r.At = r.At.Add(time.Minute)
	d, e = c.Rebalance(ctx, r, marks)
	if e != nil || len(d.Orders) != 0 || d.Sleeves[0].Reason != "pending_orders" {
		t.Fatal("pending order reissued", d, e)
	}
}
func TestDepositSpreadsAtNextRebalanceAndFeesStillFitHardPositionCap(t *testing.T) {
	c, b, r, marks := portfolioFixture(t)
	ctx := context.Background()
	d, e := c.Rebalance(ctx, r, marks)
	if e != nil || d.Orders[0].Quantity != "1.000000000" {
		t.Fatal(d, e)
	}
	r.ID = "after-deposit"
	r.At = r.At.AddDate(0, 0, 4)
	r.Fees["SPY"] = "1"
	d, e = c.Rebalance(ctx, r, marks)
	if e != nil || len(d.Orders) != 1 || d.Orders[0].Quantity != "1.249500000" {
		t.Fatal(d, e)
	}
	if _, e = c.Submit(ctx, portfolioOrder(d.Orders[0]), marks); e != nil || b.posts != 1 {
		t.Fatal("own fee made sized order exceed hard cap", e)
	}
}
func TestKillAppliesToAlreadyReservedButNotSubmittedOrderAndResumeDoesNotClearHalt(t *testing.T) {
	c, b, r, marks := portfolioFixture(t)
	ctx := context.Background()
	d, e := c.Rebalance(ctx, r, marks)
	if e != nil {
		t.Fatal(e)
	}
	o := portfolioOrder(d.Orders[0])
	s, e := c.load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s, e = c.Store.ApplyCappedBook(ctx, c.BookID, s.Version, account.BookCommand{ID: "reserve-" + o.ID, Kind: "portfolio_reserve", At: o.At, Order: &o, Marks: marks})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Control(ctx, "kill", r.At, risk.Control{Action: "kill"}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Submit(ctx, o, marks); e == nil || !strings.Contains(e.Error(), "risk_kill_switch") || b.posts != 0 {
		t.Fatal("reserved order escaped kill", e, b.posts)
	}
	if _, e = c.append(ctx, account.BookCommand{ID: "halt", Kind: "halt", At: r.At}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Control(ctx, "resume", r.At, risk.Control{Action: "resume"}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Control(ctx, "unkill", r.At, risk.Control{Action: "unkill"}); e != nil {
		t.Fatal(e)
	}
	restarted := Client{Store: c.Store, Broker: c.Broker, BookID: c.BookID}
	if _, e = restarted.Submit(ctx, o, marks); e == nil || !strings.Contains(e.Error(), "risk_hard_halt") {
		t.Fatal("restart/resume cleared halt", e)
	}
}
func TestHaltStillAllowsSizedReductionAndCannotTurnItIntoAFlip(t *testing.T) {
	c, b, r, marks := portfolioFixture(t)
	ctx := context.Background()
	if _, e := c.Rebalance(ctx, r, marks); e != nil {
		t.Fatal(e)
	}
	// Existing unmanaged Wave8 holdings are retained during the upgrade. A risk
	// breach must allow closing those holdings rather than trapping the book.
	o := account.BookOrder{ID: "legacy", SleeveID: "trend", StrategyID: r.Sleeves[0].StrategyID, Symbol: "SPY", Quantity: "20", Limit: "100", Side: "buy", Fee: "0", At: r.At}
	if _, e := c.submitLedgerTest(ctx, o, marks); e != nil {
		t.Fatal(e)
	}
	b.fills = []json.RawMessage{rawFill("legacy-fill", o.ID, "20", r.At.Add(time.Minute))}
	v := b.orders[o.ID]
	v.FilledQty = "20"
	v.Status = "filled"
	b.orders[o.ID] = v
	if _, e := c.Sync(ctx, r.At.Add(2*time.Minute), marks); e != nil {
		t.Fatal(e)
	}
	marks["SPY"] = "85"
	snap, e := c.Sync(ctx, r.At.Add(3*time.Minute), marks)
	if e != nil || !snap.Halted {
		t.Fatal("loss halt not retained", snap, e)
	}
	r.ID = "emergency-exit"
	r.At = r.At.Add(4 * time.Minute)
	r.Limits["SPY"] = "85"
	d, e := c.Rebalance(ctx, r, marks)
	if e != nil || len(d.Orders) != 1 || d.Orders[0].Side != "sell" {
		t.Fatal(d, e)
	}
	exit := portfolioOrder(d.Orders[0])
	flip := exit
	flip.Quantity = "21"
	if _, e = c.Submit(ctx, flip, marks); e == nil {
		t.Fatal("reduce-only order became a short")
	}
	if _, e = c.Submit(ctx, exit, marks); e != nil {
		t.Fatal("halt trapped reducing exit", e)
	}
	s, e := c.load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	snap, e = account.SnapshotCapped(s, marks, r.At)
	if e != nil || !snap.Halted {
		t.Fatal("exit reset halt", snap, e)
	}
}

func TestNewAssetCannotIncreaseRiskWhileAnotherPositionBreachesItsCap(t *testing.T) {
	c, b, r, marks := portfolioFixture(t)
	ctx := context.Background()
	o := account.BookOrder{ID: "existing", SleeveID: "legacy", Symbol: "SPY", Quantity: "2", Limit: "100", Side: "buy", Fee: "0", At: r.At}
	if _, e := c.submitLedgerTest(ctx, o, marks); e != nil {
		t.Fatal(e)
	}
	b.fills = []json.RawMessage{rawFill("existing-fill", o.ID, "2", r.At.Add(time.Minute))}
	v := b.orders[o.ID]
	v.FilledQty = "2"
	v.Status = "filled"
	b.orders[o.ID] = v
	if _, e := c.Sync(ctx, r.At.Add(2*time.Minute), marks); e != nil {
		t.Fatal(e)
	}
	r.At = r.At.Add(3 * time.Minute)
	r.Targets[0].Decision.Targets[0].Asset = "QQQ"
	r.Limits["QQQ"] = "100"
	r.Fees["QQQ"] = "0"
	marks["QQQ"] = "100"
	d, e := c.Rebalance(ctx, r, marks)
	if e != nil || len(d.Orders) != 1 {
		t.Fatal(d, e)
	}
	if _, e = c.Submit(ctx, portfolioOrder(d.Orders[0]), marks); e == nil || !strings.Contains(e.Error(), "risk_position_limit") || b.posts != 1 {
		t.Fatal("unrelated position breach ignored", e, b.posts)
	}
}
