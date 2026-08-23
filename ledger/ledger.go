// Package ledger is what money the platform holds, whose it is, and what
// happened to it.
//
// Every balance is a fold over one append-only table. There is no stored
// balance anywhere: a materialised total is a second source of truth and it
// drifts the first time a row is corrected. Folding also means the answer to
// "why does this holder have this much?" is the rows themselves, each naming
// its actor and its cause.
//
// See docs/money-model.md.
package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pay/money"
)

// Errors this package returns.
var (
	// ErrNotPositive reports a write of zero or less. Every write API takes an
	// unsigned magnitude and applies the sign itself, so a caller cannot turn a
	// debit into a credit by passing a negative. This matters concretely: at
	// least one producer in the platform signals "unpriceable" with -1.
	ErrNotPositive = errors.New("ledger: an amount must be positive")
	// ErrInsufficient reports a commitment larger than what the holder has
	// available. It is a refusal, not a failure.
	ErrInsufficient = errors.New("ledger: not enough credit")
	// ErrNoHolder reports a write with no holder to key on.
	ErrNoHolder = errors.New("ledger: an entry needs a holder")
	// ErrNoRef reports a write that must be idempotent but carries no external
	// reference. A purchase or a rollup must dedupe, and the reference is what
	// dedupes it, so a missing one is a programming error rather than a refusal.
	ErrNoRef = errors.New("ledger: this entry needs an external reference")
	// ErrNoGroup reports a hold or settlement with no unit of work to key on.
	ErrNoGroup = errors.New("ledger: this entry needs a group")
	// ErrNotFound reports a lookup that matched nothing.
	ErrNotFound = errors.New("ledger: no such entry")
	// ErrSameHolder reports a transfer to the holder it came from, which would
	// write two entries that cancel and tell a reader nothing.
	ErrSameHolder = errors.New("ledger: a transfer needs two different holders")
)

// Holder is whose money an entry is about: one namespaced string,
// "<namespace>:<id>", rather than several nullable columns, so a balance is one
// predicate and a level added later does not reshape the table.
//
// Products own their namespaces. There are deliberately no foreign keys: a
// ledger must outlive the thing it is about, so deleting a project must not
// delete the record of what its sessions cost.
type Holder string

// NewHolder builds a holder key. The namespace is lowercased and the id is
// trimmed, so a holder funded one way matches a holder charged another.
func NewHolder(namespace, id string) Holder {
	return Holder(strings.ToLower(strings.TrimSpace(namespace)) + ":" + strings.TrimSpace(id))
}

