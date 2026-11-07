// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pgledger

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
		_, err := o.put(ctx, p, ledger.KindCredit, p.Amount)
		return err
	})
}

// Debit takes from a holder, idempotent on Ref when one is given. It never
// refuses for want of balance: whether the work may happen is asked before the
// work, not here. It carries no group, because a group's debit is what settles
// it and only Settle writes that one.
func (o *ops) Debit(ctx context.Context, p ledger.Posting) error {
	if err := ledger.CheckPosting(p, false); err != nil {
		return err
	}
	if p.Group != "" {
		return ledger.ErrDebitGroup
	}
	return o.atomically(ctx, func(o *ops) error {
		_, err := o.put(ctx, p, ledger.KindDebit, -p.Amount)
		return err
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
		_, err := o.put(ctx, p, ledger.KindAdjust, amount)
		return err
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
		out := ledger.Posting{Holder: t.From, Amount: t.Amount, Reason: t.Reason, Ref: t.Ref, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
		// The reference belongs to the move, not to either side, so the answer
		// to "has this already happened" is asked once, before the funds are
		// checked (the first attempt used them) and before anything is written.
		// Inferring it afterwards from which insert was a no-op is how the
		// receiving side ends up posted twice.
		if replay, err := o.replayed(ctx, out, ledger.KindTransfer, -t.Amount); replay || err != nil {
			return err
		}
		have, err := scanSum(o.q.QueryRow(ctx, availableSQL, string(t.From)))
		if err != nil {
			return err
		}
		if have < t.Amount {
			return ledger.ErrInsufficient
		}
		inserted, err := o.put(ctx, out, ledger.KindTransfer, -t.Amount)
		if err != nil || !inserted {
			// A move that raced in with the same reference and the same
			// payload already posted both sides.
			return err
		}
		in := ledger.Posting{Holder: t.To, Amount: t.Amount, Reason: t.Reason, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
		_, err = o.put(ctx, in, ledger.KindTransfer, t.Amount)
		return err
	})
}

// Hold reserves against a holder, refusing when there is not that much
// available or when the group is already settled.
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
		// A replay is answered before anything else: the first attempt used the
		// funds and may since have settled the group.
		if replay, err := o.replayed(ctx, p, ledger.KindHold, -p.Amount); replay || err != nil {
			return err
		}
		// Settlement and a hold on its group serialize, so a hold cannot slip in
		// beside a debit that has already read the group's holds.
		if err := o.lockGroup(ctx, p.Group); err != nil {
			return err
		}
		var settled bool
		if err := o.q.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE grp = $1 AND kind = 'debit')`, p.Group).Scan(&settled); err != nil {
			return err
		}
		if settled {
			return ledger.ErrSettled
		}
		have, err := scanSum(o.q.QueryRow(ctx, availableSQL, string(p.Holder)))
		if err != nil {
			return err
		}
		if have < p.Amount {
			return ledger.ErrInsufficient
		}
		if p.Actor == "" {
			p.Actor = ledger.ActorSystem
		}
		_, err = o.put(ctx, p, ledger.KindHold, -p.Amount)
		return err
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
		if err := o.lockGroup(ctx, st.Group); err != nil {
			return err
		}
		// The hold belongs to whoever took it; releasing it onto another holder
		// would move credit between them.
		var foreign bool
		if err := o.q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM ledger_entries
				WHERE grp = $1 AND kind IN ('hold','release') AND holder <> $2)`,
			st.Group, string(st.Holder)).Scan(&foreign); err != nil {
			return err
		}
		if foreign {
			return ledger.ErrWrongHolder
		}
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
		held, err := scanSum(o.q.QueryRow(ctx, heldSQL, st.Group))
		if err != nil {
			return err
		}
		if held <= 0 {
			return nil
		}
		_, err = o.put(ctx, ledger.Posting{
			Holder: st.Holder, Amount: held, Reason: st.Reason, Group: st.Group, Actor: actor, Labels: st.Labels,
		}, ledger.KindRelease, held)
		return err
	})
	if err != nil {
		return false, err
	}
	return settled, nil
}

