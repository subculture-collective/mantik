package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/marketdata"
	"git.subcult.tv/subculture-collective/mantik/internal/storage"
	"git.subcult.tv/subculture-collective/mantik/internal/strategy"
)

const asset = "asset-x"

func closeAt(d int) time.Time { return time.Date(2026, 1, d, 21, 15, 0, 0, time.UTC) }

// source stores bars for January 5–9 as [open, close] pairs and reads them
// with the given corporate actions.
type source struct {
	m       *storage.Memory
	actions []marketdata.CorporateAction
}

func newSource(t *testing.T, prices [][2]string, actions ...marketdata.CorporateAction) source {
	t.Helper()
	m := storage.NewMemory()
	bars := []storage.DailyBar{}
	for i, p := range prices {
		at := closeAt(5 + i)
		bars = append(bars, storage.DailyBar{AssetID: asset, Symbol: "XYZ", Date: at.Format(time.DateOnly), Open: p[0], High: p[1], Low: p[0], Close: p[1], VWAP: p[1], Volume: 1000, TradeCount: 10, PublishedAt: at, ObservedAt: at, AvailableAt: at})
	}
	if _, err := m.RecordDailyBars(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	return source{m, actions}
}

func (s source) AdjustedBars(ctx context.Context, a string, asOf time.Time) (marketdata.Series, error) {
	return marketdata.Read(ctx, s.m, a, s.actions, asOf)
}

// allIn holds the whole sleeve in the asset.
type allIn struct{ warmup int }

func (s allIn) Spec() strategy.Spec {
	return strategy.Spec{Name: "all-in", Version: "1", Data: []string{strategy.KindDailyBars}, Warmup: s.warmup}
}
func (allIn) Decide(v strategy.View) (strategy.Decision, error) {
	return strategy.Decision{Targets: []strategy.Target{{Asset: asset, Weight: "1"}}}, nil
}

var prices = [][2]string{{"100", "101"}, {"102", "104"}, {"103", "103"}, {"105", "106"}, {"106", "108"}}

func config(src source, s strategy.Strategy, cost CostModel) Config {
	return Config{Strategy: s, Source: src, Assets: []string{asset}, Start: closeAt(1), End: closeAt(9).Add(time.Hour), InitialCash: "1000", Cost: cost}
}

func TestFillsUseTheNextSessionsOpenWithCosts(t *testing.T) {
	src := newSource(t, prices)
	cost := CostModel{Version: "test-v1", FeeBps: "10", SlippageBps: "5"}
	r, err := Run(context.Background(), config(src, allIn{}, cost))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Days) != 5 || len(r.Fills) == 0 || r.Cost != cost {
		t.Fatalf("%+v", r)
	}
	// Decided at the January 5 close, filled at the January 6 open of 102,
	// five basis points worse, with a ten basis point fee.
	f := r.Fills[0]
	if f.Date != "2026-01-06" || f.DecidedOn != "2026-01-05" || f.Open != "102" || f.Price != "102.051" || f.Fee != "1.0005" {
		t.Fatalf("first fill %+v", f)
	}
	opens := map[string]string{}
	for i, p := range prices {
		opens[closeAt(5+i).Format(time.DateOnly)] = p[0]
	}
	for _, f := range r.Fills {
		if f.Date <= f.DecidedOn || f.Open != opens[f.Date] {
			t.Fatalf("fill used the deciding session's price: %+v", f)
		}
	}
	// The last decision has no later session to fill at.
	if len(r.Pending) != 1 || r.Pending[0].Weight != "1" {
		t.Fatal("pending", r.Pending)
	}
	if r.Days[1].Cash != "-1.5005" {
		t.Fatal("cash after the first fill", r.Days[1].Cash)
	}
}

func TestRunsAreByteIdentical(t *testing.T) {
	cost := CostModel{Version: "test-v1", FeeBps: "3", SlippageBps: "7"}
	var first []byte
	for i := 0; i < 3; i++ {
		r, err := Run(context.Background(), config(newSource(t, prices), allIn{}, cost))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = raw
		} else if !bytes.Equal(first, raw) {
			t.Fatal("runs differ")
		}
	}
}

func TestSplitsAndDividendsReachTheBook(t *testing.T) {
	known := closeAt(1)
	src := newSource(t, [][2]string{{"100", "100"}, {"100", "100"}, {"50", "50"}, {"49", "49"}, {"50", "50"}},
		marketdata.CorporateAction{SourceID: "s", Kind: "split", ExDate: "2026-01-07", From: "1", To: "2", KnownAt: known},
		marketdata.CorporateAction{SourceID: "d", Kind: "cash_dividend", ExDate: "2026-01-08", PayDate: "2026-01-09", Amount: "1", KnownAt: known})
	r, err := Run(context.Background(), config(src, allIn{}, CostModel{Version: "free", FeeBps: "0", SlippageBps: "0"}))
	if err != nil {
		t.Fatal(err)
	}
	// Ten shares bought at 100 become twenty at 50. The ex-date drops the
	// price to 49 before the $20 arrives; on the pay date it buys 0.4 shares.
	d := r.Days
	if d[2].Positions[0].Shares != "20" || d[2].Equity != "1000" || d[3].Equity != "980" || d[4].Cash != "0" || d[4].Equity != "1020" {
		t.Fatalf("%+v", d)
	}
	last := r.Fills[len(r.Fills)-1]
	if last.Date != "2026-01-09" || last.Shares != "0.4" {
		t.Fatalf("dividend not reinvested: %+v", last)
	}
	if d[4].Return != "0.040816326531" {
		t.Fatal("return", d[4].Return)
	}
}

func TestWarmupDelaysTheFirstDecision(t *testing.T) {
	r, err := Run(context.Background(), config(newSource(t, prices), allIn{warmup: 3}, DefaultCost))
	if err != nil {
		t.Fatal(err)
	}
	if r.Days[1].Decided || !r.Days[2].Decided || r.Fills[0].Date != "2026-01-08" {
		t.Fatalf("%+v %+v", r.Days, r.Fills)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	src := newSource(t, prices)
	for name, change := range map[string]func(*Config){
		"cost version": func(c *Config) { c.Cost.Version = "" },
		"cost text":    func(c *Config) { c.Cost.FeeBps = "1e1" },
		"no cash":      func(c *Config) { c.InitialCash = "0" },
		"no assets":    func(c *Config) { c.Assets = nil },
		"reversed":     func(c *Config) { c.Start, c.End = c.End, c.Start },
	} {
		c := config(src, allIn{}, DefaultCost)
		change(&c)
		if _, err := Run(context.Background(), c); !errors.Is(err, ErrInvalidConfig) {
			t.Error(name, err)
		}
	}
}