// Split returns a holder's namespace and id, and whether it is well-formed.
func (h Holder) Split() (namespace, id string, ok bool) {
	s := string(h)
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// Namespace is the holder's namespace, or empty if it is malformed.
func (h Holder) Namespace() string {
	ns, _, _ := h.Split()
	return ns
}

// Valid reports a well-formed holder key.
func (h Holder) Valid() bool {
	_, _, ok := h.Split()
	return ok
}

// String makes Holder printable.
func (h Holder) String() string { return string(h) }

// Kind is the closed ledger-mechanics vocabulary. It determines an entry's sign
// and whether it commits. Products do not extend it; they describe *why* with a
// Reason.
type Kind string

// The kinds of entry.
const (
	// KindCredit adds to a holder: money entered.
	KindCredit Kind = "credit"
	// KindDebit takes from a holder: money spent. Exactly one per group, which
	// is what makes settlement exactly-once.
	KindDebit Kind = "debit"
	// KindTransfer is the paired move between two holders, written as two
	// entries in one transaction so money is never in neither place.
	KindTransfer Kind = "transfer"
	// KindHold commits a holder's credit to work about to start. It does not
	// spend it: it makes it unavailable to a second unit of work, so a burst of
	// concurrent requests cannot each be admitted against the same balance.
	KindHold Kind = "hold"
	// KindRelease returns a hold at settlement, paired with the debit for what
	// the work actually cost.
	KindRelease Kind = "release"
	// KindReverse undoes a referenced entry: a refund or a dispute.
	KindReverse Kind = "reverse"
	// KindAdjust corrects an entry after the fact, in either direction.
	// Deliberately carries no uniqueness constraint.
	KindAdjust Kind = "adjust"
)

// Sign is the direction a kind moves money: +1 adds, -1 takes, 0 for the kinds
// whose direction depends on the operation rather than the kind.
func (k Kind) Sign() int {
	switch k {
	case KindCredit, KindRelease:
		return 1
	case KindDebit, KindHold:
		return -1
	default:
		return 0
	}
}

// commits reports whether a kind moves money out of what a holder may still
// commit without having spent it. Holds and their releases are exactly that: an
// available balance counts them, a settled balance does not.
func (k Kind) commits() bool { return k == KindHold || k == KindRelease }

// Valid reports a kind this package knows.
func (k Kind) Valid() bool {
	switch k {
	case KindCredit, KindDebit, KindTransfer, KindHold, KindRelease, KindReverse, KindAdjust:
		return true
	default:
		return false
	}
}

// Reason is the product's label for why an entry exists: "topup", "allocate",
// "session", "draft". It never affects arithmetic; it is what a statement line
// reads as. Free-form to this package, and a product should declare a closed
// set of its own.
type Reason string

// Entry is one line of the ledger. Nothing is ever updated or deleted; a
// mistake is corrected by another entry, which is what keeps the history and
// the balance the same fact.
type Entry struct {
	ID     string `json:"id"`
	Holder Holder `json:"holder"`
	// Amount is signed micro-USD as stored, so a balance is a SUM and never a
	// case analysis over kinds.
	Amount money.Micro `json:"amount"`
	Kind   Kind        `json:"kind"`
	Reason Reason      `json:"reason,omitempty"`
	// Ref is an external idempotency key: a processor's payment intent, a refund
	// id, a rollup window key. A unique index refuses a second row for the same
	// reference, which is what makes a write idempotent. Empty on internal moves.
	Ref string `json:"ref,omitempty"`
	// Group ties entries that belong together: the two sides of a transfer, and
	// the hold, release and debit of one unit of work.
	Group string `json:"group,omitempty"`
	// Actor is who caused this entry, for the statement and the audit trail.
	Actor string `json:"actor"`
	// Labels are product dimensions this package stores and never interprets.
	// A snapshot, not a reference, which is what lets a purged project still
	// read correctly in an old statement.
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// ActorSystem is the actor for entries the platform writes rather than a
// person: a hold taken at admission, and the release and debit that settle it.
const ActorSystem = "system"

// Posting is a one-sided write. Amount is an unsigned magnitude; the operation
// supplies the sign.
type Posting struct {
	Holder Holder
	Amount money.Micro
	Reason Reason
	// Ref is the idempotency key. Required for Credit, and for any Debit a
	// caller intends to retry; see the rollup shape below.
	Ref    string
	Group  string
	Actor  string
	Labels map[string]string
}

// Transfer moves credit between two holders. Both sides are one transaction:
// money is never in neither place.
type Transfer struct {
	From, To Holder
	Amount   money.Micro
	Reason   Reason
	Ref      string
	Group    string
	Actor    string
	Labels   map[string]string
}

// Settlement closes a unit of work: release what it was holding, and debit what
// it actually cost.
type Settlement struct {
	Holder Holder
	// Group is the unit of work whose hold is being settled.
	Group string
	// Cost is an unsigned magnitude. Zero is legal and still settles: the debit
	// is the marker that says this group is closed.
	Cost   money.Micro
	Reason Reason
	Actor  string
	Labels map[string]string
}

// Reversal undoes a referenced entry.
type Reversal struct {
	// Of is the Ref of the entry being undone.
	Of string
	// Ref is the reversal's own reference, distinct from Of, so a refund dedupes
	// independently of the purchase it reverses.
	Ref    string
	Reason Reason
	Actor  string
	Labels map[string]string
}

// Effect is what a Reverse did, so a product can act on a crossing without this
// package knowing what a crossing means. A product that freezes an account when
// a balance goes negative compares Before and After; the policy is its own.
type Effect struct {
	// Applied is false when the reversal was already recorded, which is
	// idempotency working rather than a failure.
	Applied bool
	Amount  money.Micro
	Before  money.Micro
	After   money.Micro
}

// HolderBalance pairs a holder with a folded balance.
type HolderBalance struct {
	Holder  Holder      `json:"holder"`
	Balance money.Micro `json:"balance"`
}

// Page bounds a statement read.
type Page struct {
	// Limit caps the rows returned; zero means a sensible default.
	Limit int
	// Before returns only entries older than this, for paging back through a
	// statement. Zero means start at the newest.
	Before time.Time
}

// defaultLimit caps a statement that asked for no limit.
const defaultLimit = 200

// limit resolves a page's row cap.
func (p Page) limit() int {
	if p.Limit <= 0 {
		return defaultLimit
	}
	return p.Limit
}

// RollupRef builds the reference for a rollup debit: the shape that lets a
// high-rate consumer meter into a fast counter and settle to this ledger
// periodically, without one row per billable event.
//
// Each flush posts the delta accrued since the last one under a monotonically
// increasing sequence, so a retried flush posts the same reference and is a
// no-op, while a lost flush is recovered by the next.
func RollupRef(h Holder, window time.Time, seq int) string {
	return string(h) + ":" + window.UTC().Format(time.RFC3339) + ":" + strconv.Itoa(seq)
}

// CheckPosting validates a one-sided write before any store is consulted.
//
// It is exported because an implementation of Ops lives in another package and
// must refuse the same nonsense the same way. Validation duplicated per store
// is validation that drifts, and in a ledger a drifted refusal is a wrong
// balance.
func CheckPosting(p Posting, needRef bool) error {
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

// CheckTransfer validates a two-sided move.
func CheckTransfer(t Transfer) error {
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

// randRead is the entropy source for entry ids. It is a variable so the failure
// path is reachable in a test: an id that cannot be minted must abort the write
// rather than produce an entry with an empty primary key.
var randRead = rand.Read

// NewID mints a ledger entry identifier. Exported for implementations of Ops
// living in other packages, so there is one minter rather than one per store.
func NewID() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", fmt.Errorf("ledger: mint entry id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SetRandReadForTest replaces the entropy source and returns a function that
// restores it. It exists only so the mint-failure path can be exercised.
func SetRandReadForTest(fn func([]byte) (int, error)) func() {
	prev := randRead
	randRead = fn
	return func() { randRead = prev }
}
