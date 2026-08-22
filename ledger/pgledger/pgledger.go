package pgledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/money"
)

// Store is the Postgres ledger.
type Store struct {
	pool  *pgxpool.Pool
	clock func() time.Time
}

var _ ledger.Store = (*Store)(nil)

// New builds a store over a pool. The caller owns the pool's lifetime.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, clock: time.Now}
}

// SetClock replaces the clock, so a test can order entries deterministically.
func (s *Store) SetClock(now func() time.Time) { s.clock = now }

// querier is the surface both a pool and a transaction satisfy, so every
// statement in this file is written once and runs in either.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pgconnCommandTag is the result of an Exec, narrowed to what this package
// reads so the interface above does not drag in a pgconn import at every seam.
type pgconnCommandTag interface{ RowsAffected() int64 }

// txQuerier adapts a transaction to querier.
type txQuerier struct{ tx pgx.Tx }

func (q txQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error) {
	return q.tx.Exec(ctx, sql, args...)
}
func (q txQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.tx.QueryRow(ctx, sql, args...)
}

// ops implements ledger.Ops over whichever querier it was built with.
//
// Every write runs inside a transaction. When pool is nil the transaction is
// the caller's, handed in through Bind; when it is set, each operation opens
// one of its own. Nothing here ever writes on a bare pool: a hold reads a
// balance and inserts against it, and a read on one connection with an insert
// on another is exactly the race the advisory lock exists to prevent.
type ops struct {
	q     querier
	clock func() time.Time
	pool  *pgxpool.Pool
}

// Bind returns write operations that run inside a transaction the caller
// already owns.
//
// This is the method that cannot live on ledger.Store, and it is why this is a
// separate package. A product inserting the row that a hold guards passes its
// own pgx.Tx here, so the hold and the row commit or roll back together and a
// unit of work can never be recorded without its money.
func (s *Store) Bind(tx pgx.Tx) ledger.Ops {
	return &ops{q: txQuerier{tx}, clock: s.clock}
}

// opsOnPool is the store's own write surface: it opens a transaction per
// operation, since it has no caller's transaction to join.
func (s *Store) opsOnPool() *ops {
	return &ops{clock: s.clock, pool: s.pool}
}

// Within runs fn as one transaction. Every write fn performs on the Ops it is
// handed commits or rolls back together.
func (s *Store) Within(ctx context.Context, fn func(context.Context, ledger.Ops) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(ctx, s.Bind(tx))
	})
}

// The store's writes delegate to an ops over the pool.

func (s *Store) Credit(ctx context.Context, p ledger.Posting) error {
	return s.opsOnPool().Credit(ctx, p)
}
func (s *Store) Debit(ctx context.Context, p ledger.Posting) error {
	return s.opsOnPool().Debit(ctx, p)
}
func (s *Store) Adjust(ctx context.Context, p ledger.Posting, up bool) error {
	return s.opsOnPool().Adjust(ctx, p, up)
}
func (s *Store) Transfer(ctx context.Context, t ledger.Transfer) error {
	return s.opsOnPool().Transfer(ctx, t)
}
func (s *Store) Hold(ctx context.Context, p ledger.Posting) error {
	return s.opsOnPool().Hold(ctx, p)
}
func (s *Store) Settle(ctx context.Context, st ledger.Settlement) (bool, error) {
	return s.opsOnPool().Settle(ctx, st)
}
func (s *Store) Reverse(ctx context.Context, r ledger.Reversal) (ledger.Effect, error) {
	return s.opsOnPool().Reverse(ctx, r)
}

// Credit adds to a holder, idempotent on Ref.
func (o *ops) Credit(ctx context.Context, p ledger.Posting) error {
	if err := ledger.CheckPosting(p, true); err != nil {
		return err
	}
	return o.atomically(ctx, func(o *ops) error {
		return o.insert(ctx, p, ledger.KindCredit, p.Amount)
	})
}

// Debit takes from a holder, idempotent on Ref when one is given. It never
// refuses for want of balance: whether the work may happen is asked before the
// work, not here.
func (o *ops) Debit(ctx context.Context, p ledger.Posting) error {
	if err := ledger.CheckPosting(p, false); err != nil {
		return err
	}
	return o.atomically(ctx, func(o *ops) error {
		return o.insert(ctx, p, ledger.KindDebit, -p.Amount)
	})
}

// Adjust corrects after the fact, in either direction.
func (o *ops) Adjust(ctx context.Context, p ledger.Posting, up bool) error {
	if err := ledger.CheckPosting(p, false); err != nil {
		return err
	}
	amount := p.Amount
	if !up {
		amount = -amount
	}
	return o.atomically(ctx, func(o *ops) error {
		return o.insert(ctx, p, ledger.KindAdjust, amount)
	})
}

