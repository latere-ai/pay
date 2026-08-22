package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// retried write is refused without scanning.
	byRef map[string]int
	now   func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore builds an empty in-memory ledger.
func NewMemStore() *MemStore {
	return &MemStore{byRef: map[string]int{}, now: time.Now}
}

// SetClock replaces the clock, so a test can order entries deterministically.
func (s *MemStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Credit adds to a holder, idempotent on Ref.
func (s *MemStore) Credit(_ context.Context, p Posting) error {
	if err := checkPosting(p, true); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.post(p, KindCredit, p.Amount)
}

// Debit takes from a holder, idempotent on Ref when one is given.
//
// It never refuses for want of balance: whether the work may happen is asked
// before the work, not here, and a holder who has just gone under is one who
// owes rather than one who got the work free.
func (s *MemStore) Debit(_ context.Context, p Posting) error {
	if err := checkPosting(p, false); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.post(p, KindDebit, -p.Amount)
}

// Adjust corrects after the fact, in either direction. It carries no
// uniqueness constraint beyond its own reference.
func (s *MemStore) Adjust(_ context.Context, p Posting, up bool) error {
	if err := checkPosting(p, false); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	amount := p.Amount
	if !up {
		amount = -amount
	}
	return s.post(p, KindAdjust, amount)
}

// Transfer moves credit between two holders in one atomic step.
func (s *MemStore) Transfer(_ context.Context, t Transfer) error {
	if err := checkTransfer(t); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available(t.From) < t.Amount {
		return ErrInsufficient
	}
	if t.Ref != "" {
		if _, dup := s.byRef[t.Ref]; dup {
			return nil
		}
	}
	out := Posting{Holder: t.From, Amount: t.Amount, Reason: t.Reason, Ref: t.Ref, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
	if err := s.post(out, KindTransfer, -t.Amount); err != nil {
		return err
	}
	// The receiving side carries no Ref: the reference belongs to the move, and
	// a unique index must not see it twice.
	in := Posting{Holder: t.To, Amount: t.Amount, Reason: t.Reason, Group: t.Group, Actor: t.Actor, Labels: t.Labels}
	return s.post(in, KindTransfer, t.Amount)
}

// Hold reserves against a holder, refusing when there is not that much
// available.
func (s *MemStore) Hold(_ context.Context, p Posting) error {
	if err := checkPosting(p, false); err != nil {
		return err
	}
	if strings.TrimSpace(p.Group) == "" {
		return ErrNoGroup
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available(p.Holder) < p.Amount {
		return ErrInsufficient
	}
	if p.Actor == "" {
		p.Actor = ActorSystem
	}
	return s.post(p, KindHold, -p.Amount)
}

// Settle releases a group's open hold and debits what the work cost, once.
//
// It reports whether it settled: false means the group was already settled,
// which is the exactly-once guarantee doing its job rather than a failure.
func (s *MemStore) Settle(_ context.Context, st Settlement) (bool, error) {
	if st.Cost < 0 {
		return false, ErrNotPositive
	}
	if !st.Holder.Valid() {
		return false, ErrNoHolder
	}
	if strings.TrimSpace(st.Group) == "" {
		return false, ErrNoGroup
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var held money.Micro
	for _, e := range s.entries {
		if e.Group != st.Group {
			continue
		}
		if e.Kind == KindDebit {
			return false, nil // already settled
		}
		if e.Kind.commits() {
			held -= e.Amount
		}
	}
	actor := st.Actor
	if actor == "" {
		actor = ActorSystem
	}
	if held > 0 {
		if err := s.post(Posting{
			Holder: st.Holder, Amount: held, Reason: st.Reason, Group: st.Group, Actor: actor, Labels: st.Labels,
		}, KindRelease, held); err != nil {
			return false, err
		}
	}
	// The debit is written even when it is zero: it is the marker that says this
	// group settled, and without it a retry would release a second time.
	err := s.post(Posting{
		Holder: st.Holder, Amount: st.Cost, Reason: st.Reason, Group: st.Group, Actor: actor, Labels: st.Labels,
	}, KindDebit, -st.Cost)
	return err == nil, err
}

// Reverse undoes a referenced entry by the exact amount it moved, idempotent on
// the reversal's own reference.
func (s *MemStore) Reverse(_ context.Context, r Reversal) (Effect, error) {
	if strings.TrimSpace(r.Of) == "" || strings.TrimSpace(r.Ref) == "" {
		return Effect{}, ErrNoRef
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byRef[r.Ref]; dup {
		return Effect{}, nil
	}
	i, ok := s.byRef[r.Of]
	if !ok {
		return Effect{}, ErrNotFound
	}
	orig := s.entries[i]
	before := s.balance(orig.Holder)
	amount := orig.Amount
	if amount < 0 {
		amount = -amount
	}
	actor := r.Actor
	if actor == "" {
		actor = ActorSystem
	}
	// The sign is the opposite of what the original did, so reversing a credit
	// takes money and reversing a debit returns it.
	delta := -orig.Amount
	if err := s.post(Posting{
		Holder: orig.Holder, Amount: amount, Reason: r.Reason, Ref: r.Ref, Group: orig.Group, Actor: actor, Labels: r.Labels,
	}, KindReverse, delta); err != nil {
		return Effect{}, err
	}
	return Effect{Applied: true, Amount: amount, Before: before, After: s.balance(orig.Holder)}, nil
}

// Balance is everything a holder received less everything spent. It excludes
// holds: a hold has not been spent.
func (s *MemStore) Balance(_ context.Context, h Holder) (money.Micro, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balance(h), nil
}

// Available is Balance less every open hold: what a holder may still commit.
func (s *MemStore) Available(_ context.Context, h Holder) (money.Micro, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available(h), nil
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
		out[e.Holder] += e.Amount
	}
	return out, nil
}

// Entries lists a holder's statement, newest first.
func (s *MemStore) Entries(_ context.Context, h Holder, p Page) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
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
		sums[e.Holder] += e.Amount
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
		total += e.Amount
	}
	return total, nil
}

// Within runs fn as one unit: if it returns an error, every write it made is
// discarded.
//
// The in-memory store rolls back by snapshotting, which is affordable because
// the store is for tests and small deployments. The shape matters more than the
// mechanism: a caller writes the same code against either implementation, and
// the contract suite asserts that a failed unit moves nothing in both.
func (s *MemStore) Within(ctx context.Context, fn func(context.Context, Ops) error) error {
	s.mu.Lock()
	snapshot := slices.Clone(s.entries)
	refs := maps.Clone(s.byRef)
	s.mu.Unlock()

	if err := fn(ctx, s); err != nil {
		s.mu.Lock()
		s.entries, s.byRef = snapshot, refs
		s.mu.Unlock()
		return err
	}
	return nil
}

// balance folds a holder's settled balance. The caller holds the lock.
func (s *MemStore) balance(h Holder) money.Micro {
	var v money.Micro
	for _, e := range s.entries {
		if e.Holder == h && !e.Kind.commits() {
			v += e.Amount
		}
	}
	return v
}

// available folds a holder's balance less its open holds. The caller holds the
// lock.
func (s *MemStore) available(h Holder) money.Micro {
	var v money.Micro
	for _, e := range s.entries {
		if e.Holder == h {
			v += e.Amount
		}
	}
	return v
}

// post appends one entry, refusing a duplicate reference. The caller holds the
// lock and has already validated the posting.
func (s *MemStore) post(p Posting, kind Kind, amount money.Micro) error {
	if p.Ref != "" {
		if _, dup := s.byRef[p.Ref]; dup {
			return nil // idempotent: this reference already moved the balance
		}
	}
	id, err := newID()
	if err != nil {
		return err
	}
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
	return nil
}

// cloneEntry hands out a copy, so a caller cannot mutate the ledger by writing
// through a returned entry's label map.
func cloneEntry(e Entry) Entry {
	if e.Labels != nil {
		e.Labels = maps.Clone(e.Labels)
	}
	return e
}

// checkPosting validates a one-sided write before anything is consulted, so
// every store refuses the same nonsense the same way.
func checkPosting(p Posting, needRef bool) error {
	if !p.Holder.Valid() {
		return ErrNoHolder
	}
	if p.Amount <= 0 {
		return ErrNotPositive
	}
	if needRef && strings.TrimSpace(p.Ref) == "" {
		return ErrNoRef
	}
	return nil
}

// checkTransfer validates a two-sided move.
func checkTransfer(t Transfer) error {
	if !t.From.Valid() || !t.To.Valid() {
		return ErrNoHolder
	}
	if t.From == t.To {
		return ErrSameHolder
	}
	if t.Amount <= 0 {
		return ErrNotPositive
	}
	return nil
}

// newID mints a ledger entry identifier.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ledger: mint entry id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
