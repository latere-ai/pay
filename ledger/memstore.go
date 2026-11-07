// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package ledger

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pay/money"
)

// MemStore is the in-memory ledger: the same invariants as the Postgres one,
// with a mutex where that one has an advisory lock. It exists so a product's
// money path is testable offline, and it is driven through the same contract
// suite, which is what keeps the two honest.
type MemStore struct {
	mu      sync.Mutex
	entries []Entry
	// byRef indexes the external references already posted, which is how a
	// retried write is recognized without scanning.
	byRef map[string]int
	// reversedBy maps the reference of a reversed entry to the reference of the
	// reversal that undid it: the in-memory form of the unique index that lets
	// an entry be reversed at most once.
	reversedBy map[string]string
	now        func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore builds an empty in-memory ledger.
func NewMemStore() *MemStore {
	return &MemStore{byRef: map[string]int{}, reversedBy: map[string]string{}, now: time.Now}
}

// SetClock replaces the clock, so a test can order entries deterministically.
func (s *MemStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// memOps is the write surface with the store's lock already held by the
// caller. MemStore's own methods take the lock and delegate to it; Within hands
// it to its function, which is how a unit holds the lock for its whole length
// without deadlocking on itself.
type memOps struct{ s *MemStore }

var _ Ops = memOps{}

// Credit adds to a holder, idempotent on Ref.
func (s *MemStore) Credit(ctx context.Context, p Posting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Credit(ctx, p)
}

// Debit takes from a holder, idempotent on Ref when one is given. A debit
// carries no group: the debit of a group is what settles it, so Settle writes
// it and a caller asking for one gets ErrDebitGroup.
//
// It never refuses for want of balance: whether the work may happen is asked
// before the work, not here, and a holder who has just gone under is one who
// owes rather than one who got the work free.
func (s *MemStore) Debit(ctx context.Context, p Posting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Debit(ctx, p)
}

// Adjust corrects after the fact, in either direction. It carries no
// uniqueness constraint beyond its own reference.
func (s *MemStore) Adjust(ctx context.Context, p Posting, up bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Adjust(ctx, p, up)
}

// Transfer moves credit between two holders in one atomic step.
func (s *MemStore) Transfer(ctx context.Context, t Transfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Transfer(ctx, t)
}

// Hold reserves against a holder, refusing when there is not that much
// available and when the group is already settled.
func (s *MemStore) Hold(ctx context.Context, p Posting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Hold(ctx, p)
}

// Settle releases a group's open hold and debits what the work cost, once.
//
// It reports whether it settled: false means the group was already settled,
// which is the exactly-once guarantee doing its job rather than a failure.
func (s *MemStore) Settle(ctx context.Context, st Settlement) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Settle(ctx, st)
}

// Reverse undoes a referenced entry by the exact amount it moved, idempotent on
// the reversal's own reference. An entry already reversed under another
// reference is left alone and reported as not applied.
func (s *MemStore) Reverse(ctx context.Context, r Reversal) (Effect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memOps{s}.Reverse(ctx, r)
}

func (o memOps) Credit(_ context.Context, p Posting) error {
	if err := CheckPosting(p, true); err != nil {
		return err
	}
	return o.s.postOnce(p, KindCredit, p.Amount)
}

func (o memOps) Debit(_ context.Context, p Posting) error {
	if err := CheckPosting(p, false); err != nil {
		return err
	}
	if p.Group != "" {
		return ErrDebitGroup
	}
	return o.s.postOnce(p, KindDebit, -p.Amount)
}

func (o memOps) Adjust(_ context.Context, p Posting, up bool) error {
	if err := CheckPosting(p, false); err != nil {
		return err
	}
	amount := p.Amount
	if !up {
		amount = -amount
	}
	return o.s.postOnce(p, KindAdjust, amount)
}

func (o memOps) Transfer(_ context.Context, t Transfer) error {
	if err := CheckTransfer(t); err != nil {
		return err
	}
	s := o.s
	out := Posting{Holder: t.From, Amount: t.Amount, Reason: t.Reason, Ref: t.Ref, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
	// The reference is looked up before the funds are checked: the first
	// attempt used the funds, so its replay would otherwise be refused.
	if replay, err := s.replay(out, KindTransfer, -t.Amount); replay || err != nil {
		return err
	}
	have, err := s.available(t.From)
	if err != nil {
		return err
	}
	if have < t.Amount {
		return ErrInsufficient
	}
	// Both ids are minted before either side is posted, so an id that cannot be
	// minted moves nothing.
	outID, err := NewID()
	if err != nil {
		return err
	}
	inID, err := NewID()
	if err != nil {
		return err
	}
	s.append(outID, out, KindTransfer, -t.Amount)
	// The receiving side carries no Ref: the reference belongs to the move, and
	// a unique index must not see it twice.
	in := Posting{Holder: t.To, Amount: t.Amount, Reason: t.Reason, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
	s.append(inID, in, KindTransfer, t.Amount)
	return nil
}

func (o memOps) Hold(_ context.Context, p Posting) error {
	if err := CheckPosting(p, false); err != nil {
		return err
	}
	if strings.TrimSpace(p.Group) == "" {
		return ErrNoGroup
	}
	s := o.s
	if replay, err := s.replay(p, KindHold, -p.Amount); replay || err != nil {
		return err
	}
	for _, e := range s.entries {
		if e.Group == p.Group && e.Kind == KindDebit {
			return ErrSettled
		}
	}
	have, err := s.available(p.Holder)
	if err != nil {
		return err
	}
	if have < p.Amount {
		return ErrInsufficient
	}
	if p.Actor == "" {
		p.Actor = ActorSystem
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	s.append(id, p, KindHold, -p.Amount)
	return nil
}

func (o memOps) Settle(_ context.Context, st Settlement) (bool, error) {
	if st.Cost < 0 {
		return false, ErrNotPositive
	}
	if !st.Holder.Valid() {
		return false, ErrNoHolder
	}
	if strings.TrimSpace(st.Group) == "" {
		return false, ErrNoGroup
	}
	s := o.s
	var held money.Micro
	settled := false
	for _, e := range s.entries {
		if e.Group != st.Group {
			continue
		}
		if e.Kind == KindDebit {
			settled = true
		}
		if e.Kind.commits() {
			if e.Holder != st.Holder {
				return false, ErrWrongHolder
			}
			held -= e.Amount
		}
	}
	if settled {
		return false, nil
	}
	actor := st.Actor
	if actor == "" {
		actor = ActorSystem
	}
	// Every id is minted before any entry is posted, so an id that cannot be
	// minted settles nothing.
	var releaseID string
	var err error
	if held > 0 {
		if releaseID, err = NewID(); err != nil {
			return false, err
		}
	}
	debitID, err := NewID()
	if err != nil {
		return false, err
	}
	if held > 0 {
		s.append(releaseID, Posting{
			Holder: st.Holder, Amount: held, Reason: st.Reason, Group: st.Group, Actor: actor, Labels: st.Labels,
		}, KindRelease, held)
	}
	// The debit is written even when it is zero: it is the marker that says this
	// group settled, and without it a retry would release a second time.
	s.append(debitID, Posting{
		Holder: st.Holder, Amount: st.Cost, Reason: st.Reason, Group: st.Group, Actor: actor, Labels: st.Labels,
	}, KindDebit, -st.Cost)
	return true, nil
}

func (o memOps) Reverse(_ context.Context, r Reversal) (Effect, error) {
	if err := CheckReversal(r); err != nil {
		return Effect{}, err
	}
	s := o.s
	if i, ok := s.byRef[r.Ref]; ok {
		// The reversal's own reference exists: a replay only if that entry is
		// this very reversal.
		if s.entries[i].Kind == KindReverse && s.reversedBy[r.Of] == r.Ref {
			return Effect{}, nil
		}
		return Effect{}, ErrRefConflict
	}
	i, ok := s.byRef[r.Of]
	if !ok {
		return Effect{}, ErrNotFound
	}
	orig := s.entries[i]
	switch orig.Kind {
	case KindTransfer, KindHold, KindRelease:
		return Effect{}, ErrNotReversible
	}
	if _, done := s.reversedBy[r.Of]; done {
		return Effect{}, nil // reversed already, under another reference
	}
	before, err := s.balance(orig.Holder)
	if err != nil {
		return Effect{}, err
	}
	amount := orig.Amount
	if amount < 0 {
		amount = -amount
	}
	// The sign is the opposite of what the original did, so reversing a credit
	// takes money and reversing a debit returns it.
	delta := -orig.Amount
	after, ok := addMicro(before, delta)
	if !ok {
		return Effect{}, rangeError("balance of " + string(orig.Holder))
	}
	actor := r.Actor
	if actor == "" {
		actor = ActorSystem
	}
	id, err := NewID()
	if err != nil {
		return Effect{}, err
	}
	s.append(id, Posting{
		Holder: orig.Holder, Amount: amount, Reason: r.Reason, Ref: r.Ref, Group: orig.Group, Actor: actor, Labels: r.Labels,
	}, KindReverse, delta)
	s.reversedBy[r.Of] = r.Ref
	return Effect{Applied: true, Amount: amount, Before: before, After: after}, nil
}

// Balance is everything a holder received less everything spent. It excludes
// holds: a hold has not been spent. A balance that does not fit in int64 is an
// error wrapping money.ErrRange.
func (s *MemStore) Balance(_ context.Context, h Holder) (money.Micro, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balance(h)
}

// Available is Balance less every open hold: what a holder may still commit.
func (s *MemStore) Available(_ context.Context, h Holder) (money.Micro, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available(h)
}

// BalancesFor folds several holders in one pass. A holder with no entry is
// omitted rather than zero-valued; a caller defaults a missing key to zero.
func (s *MemStore) BalancesFor(_ context.Context, hs []Holder) (map[Holder]money.Micro, error) {
	want := make(map[Holder]bool, len(hs))
	for _, h := range hs {
		want[h] = true
	}
	out := map[Holder]money.Micro{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if !want[e.Holder] || e.Kind.commits() {
			continue
		}
		v, ok := addMicro(out[e.Holder], e.Amount)
		if !ok {
			return nil, rangeError("balance of " + string(e.Holder))
		}
		out[e.Holder] = v
	}
	return out, nil
}

// Entries lists a holder's statement, newest first.
func (s *MemStore) Entries(_ context.Context, h Holder, p Page) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range slices.Backward(s.entries) {
		if e.Holder != h {
			continue
		}
		if !p.Before.IsZero() && !e.CreatedAt.Before(p.Before) {
			continue
		}
		out = append(out, cloneEntry(e))
		if len(out) == p.limit() {
			break
		}
	}
	return out, nil
}

// EntryByRef finds the entry carrying ref, if any.
func (s *MemStore) EntryByRef(_ context.Context, ref string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byRef[ref]
	if !ok {
		return Entry{}, false, nil
	}
	return cloneEntry(s.entries[i]), true, nil
}

// NegativeHolders returns every holder in a namespace whose settled balance is
// below zero, most-owed first.
func (s *MemStore) NegativeHolders(_ context.Context, namespace string) ([]HolderBalance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sums := map[Holder]money.Micro{}
	for _, e := range s.entries {
		if e.Kind.commits() || (namespace != "" && e.Holder.Namespace() != namespace) {
			continue
		}
		v, ok := addMicro(sums[e.Holder], e.Amount)
		if !ok {
			return nil, rangeError("balance of " + string(e.Holder))
		}
		sums[e.Holder] = v
	}
	var out []HolderBalance
	for h, v := range sums {
		if v < 0 {
			out = append(out, HolderBalance{Holder: h, Balance: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Balance != out[j].Balance {
			return out[i].Balance < out[j].Balance
		}
		return out[i].Holder < out[j].Holder
	})
	return out, nil
}

// TotalOutstanding is every credit the platform holds and has not yet
// delivered: the sum over every holder in the given namespaces, or over all of
// them when none is given. It is the solvency number an operator watches
// against what the processor account holds.
func (s *MemStore) TotalOutstanding(_ context.Context, namespaces ...string) (money.Micro, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total money.Micro
	for _, e := range s.entries {
		if e.Kind.commits() {
			continue
		}
		if len(namespaces) > 0 && !slices.Contains(namespaces, e.Holder.Namespace()) {
			continue
		}
		var ok bool
		if total, ok = addMicro(total, e.Amount); !ok {
			return 0, rangeError("total outstanding")
		}
	}
	return total, nil
}

// Within runs fn as one unit: if it returns an error or panics, every write it
// made is discarded.
//
// The in-memory store holds its lock for the whole unit and hands fn a view of
// the writes that does not take it again. That is what Postgres gives a
// transaction: no other writer interleaves, no reader sees the unit's entries
// before it commits, and a rollback restores exactly what the unit changed.
// The cost is that fn must write through the Ops it is given: calling the
// store itself from inside fn waits on a lock the unit holds.
//
// Rollback is by snapshot, which is affordable because the store is for tests
// and small deployments. The shape matters more than the mechanism: a caller
// writes the same code against either implementation, and the contract suite
// asserts that a failed unit moves nothing in both.
func (s *MemStore) Within(ctx context.Context, fn func(context.Context, Ops) error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := slices.Clone(s.entries)
	refs := maps.Clone(s.byRef)
	reversed := maps.Clone(s.reversedBy)
	committed := false
	defer func() {
		if !committed {
			s.entries, s.byRef, s.reversedBy = entries, refs, reversed
		}
	}()
	if err = fn(ctx, memOps{s}); err != nil {
		return err
	}
	committed = true
	return nil
}

// balance folds a holder's settled balance. The caller holds the lock.
func (s *MemStore) balance(h Holder) (money.Micro, error) {
	var v money.Micro
	for _, e := range s.entries {
		if e.Holder == h && !e.Kind.commits() {
			var ok bool
			if v, ok = addMicro(v, e.Amount); !ok {
				return 0, rangeError("balance of " + string(h))
			}
		}
	}
	return v, nil
}

// available folds a holder's balance less its open holds. The caller holds the
// lock.
func (s *MemStore) available(h Holder) (money.Micro, error) {
	var v money.Micro
	for _, e := range s.entries {
		if e.Holder == h {
			var ok bool
			if v, ok = addMicro(v, e.Amount); !ok {
				return 0, rangeError("available balance of " + string(h))
			}
		}
	}
	return v, nil
}

// addMicro adds two amounts and reports false when the sum does not fit.
func addMicro(a, b money.Micro) (money.Micro, bool) {
	c := a + b
	if (b > 0 && c < a) || (b < 0 && c > a) {
		return 0, false
	}
	return c, true
}

// rangeError reports a figure that does not fit in int64, wrapping the money
// package's sentinel so one errors.Is check covers every store.
func rangeError(what string) error {
	return fmt.Errorf("ledger: %s does not fit in int64: %w", what, money.ErrRange)
}

// replay reports whether p's reference is already recorded. A recorded
// reference for the same write is a replay (true, nil); for any other write it
// is ErrRefConflict. The caller holds the lock.
func (s *MemStore) replay(p Posting, kind Kind, amount money.Micro) (bool, error) {
	if p.Ref == "" {
		return false, nil
	}
	i, ok := s.byRef[p.Ref]
	if !ok {
		return false, nil
	}
	want := Entry{Holder: p.Holder, Kind: kind, Amount: amount, Group: p.Group}
	return true, CheckReplay(s.entries[i], want)
}

// postOnce appends one entry unless its reference is already recorded. The
// caller holds the lock and has already validated the posting.
func (s *MemStore) postOnce(p Posting, kind Kind, amount money.Micro) error {
	if replay, err := s.replay(p, kind, amount); replay || err != nil {
		return err
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	s.append(id, p, kind, amount)
	return nil
}

// append adds one entry under an id minted by the caller, which is what lets a
// write of several entries mint them all before changing anything. The caller
// holds the lock.
func (s *MemStore) append(id string, p Posting, kind Kind, amount money.Micro) {
	e := Entry{
		ID: id, Holder: p.Holder, Amount: amount, Kind: kind, Reason: p.Reason,
		Ref: p.Ref, Group: p.Group, Actor: p.Actor, CreatedAt: s.now().UTC(),
	}
	if len(p.Labels) > 0 {
		e.Labels = maps.Clone(p.Labels)
	}
	if e.Actor == "" {
		e.Actor = ActorSystem
	}
	s.entries = append(s.entries, e)
	if p.Ref != "" {
		s.byRef[p.Ref] = len(s.entries) - 1
	}
}

// cloneEntry hands out a copy, so a caller cannot mutate the ledger by writing
// through a returned entry's label map.
func cloneEntry(e Entry) Entry {
	if e.Labels != nil {
		e.Labels = maps.Clone(e.Labels)
	}
	return e
}
