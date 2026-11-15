package kalshi

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/storage"
)

type simStore struct {
	markets []storage.KalshiMarket
	books   []storage.KalshiOrderBook
}

func (s simStore) KalshiMarketsAsOf(context.Context, time.Time) ([]storage.KalshiMarket, error) {
	return s.markets, nil
}
func (s simStore) KalshiBooksAsOf(context.Context, string, time.Time) ([]storage.KalshiOrderBook, error) {
	return s.books, nil
}
func simFixture() (simStore, time.Time) {
	at := time.Date(2026, 11, 15, 15, 0, 0, 0, time.UTC)
	series := `{"series":{"ticker":"KXTEST","fee_type":"quadratic","fee_multiplier":1}}`
	original := `{"markets":[{"ticker":"KXTEST","event_ticker":"KXTEST-EVENT","status":"active","close_time":"2026-11-16T15:00:00Z"}]}`
	m := storage.KalshiMarket{Ticker: "KXTEST", EventTicker: "KXTEST-EVENT", SeriesTicker: "KXTEST", Status: "active", CloseTime: at.Add(24 * time.Hour).Format(time.RFC3339), ObservedAt: at.Add(-time.Hour), Original: original, SHA256: simHash(original), SeriesOriginal: series, SeriesSHA256: simHash(series)}
	raw := `{"yes_dollars":[["0.55","8"]],"no_dollars":[["0.40","3"],["0.35","2.75"]]}`
	source := `{"orderbook_fp":` + raw + `}`
	b := storage.KalshiOrderBook{MarketTicker: "KXTEST", ObservedAt: at.Add(time.Minute), Book: json.RawMessage(raw), Original: source, SHA256: simHash(source)}
	return simStore{[]storage.KalshiMarket{m}, []storage.KalshiOrderBook{b}}, at
}
func newSim(t *testing.T) *NotionalBook {
	t.Helper()
	b, e := NewNotionalBook(100 * dollarMicros)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestSimulationDepthCashFeesAndIdempotence(t *testing.T) {
	s, at := simFixture()
	b := newSim(t)
	o := SimOrder{ID: "o1", MarketTicker: "KXTEST", Side: "yes", Action: "buy", Limit: "0.65", Contracts: 100, At: at}
	fills, e := b.Execute(context.Background(), s, o, at.Add(2*time.Minute), simEvent(at))
	if e != nil {
		t.Fatal(e)
	}
	if len(fills) != 2 || fills[0].PriceMicros != 600000 || fills[0].Contracts != 3 || fills[1].PriceMicros != 650000 || fills[1].Contracts != 2 {
		t.Fatal(fills)
	}
	r := b.Report()
	if r.CashMicros != 100000000-3*600000-2*650000-r.FeesMicros || r.Positions["KXTEST/yes"] != 5 || r.Kind != "kalshi_notional" || r.IncludedInSPY {
		t.Fatal(r)
	}
	again, e := b.Execute(context.Background(), s, o, at.Add(2*time.Minute), simEvent(at))
	if e != nil || len(again) != 2 || b.Report().CashMicros != r.CashMicros {
		t.Fatal("replay changed book", again, e)
	}
	o.ID = "o2"
	fills, e = b.Execute(context.Background(), s, o, at.Add(3*time.Minute), simEvent(at))
	if e != nil || len(fills) != 0 {
		t.Fatal("reused same depth", fills, e)
	}
	s.books[0].ObservedAt = s.books[0].ObservedAt.Add(time.Minute)
	o.ID = "o3"
	fills, e = b.Execute(context.Background(), s, o, at.Add(4*time.Minute), simEvent(at))
	if e != nil || len(fills) != 0 {
		t.Fatal("poll replenished depth", fills, e)
	}
	// Buying NO consumes YES bids. The two contracts are distinct holdings.
	o.ID = "no"
	o.Side = "no"
	o.Contracts = 2
	fills, e = b.Execute(context.Background(), s, o, at.Add(5*time.Minute), simEvent(at))
	if e != nil || len(fills) != 1 || fills[0].PriceMicros != 450000 {
		t.Fatal(fills, e)
	}
	// No unbacked sell or negative cash is allowed; failure is atomic.
	before := b.Report()
	o.ID = "short"
	o.Action = "sell"
	o.Contracts = 3
	if _, e := b.Execute(context.Background(), s, o, at.Add(6*time.Minute), simEvent(at)); e == nil {
		t.Fatal("short admitted")
	}
	if b.Report().CashMicros != before.CashMicros {
		t.Fatal("failed order mutated cash")
	}
}
func officialResult(at time.Time, yes bool) storage.KalshiWeatherSettlement {
	result := "no"
	if yes {
		result = "yes"
	}
	raw, _ := json.Marshal(map[string]any{"market": map[string]any{"ticker": "KXTEST", "status": "finalized", "result": result, "close_time": at.Add(-time.Hour), "settlement_ts": at}})
	return storage.KalshiWeatherSettlement{MarketTicker: "KXTEST", Yes: yes, SettledAt: at, ObservedAt: at, Original: string(raw), SHA256: simHash(string(raw))}
}
func TestSimulationOfficialBinarySettlementAndIsolation(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Run(map[bool]string{false: "no", true: "yes"}[yes], func(t *testing.T) {
			s, at := simFixture()
			b := newSim(t)
			for _, side := range []string{"yes", "no"} {
				_, e := b.Execute(context.Background(), s, SimOrder{ID: side, MarketTicker: "KXTEST", Side: side, Action: "buy", Limit: "0.9", Contracts: 2, At: at}, at.Add(2*time.Minute), simEvent(at))
				if e != nil {
					t.Fatal(e)
				}
			}
			settledAt := at.Add(25 * time.Hour)
			r := officialResult(settledAt, yes)
			before := b.Report()
			if e := b.Settle(context.Background(), settlementRows{r}, "KXTEST", settledAt.Add(-time.Second)); e == nil {
				t.Fatal("future result admitted")
			}
			if e := b.Settle(context.Background(), settlementRows{r}, "KXTEST", settledAt); e != nil {
				t.Fatal(e)
			}
			after := b.Report()
			if after.CashMicros-before.CashMicros != 2*dollarMicros || after.PayoutMicros != 2*dollarMicros || len(after.Positions) != 0 || after.IncludedInSPY {
				t.Fatal(after)
			}
			if e := b.Settle(context.Background(), settlementRows{r}, "KXTEST", settledAt); e != nil || b.Report().CashMicros != after.CashMicros {
				t.Fatal("duplicate settlement", e)
			}
			if e := b.Settle(context.Background(), settlementRows{officialResult(settledAt, !yes)}, "KXTEST", settledAt); e == nil {
				t.Fatal("conflict accepted")
			}
			after.Positions["fake"] = 100
			after.Fills[0].Contracts = 999
			if b.Report().Positions["fake"] != 0 || b.Report().Fills[0].Contracts == 999 {
				t.Fatal("report mutated book")
			}
		})
	}
}
func TestSimulationRejectsFutureDataFeeUnknownsAndNoLaterBook(t *testing.T) {
	for name, change := range map[string]func(*simStore, time.Time){
		"future book":   func(s *simStore, at time.Time) { s.books[0].ObservedAt = at.Add(time.Hour) },
		"decision book": func(s *simStore, at time.Time) { s.books[0].ObservedAt = at },
		"future market": func(s *simStore, at time.Time) { s.markets[0].ObservedAt = at.Add(time.Hour) },
		"unknown fee": func(s *simStore, _ time.Time) {
			s.markets[0].SeriesOriginal = `{"series":{"ticker":"KXTEST","fee_type":"flat","fee_multiplier":1}}`
			s.markets[0].SeriesSHA256 = simHash(s.markets[0].SeriesOriginal)
		},
		"missing multiplier": func(s *simStore, _ time.Time) {
			s.markets[0].SeriesOriginal = `{"series":{"ticker":"KXTEST","fee_type":"quadratic"}}`
			s.markets[0].SeriesSHA256 = simHash(s.markets[0].SeriesOriginal)
		},
		"closed": func(s *simStore, _ time.Time) { s.markets[0].Status = "finalized" },
	} {
		t.Run(name, func(t *testing.T) {
			s, at := simFixture()
			change(&s, at)
			b := newSim(t)
			if _, e := b.Execute(context.Background(), s, SimOrder{ID: "test", MarketTicker: "KXTEST", Side: "yes", Action: "buy", Limit: "0.9", Contracts: 1, At: at}, at.Add(2*time.Minute), simEvent(at)); e == nil {
				t.Fatal("invalid source accepted")
			}
			if len(b.Report().Fills) != 0 {
				t.Fatal("failed order mutated book")
			}
		})
	}
}
func TestSimulationFeeWorkedExamples(t *testing.T) {
	for _, tc := range []struct{ price, qty, want int64 }{{500000, 100, 1750000}, {100000, 100, 630000}, {600000, 3, 50400}, {650000, 2, 31850}} {
		got, e := takerFee(tc.price, tc.qty, big.NewRat(1, 1))
		if e != nil || got != tc.want {
			t.Fatal(tc, got, e)
		}
	}
}

