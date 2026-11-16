package storage

import (
	"context"
	"encoding/json"
	"errors"

	"git.subcult.tv/subculture-collective/mantik/internal/account"
	"github.com/jackc/pgx/v5"
)

// ErrCappedReadCapacity means a read cannot return a complete bounded prefix.
// Callers must report unavailable, never truncate or synthesize a partial book.
var ErrCappedReadCapacity = errors.New("capped book read exceeds byte capacity")

// BoundedCappedBookStore reads a detached complete current state within a byte budget.
// It is separate from the command interface and never appends or compacts events.
type BoundedCappedBookStore interface {
	CappedBookBounded(context.Context, string, int) (account.CappedState, error)
}

func cappedReadConfig(raw []byte, max int) (account.CappedState, error) {
	if max < 1 || len(raw) == 0 || len(raw) > max {
		return account.CappedState{}, ErrCappedReadCapacity
	}
	var c account.CappedConfig
	if e := json.Unmarshal(raw, &c); e != nil {
		return account.CappedState{}, e
	}
	return account.NewCappedState(c)
}
func appendCappedRead(s *account.CappedState, raw []byte, remaining *int) error {
	// Budget includes per-event JSON delimiters and version field overhead.
	*remaining -= len(raw) + 2
	if len(raw) == 0 || *remaining < 0 {
		return ErrCappedReadCapacity
	}
	var c account.BookCommand
	if e := json.Unmarshal(raw, &c); e != nil {
		return e
	}
	s.Events = append(s.Events, c)
	s.Version += account.CappedCommandCount(c)
	return nil
}

// CappedBookBounded snapshots under the existing memory-store lock. Individual
// event encodings are checked before copying; no whole-book encoding is needed.
func (m *Memory) CappedBookBounded(ctx context.Context, id string, max int) (account.CappedState, error) {
	if e := ctx.Err(); e != nil {
		return account.CappedState{}, e
	}
	if max < 1 {
		return account.CappedState{}, ErrCappedReadCapacity
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	original, ok := m.cappedBooks[id]
	if !ok {
		return account.CappedState{}, account.ErrBookNotFound
	}
	raw, e := json.Marshal(original.Config)
	if e != nil {
		return account.CappedState{}, e
	}
	out, e := cappedReadConfig(raw, max)
	if e != nil {
		return account.CappedState{}, e
	}
	remaining := max - len(raw) - 128
	if remaining < 0 {
		return account.CappedState{}, ErrCappedReadCapacity
	}
	for _, c := range original.Events {
		if e = ctx.Err(); e != nil {
			return account.CappedState{}, e
		}
		raw, e = json.Marshal(c)
		if e != nil {
			return account.CappedState{}, e
		}
		if e = appendCappedRead(&out, raw, &remaining); e != nil {
			return account.CappedState{}, e
		}
	}
	if e = account.ValidateCappedState(out); e != nil {
		return account.CappedState{}, e
	}
	if out.Version != original.Version {
		return account.CappedState{}, account.ErrBookConflict
	}
	return out, nil
}

// CappedBookBounded reads a verified current head, or a complete legacy stream
// when no head exists. A read-only repeatable snapshot prevents a concurrent
// legacy-to-head transition from returning stale legacy state. SQL suppresses
// oversized payloads before they enter pgx buffers; no partial state is returned.
func (p *Postgres) CappedBookBounded(ctx context.Context, id string, max int) (account.CappedState, error) {
	if max < 1 {
		return account.CappedState{}, ErrCappedReadCapacity
	}
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return account.CappedState{}, e
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var raw []byte
	var version int64
	var digest string
	e = tx.QueryRow(ctx, `SELECT CASE WHEN octet_length(h.state::text)<=$2 THEN h.state ELSE NULL END,h.version,j.state_sha256
 FROM mantik_capped_book_heads h JOIN mantik_capped_book_history j
 ON j.book_id=h.book_id AND j.sequence=h.version AND j.state_sha256=h.state_sha256
 WHERE h.book_id=$1`, id, max).Scan(&raw, &version, &digest)
	if e == nil {
		if len(raw) == 0 {
			return account.CappedState{}, ErrCappedReadCapacity
		}
		var out account.CappedState
		if cappedDigest(raw) != digest || json.Unmarshal(raw, &out) != nil || out.Config.ID != id || out.Version != version {
			return account.CappedState{}, account.ErrBookConflict
		}
		if e = account.ValidateCappedState(out); e != nil {
			return account.CappedState{}, e
		}
		return out, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return account.CappedState{}, e
	}
	// A corrupt/missing custody join must not downgrade to legacy state.
	var hasHead bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mantik_capped_book_heads WHERE book_id=$1)`, id).Scan(&hasHead); e != nil {
		return account.CappedState{}, e
	}
	if hasHead {
		return account.CappedState{}, account.ErrBookConflict
	}
	e = tx.QueryRow(ctx, `SELECT CASE WHEN octet_length(config::text)<=$2 THEN config ELSE NULL END FROM mantik_capped_books WHERE id=$1`, id, max).Scan(&raw)
	if errors.Is(e, pgx.ErrNoRows) {
		return account.CappedState{}, account.ErrBookNotFound
	}
	if e != nil {
		return account.CappedState{}, e
	}
	out, e := cappedReadConfig(raw, max)
	if e != nil {
		return account.CappedState{}, e
	}
	remaining := max - len(raw) - 128
	if remaining < 0 {
		return account.CappedState{}, ErrCappedReadCapacity
	}
	rows, e := tx.Query(ctx, `SELECT CASE WHEN sum(octet_length(payload::text)+2) OVER (ORDER BY sequence ROWS UNBOUNDED PRECEDING)<=$2 THEN payload ELSE NULL END FROM mantik_capped_book_events WHERE book_id=$1 ORDER BY sequence LIMIT 4097`, id, remaining)
	if e != nil {
		return account.CappedState{}, e
	}
	defer rows.Close()
	for rows.Next() {
		if e = rows.Scan(&raw); e != nil {
			return account.CappedState{}, e
		}
		if e = appendCappedRead(&out, raw, &remaining); e != nil {
			return account.CappedState{}, e
		}
	}
	if e = rows.Err(); e != nil {
		return account.CappedState{}, e
	}
	if e = account.ValidateCappedState(out); e != nil {
		return account.CappedState{}, e
	}
	return out, nil
}