// availableSQL folds a holder's balance less its open holds. Every kind counts,
// which is what makes it "available" rather than "settled".
const availableSQL = `SELECT COALESCE(SUM(amount), 0) FROM ledger_entries WHERE holder = $1`

// balanceSQL folds a holder's settled balance, excluding holds and releases: a
// hold has not been spent.
const balanceSQL = `
	SELECT COALESCE(SUM(amount), 0) FROM ledger_entries
	WHERE holder = $1 AND kind NOT IN ('hold','release')`

// Transfer moves credit between two holders inside one transaction.
func (o *ops) Transfer(ctx context.Context, t ledger.Transfer) error {
	if err := ledger.CheckTransfer(t); err != nil {
		return err
	}
	return o.atomically(ctx, func(o *ops) error {
		// Lock the source before reading it, for the same reason Hold does: a
		// check on one connection and an insert on another lets two concurrent
		// transfers each read the other's uncommitted debit as absent.
		if err := o.lock(ctx, t.From); err != nil {
			return err
		}
		var have money.Micro
		if err := o.q.QueryRow(ctx, availableSQL, string(t.From)).Scan(&have); err != nil {
			return err
		}
		if have < t.Amount {
			return ledger.ErrInsufficient
		}
		// The reference belongs to the move, not to either side, so the answer
		// to "has this already happened" is asked once, before anything is
		// written. Inferring it afterwards from which insert was a no-op is
		// how the receiving side ends up posted twice.
		if t.Ref != "" {
			var posted bool
			if err := o.q.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE ref = $1)`, t.Ref).Scan(&posted); err != nil {
				return err
			}
			if posted {
				return nil
			}
		}
		out := ledger.Posting{Holder: t.From, Amount: t.Amount, Reason: t.Reason, Ref: t.Ref, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
		if err := o.insert(ctx, out, ledger.KindTransfer, -t.Amount); err != nil {
			return err
		}
		in := ledger.Posting{Holder: t.To, Amount: t.Amount, Reason: t.Reason, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
		return o.insert(ctx, in, ledger.KindTransfer, t.Amount)
	})
}

// Hold reserves against a holder, refusing when there is not that much
// available.
//
// The read and the write are the same table, so this cannot be a check on one
// connection and an insert on another: two concurrent admissions would each
// read the other's uncommitted hold as absent and both be let through. The
// advisory lock makes the read authoritative for the rest of the transaction.
func (o *ops) Hold(ctx context.Context, p ledger.Posting) error {
	if err := ledger.CheckPosting(p, false); err != nil {
		return err
	}
	if strings.TrimSpace(p.Group) == "" {
		return ledger.ErrNoGroup
	}
	return o.atomically(ctx, func(o *ops) error {
		if err := o.lock(ctx, p.Holder); err != nil {
			return err
		}
		var have money.Micro
		if err := o.q.QueryRow(ctx, availableSQL, string(p.Holder)).Scan(&have); err != nil {
			return err
		}
		if have < p.Amount {
			return ledger.ErrInsufficient
		}
		if p.Actor == "" {
			p.Actor = ledger.ActorSystem
		}
		return o.insert(ctx, p, ledger.KindHold, -p.Amount)
	})
}

// heldSQL sums a group's open hold: everything reserved less everything already
// released, so settlement returns exactly what is outstanding rather than an
// amount the caller has to remember.
const heldSQL = `
	SELECT COALESCE(SUM(amount), 0) * -1 FROM ledger_entries
	WHERE grp = $1 AND kind IN ('hold','release')`

// Settle releases a group's hold and debits what the work cost, once.
//
// The debit goes in first, and its conflict is what makes this exactly-once: if
// a concurrent or retried settlement already wrote one, nothing is inserted and
// no hold is released twice. Releasing first would let a retry release again
// before discovering the debit.
func (o *ops) Settle(ctx context.Context, st ledger.Settlement) (bool, error) {
	if st.Cost < 0 {
		return false, ledger.ErrNotPositive
	}
	if !st.Holder.Valid() {
		return false, ledger.ErrNoHolder
	}
	if strings.TrimSpace(st.Group) == "" {
		return false, ledger.ErrNoGroup
	}
	actor := st.Actor
	if actor == "" {
		actor = ledger.ActorSystem
	}
	var settled bool
	err := o.atomically(ctx, func(o *ops) error {
		id, err := ledger.NewID()
		if err != nil {
			return err
		}
		labels, err := marshalLabels(st.Labels)
		if err != nil {
			return err
		}
		tag, err := o.q.Exec(ctx, `
			INSERT INTO ledger_entries (id, holder, amount, kind, reason, grp, actor, labels, created_at)
			VALUES ($1,$2,$3,'debit',$4,$5,$6,$7,$8)
			ON CONFLICT (grp) WHERE kind = 'debit' AND grp IS NOT NULL DO NOTHING`,
			id, string(st.Holder), -st.Cost, string(st.Reason), st.Group, actor, labels, o.clock().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already settled
		}
		settled = true
		var held money.Micro
		if err := o.q.QueryRow(ctx, heldSQL, st.Group).Scan(&held); err != nil {
			return err
		}
		if held <= 0 {
			return nil
		}
		return o.insert(ctx, ledger.Posting{
			Holder: st.Holder, Amount: held, Reason: st.Reason, Group: st.Group, Actor: actor, Labels: st.Labels,
		}, ledger.KindRelease, held)
	})
	if err != nil {
		return false, err
	}
	return settled, nil
}

// Reverse undoes a referenced entry by the exact amount it moved.
func (o *ops) Reverse(ctx context.Context, r ledger.Reversal) (ledger.Effect, error) {
	if strings.TrimSpace(r.Of) == "" || strings.TrimSpace(r.Ref) == "" {
		return ledger.Effect{}, ledger.ErrNoRef
	}
	actor := r.Actor
	if actor == "" {
		actor = ledger.ActorSystem
	}
	var eff ledger.Effect
	err := o.atomically(ctx, func(o *ops) error {
		var dup bool
		if err := o.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE ref = $1)`, r.Ref).Scan(&dup); err != nil {
			return err
		}
		if dup {
			return nil // this reversal already applied
		}
		var holder string
		var amount money.Micro
		var group *string
		err := o.q.QueryRow(ctx,
			`SELECT holder, amount, grp FROM ledger_entries WHERE ref = $1`, r.Of).Scan(&holder, &amount, &group)
		if errors.Is(err, pgx.ErrNoRows) {
			return ledger.ErrNotFound
		}
		if err != nil {
			return err
		}
		h := ledger.Holder(holder)
		if err := o.lock(ctx, h); err != nil {
			return err
		}
		var before money.Micro
		if err := o.q.QueryRow(ctx, balanceSQL, holder).Scan(&before); err != nil {
			return err
		}
		magnitude := amount
		if magnitude < 0 {
			magnitude = -magnitude
		}
		p := ledger.Posting{Holder: h, Amount: magnitude, Reason: r.Reason, Ref: r.Ref, Actor: actor, Labels: r.Labels}
		if group != nil {
			p.Group = *group
		}
		// The sign is the opposite of what the original did, so reversing a
		// credit takes money and reversing a debit returns it.
		if err := o.insert(ctx, p, ledger.KindReverse, -amount); err != nil {
			return err
		}
		var after money.Micro
		if err := o.q.QueryRow(ctx, balanceSQL, holder).Scan(&after); err != nil {
			return err
		}
		eff = ledger.Effect{Applied: true, Amount: magnitude, Before: before, After: after}
		return nil
	})
	if err != nil {
		return ledger.Effect{}, err
	}
	return eff, nil
}