type settlementRows []storage.KalshiWeatherSettlement

func (r settlementRows) KalshiWeatherSettlements(context.Context, time.Time) ([]storage.KalshiWeatherSettlement, error) {
	return r, nil
}

func simEvent(at time.Time) EventObservation {
	raw := `{"event":{"event_ticker":"KXTEST-EVENT","series_ticker":"KXTEST","fee_type_override":null,"fee_multiplier_override":null}}`
	return EventObservation{Ticker: "KXTEST-EVENT", Original: raw, SHA256: simHash(raw), ObservedAt: at}
}

func TestEventFeeEvidenceAndRawSourceConsistency(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*simStore, *EventObservation, time.Time){
		"missing event":  func(_ *simStore, e *EventObservation, _ time.Time) { *e = EventObservation{} },
		"future event":   func(_ *simStore, e *EventObservation, at time.Time) { e.ObservedAt = at.Add(time.Hour) },
		"fee after fill": func(_ *simStore, e *EventObservation, at time.Time) { e.ObservedAt = at.Add(90 * time.Second) },
		"wrong event":    func(_ *simStore, e *EventObservation, _ time.Time) { e.Ticker = "OTHER" },
		"raw-normalized mismatch": func(s *simStore, _ *EventObservation, _ time.Time) {
			s.books[0].Book = json.RawMessage(`{"yes_dollars":[],"no_dollars":[["0.40","10000"]]}`)
		},
		"raw market mismatch": func(s *simStore, _ *EventObservation, _ time.Time) { s.markets[0].CloseTime = "2026-11-17T15:00:00Z" },
		"unsupported event fee": func(_ *simStore, e *EventObservation, _ time.Time) {
			e.Original = `{"event":{"event_ticker":"KXTEST-EVENT","series_ticker":"KXTEST","fee_type_override":"flat"}}`
			e.SHA256 = simHash(e.Original)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, at := simFixture()
			event := simEvent(at)
			change(&s, &event, at)
			b := newSim(t)
			if _, e := b.Execute(ctx, s, SimOrder{ID: "test", MarketTicker: "KXTEST", Side: "yes", Action: "buy", Limit: "0.9", Contracts: 1, At: at}, at.Add(2*time.Minute), event); e == nil {
				t.Fatal("invalid fee/source evidence accepted")
			}
		})
	}
	s, at := simFixture()
	event := simEvent(at)
	event.Original = `{"event":{"event_ticker":"KXTEST-EVENT","series_ticker":"KXTEST","fee_multiplier_override":0}}`
	event.SHA256 = simHash(event.Original)
	b := newSim(t)
	fills, e := b.Execute(ctx, s, SimOrder{ID: "zero-event-fee", MarketTicker: "KXTEST", Side: "yes", Action: "buy", Limit: "0.9", Contracts: 1, At: at}, at.Add(2*time.Minute), event)
	if e != nil || len(fills) != 1 || fills[0].FeeMicros != 0 {
		t.Fatal("event override ignored", fills, e)
	}
	if e := b.Settle(ctx, settlementRows{}, "KXTEST", at.Add(48*time.Hour)); e == nil {
		t.Fatal("invented official settlement")
	}
}

