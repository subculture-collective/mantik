package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/account"
	"git.subcult.tv/subculture-collective/mantik/migrations"
)

func cappedContracts(t *testing.T, s CappedBookStore) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	cfg := account.CappedConfig{ID: "book", BrokerAccountID: "paper-account", Type: account.Cash, OpenedAt: at, OpeningCash: "2000", DepositAmount: "500", EveryWeeks: 2}
	state, e := s.CreateCappedBook(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateCappedBook(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	bad := cfg
	bad.OpeningCash = "100000"
	if _, e = s.CreateCappedBook(ctx, bad); !errors.Is(e, account.ErrBookConflict) {
		t.Fatal("configuration changed", e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"first", "second"} {
		wg.Go(func() {
			o := account.BookOrder{ID: id, Symbol: "SPY", Quantity: "15", Limit: "100", Side: "buy", Fee: "0", At: at}
			_, e := s.ApplyCappedBook(ctx, cfg.ID, state.Version, account.BookCommand{ID: "reserve-" + id, Kind: "reserve", At: at, Order: &o, Marks: map[string]string{"SPY": "100"}})
			errs <- e
		})
	}
	wg.Wait()
	close(errs)
	n := 0
	for e := range errs {
		if e == nil {
			n++
		}
	}
	if n != 1 {
		t.Fatal("concurrent cap not serialized", n)
	}
	current, e := s.CappedBook(ctx, cfg.ID)
	if e != nil || current.Version != 1 {
		t.Fatal(current, e)
	}
	cmd := current.Events[0]
	again, e := s.ApplyCappedBook(ctx, cfg.ID, 0, cmd)
	if e != nil || again.Version != 1 {
		t.Fatal("retry counted twice", again, e)
	}
	cmd.Order.Limit = "1"
	if _, e = s.ApplyCappedBook(ctx, cfg.ID, 1, cmd); !errors.Is(e, account.ErrBookConflict) {
		t.Fatal("intent changed", e)
	}
	current, e = s.CappedBook(ctx, cfg.ID)
	if e != nil || current.Events[0].Order.Limit != "100" {
		t.Fatal("read modified retained state", current, e)
	}
}
func TestMemoryCappedBook(t *testing.T) { cappedContracts(t, NewMemory()) }
func TestPostgresContractsCappedBook(t *testing.T) {
	p := timerPostgres(t, "capped_book")
	cappedContracts(t, p)
	ctx := context.Background()
	if _, e := p.pool.Exec(ctx, migrations.CappedBooks); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"mantik_capped_books", "mantik_capped_book_events"} {
		for _, sql := range []string{"DELETE FROM " + table, "TRUNCATE " + table + " CASCADE"} {
			if _, e := p.pool.Exec(ctx, sql); e == nil {
				t.Fatal("mutable history", sql)
			}
		}
	}
	reopened, e := OpenPostgres(ctx, p.pool.Config().ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	state, e := reopened.CappedBook(ctx, "book")
	if e != nil || state.Version != 1 {
		t.Fatal("restart lost book", state, e)
	}
}
