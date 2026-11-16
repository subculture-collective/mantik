package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/account"
	"git.subcult.tv/subculture-collective/mantik/internal/alpaca"
	"git.subcult.tv/subculture-collective/mantik/internal/service"
	"git.subcult.tv/subculture-collective/mantik/internal/storage"
	"git.subcult.tv/subculture-collective/mantik/internal/strategy/library"
	"git.subcult.tv/subculture-collective/mantik/internal/strategygate"
)

type supervisionStore struct {
	*storage.Memory
	gateReads atomic.Int64
}

func (s *supervisionStore) StrategyGate(ctx context.Context, id string) (strategygate.Record, error) {
	s.gateReads.Add(1)
	return s.Memory.StrategyGate(ctx, id)
}

type supervisionBroker struct{ fills, posts atomic.Int64 }

func (*supervisionBroker) PaperAccountID() string                         { return "supervision-fixture" }
func (*supervisionBroker) AbortUnsubmitted(context.Context, string) error { return nil }
func (b *supervisionBroker) Submit(context.Context, alpaca.StockOrder) (alpaca.PaperOrder, error) {
	b.posts.Add(1)
	return alpaca.PaperOrder{}, errors.New("fixture must not submit")
}
func (*supervisionBroker) Order(context.Context, string) (alpaca.PaperOrder, error) {
	return alpaca.PaperOrder{}, alpaca.ErrPaperNotSubmitted
}
func (b *supervisionBroker) Fills(context.Context, time.Time) ([]json.RawMessage, error) {
	b.fills.Add(1)
	return nil, nil
}

func privateSupervisionJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestIncubationSupervisionKeepsSafetyRunningAfterCycleRefusal(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-independent-quotes", true: "current-independent-quotes"}[available], func(t *testing.T) {
			root := t.TempDir()
			quotesPath, configPath, cyclePath := filepath.Join(root, "quotes.json"), filepath.Join(root, "safety.json"), filepath.Join(root, "cycle.json")
			at := time.Now().UTC().Add(-time.Second)
			quotes := map[string]account.IncubationQuote{"SPY": {Symbol: "SPY", Bid: "99.99", Ask: "100.01", BidQuantity: "100", AskQuantity: "100", OccurredAt: at, ReceivedAt: at, SourceSHA256: strings.Repeat("a", 64), EvidenceClass: "prospective_market_unqualified"}}
			if available {
				privateSupervisionJSON(t, quotesPath, quotes)
			}
			privateSupervisionJSON(t, configPath, incubationSafetyInput{QuotesFile: quotesPath, Symbols: []string{"SPY"}, Fees: map[string]string{"SPY": "1"}, MaximumHoldingSeconds: 3600, IntervalSeconds: 1})
			cfg := library.Default(library.Trend)
			privateSupervisionJSON(t, cyclePath, incubationInput{ID: "refused-cycle", DecidedAt: at.Add(-time.Second), ExecuteAt: at, Quotes: quotes, Strategies: []incubationStrategyInput{{SleeveID: "trend", GateID: "missing-gate", Library: &cfg, Assets: []string{"SPY"}}}})
			store := &supervisionStore{Memory: storage.NewMemory()}
			if _, err := store.CreateCappedBook(context.Background(), account.DefaultCappedConfig("supervised-book", "supervision-fixture", account.Cash, at.Add(-time.Minute))); err != nil {
				t.Fatal(err)
			}
			broker := &supervisionBroker{}
			runner := service.IncubationRunner{Store: store, Broker: broker, BookID: "supervised-book"}
			ctx, cancel := context.WithTimeout(context.Background(), 2200*time.Millisecond)
			defer cancel()
			var out bytes.Buffer
			err := runIncubationSupervision(ctx, runner, cyclePath, configPath, &out)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			var events []incubationSupervisionEvent
			decoder := json.NewDecoder(&out)
			for decoder.More() {
				var e incubationSupervisionEvent
				if err := decoder.Decode(&e); err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			if len(events) == 0 || events[len(events)-1].Kind != "supervision_stopped" || broker.posts.Load() != 0 {
				t.Fatal(events, broker.posts.Load())
			}
			cycles := 0
			for _, e := range events {
				if e.Kind == "cycle" {
					cycles++
					if e.CycleID != "refused-cycle" || e.Error == "" {
						t.Fatal(e)
					}
				}
			}
			if available {
				if store.gateReads.Load() != 1 || cycles != 1 || broker.fills.Load() < 2 {
					t.Fatal("entry refusal stopped safety or replayed unchanged input", events, store.gateReads.Load(), broker.fills.Load())
				}
			} else if store.gateReads.Load() != 0 || cycles != 0 || broker.fills.Load() != 0 {
				t.Fatal("missing safety data reached entry or reconciliation", events)
			}
		})
	}
}

func TestIncubationSafetyFileRefusesUnsafeOrExpiredSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	at := time.Now().UTC()
	q := account.IncubationQuote{Symbol: "SPY", Bid: "99.99", Ask: "100.01", BidQuantity: "100", AskQuantity: "100", OccurredAt: at, ReceivedAt: at, SourceSHA256: strings.Repeat("b", 64), EvidenceClass: "prospective_market_unqualified"}
	privateSupervisionJSON(t, path, map[string]account.IncubationQuote{"SPY": q})
	source := incubationSafetyFile(path)
	if _, err := source.Quotes(context.Background(), []string{"SPY"}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Quotes(context.Background(), []string{"SPY"}, at.Add(time.Minute+time.Nanosecond)); err == nil {
		t.Fatal("expired independent quote accepted")
	}
	if _, err := source.Quotes(context.Background(), []string{"QQQ"}, at); err == nil {
		t.Fatal("uncovered symbol accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Quotes(context.Background(), []string{"SPY"}, at); err == nil {
		t.Fatal("non-private safety source accepted")
	}
}