func TestSimulationCannotSpendUnknownCashOrConsumeOppositeDepthTwice(t *testing.T) {
	s, at := simFixture()
	ctx := context.Background()
	event := simEvent(at)
	tiny, e := NewNotionalBook(1)
	if e != nil {
		t.Fatal(e)
	}
	order := SimOrder{ID: "tiny", MarketTicker: "KXTEST", Side: "yes", Action: "buy", Limit: "0.9", Contracts: 1, At: at}
	if _, e := tiny.Execute(ctx, s, order, at.Add(2*time.Minute), event); e == nil {
		t.Fatal("overspent cash")
	}
	if tiny.Report().CashMicros != 1 || len(tiny.Report().Fills) != 0 {
		t.Fatal("failed order changed book")
	}
	b := newSim(t)
	order.ID = "yes"
	order.Contracts = 3
	if _, e := b.Execute(ctx, s, order, at.Add(2*time.Minute), event); e != nil {
		t.Fatal(e)
	}
	order.ID = "no"
	order.Side = "no"
	order.Contracts = 2
	if _, e := b.Execute(ctx, s, order, at.Add(2*time.Minute), event); e != nil {
		t.Fatal(e)
	}
	order.ID = "sell-no"
	order.Action = "sell"
	order.Limit = "0.4"
	fills, e := b.Execute(ctx, s, order, at.Add(2*time.Minute), event)
	if e != nil || len(fills) != 0 {
		t.Fatal("same resting NO bids consumed by YES buy and NO sell", fills, e)
	}
}