// atomically runs fn in a transaction, opening one when this ops does not
// already run inside the caller's.
func (o *ops) atomically(ctx context.Context, fn func(*ops) error) error {
	if o.pool == nil {
		return fn(o) // already inside the caller's transaction
	}
	return pgx.BeginFunc(ctx, o.pool, func(tx pgx.Tx) error {
		return fn(&ops{q: txQuerier{tx}, clock: o.clock})
	})
}

// lock takes a transaction-scoped advisory lock on a holder, so a read of that
// holder's balance stays authoritative until the transaction ends.
func (o *ops) lock(ctx context.Context, h ledger.Holder) error {
	_, err := o.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, string(h))
	return err
}

// insert writes one entry, doing nothing when its reference was already posted.
func (o *ops) insert(ctx context.Context, p ledger.Posting, kind ledger.Kind, amount money.Micro) error {
	id, err := ledger.NewID()
	if err != nil {
		return err
	}
	labels, err := marshalLabels(p.Labels)
	if err != nil {
		return err
	}
	actor := p.Actor
	if actor == "" {
		actor = ledger.ActorSystem
	}
	var ref, group *string
	if p.Ref != "" {
		ref = &p.Ref
	}
	if p.Group != "" {
		group = &p.Group
	}
	_, err = o.q.Exec(ctx, `
		INSERT INTO ledger_entries (id, holder, amount, kind, reason, ref, grp, actor, labels, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (ref) WHERE ref IS NOT NULL DO NOTHING`,
		id, string(p.Holder), amount, string(kind), string(p.Reason), ref, group, actor, labels, o.clock().UTC())
	return err
}

func marshalLabels(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("pgledger: marshal labels: %w", err)
	}
	return b, nil
}