// Reverse undoes a referenced entry by the exact amount it moved.
//
// The entry is reversed at most once, and the database decides: the insert
// names the entry it reverses, and a unique index on that column refuses a
// second reversal whatever its own reference. The insert's row count, not a
// read taken beforehand, says whether this call is the one that applied.
func (o *ops) Reverse(ctx context.Context, r ledger.Reversal) (ledger.Effect, error) {
	if err := ledger.CheckReversal(r); err != nil {
		return ledger.Effect{}, err
	}
	actor := r.Actor
	if actor == "" {
		actor = ledger.ActorSystem
	}
	var eff ledger.Effect
	err := o.atomically(ctx, func(o *ops) error {
		stored, found, err := o.byRef(ctx, r.Ref)
		if err != nil {
			return err
		}
		if found {
			return reversalReplay(stored, r)
		}
		orig, found, err := o.byRef(ctx, r.Of)
		if err != nil {
			return err
		}
		if !found {
			return ledger.ErrNotFound
		}
		switch orig.Kind {
		case ledger.KindTransfer, ledger.KindHold, ledger.KindRelease:
			return ledger.ErrNotReversible
		}
		if err := o.lock(ctx, orig.Holder); err != nil {
			return err
		}
		before, err := scanSum(o.q.QueryRow(ctx, balanceSQL, string(orig.Holder)))
		if err != nil {
			return err
		}
		magnitude := orig.Amount
		if magnitude < 0 {
			magnitude = -magnitude
		}
		p := ledger.Posting{Holder: orig.Holder, Amount: magnitude, Reason: r.Reason, Ref: r.Ref, Group: orig.Group, Actor: actor, Labels: r.Labels}
		// The sign is the opposite of what the original did, so reversing a
		// credit takes money and reversing a debit returns it.
		inserted, err := o.insert(ctx, p, ledger.KindReverse, -orig.Amount, r.Of, "")
		if err != nil {
			return err
		}
		if !inserted {
			// Either index refused it. If our own reference is now recorded
			// the same reversal beat us; otherwise the entry was reversed under
			// another reference. Neither applied here.
			stored, found, err := o.byRef(ctx, r.Ref)
			if err != nil {
				return err
			}
			if found {
				return reversalReplay(stored, r)
			}
			return nil
		}
		after, err := scanSum(o.q.QueryRow(ctx, balanceSQL, string(orig.Holder)))
		if err != nil {
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

// reversalReplay answers a reversal whose own reference is already recorded:
// nil when the stored entry is this very reversal, a conflict when the
// reference belongs to anything else.
func reversalReplay(stored row, r ledger.Reversal) error {
	if stored.Kind == ledger.KindReverse && stored.reverses == r.Of {
		return nil
	}
	return ledger.ErrRefConflict
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

// lockGroup takes a transaction-scoped advisory lock on a unit of work, so a
// hold and the settlement of its group do not interleave.
func (o *ops) lockGroup(ctx context.Context, group string) error {
	_, err := o.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "settle-group:"+group)
	return err
}

// row is a stored entry as the idempotency checks need it.
type row struct {
	ledger.Entry
	// reverses is the reference of the entry a reversal undoes, empty on any
	// other kind and on reversals written before the column existed.
	reverses string
}

// byRef reads the entry carrying ref.
func (o *ops) byRef(ctx context.Context, ref string) (row, bool, error) {
	var (
		r               row
		holder, kind    string
		group, reverses *string
	)
	err := o.q.QueryRow(ctx,
		`SELECT holder, amount, kind, grp, reverses FROM ledger_entries WHERE ref = $1`, ref).
		Scan(&holder, &r.Amount, &kind, &group, &reverses)
	if errors.Is(err, pgx.ErrNoRows) {
		return row{}, false, nil
	}
	if err != nil {
		return row{}, false, err
	}
	r.Holder, r.Kind = ledger.Holder(holder), ledger.Kind(kind)
	if group != nil {
		r.Group = *group
	}
	if reverses != nil {
		r.reverses = *reverses
	}
	return r, true, nil
}

// replayed reports whether p's reference is already recorded. For the same
// write it is (true, nil); for any other it is (true, ErrRefConflict).
func (o *ops) replayed(ctx context.Context, p ledger.Posting, kind ledger.Kind, amount money.Micro) (bool, error) {
	if p.Ref == "" {
		return false, nil
	}
	stored, found, err := o.byRef(ctx, p.Ref)
	if err != nil || !found {
		return false, err
	}
	want := ledger.Entry{Holder: p.Holder, Kind: kind, Amount: amount, Group: p.Group}
	return true, ledger.CheckReplay(stored.Entry, want)
}

// put writes one entry and reports whether this call inserted it. A reference
// that is already recorded inserts nothing: the answer is nil when the stored
// entry is the same write and ErrRefConflict when it is another. The unique
// index decides, so two writers racing on one reference cannot both insert.
func (o *ops) put(ctx context.Context, p ledger.Posting, kind ledger.Kind, amount money.Micro) (bool, error) {
	inserted, err := o.insert(ctx, p, kind, amount, "", "(ref) WHERE ref IS NOT NULL")
	if err != nil || inserted {
		return inserted, err
	}
	replay, err := o.replayed(ctx, p, kind, amount)
	if err != nil {
		return false, err
	}
	if !replay {
		return false, fmt.Errorf("pgledger: insert of %s skipped with no recorded reference", kind)
	}
	return false, nil
}

// insert writes one entry, doing nothing when a unique index refuses it. target
// narrows the conflict to one index; empty means any, which Reverse needs
// because it is bound by two: its own reference and the entry it reverses.
func (o *ops) insert(ctx context.Context, p ledger.Posting, kind ledger.Kind, amount money.Micro, reverses, target string) (bool, error) {
	id, err := ledger.NewID()
	if err != nil {
		return false, err
	}
	labels, err := marshalLabels(p.Labels)
	if err != nil {
		return false, err
	}
	actor := p.Actor
	if actor == "" {
		actor = ledger.ActorSystem
	}
	var ref, group, rev *string
	if p.Ref != "" {
		ref = &p.Ref
	}
	if p.Group != "" {
		group = &p.Group
	}
	if reverses != "" {
		rev = &reverses
	}
	tag, err := o.q.Exec(ctx, `
		INSERT INTO ledger_entries (id, holder, amount, kind, reason, ref, grp, actor, labels, created_at, reverses)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT `+target+` DO NOTHING`,
		id, string(p.Holder), amount, string(kind), string(p.Reason), ref, group, actor, labels, o.clock().UTC(), rev)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// scanSum reads a SUM of amounts. Postgres sums bigint into numeric, so the
// total can exceed int64 even when every row fits; scanning it into an int64
// would fail with a driver error that says nothing about the ledger. Reading
// the numeric and converting it gives the error the memory store gives: one
// wrapping money.ErrRange.
func scanSum(row pgx.Row) (money.Micro, error) {
	var n pgtype.Numeric
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return microOf(n)
}

// microOf converts a summed numeric to micro-USD, or reports money.ErrRange.
func microOf(n pgtype.Numeric) (money.Micro, error) {
	v, err := n.Int64Value()
	if err != nil {
		return 0, fmt.Errorf("pgledger: a sum of entries does not fit in int64: %w", money.ErrRange)
	}
	return money.Micro(v.Int64), nil
}

func marshalLabels(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return nil, fmt.Errorf("pgledger: marshal labels: %w", err)
	}
	return b, nil
}
