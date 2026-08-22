package ledger

import (
	"context"

	"latere.ai/x/pay/money"
)

// Ops is the portable write surface: everything that moves money, and nothing
// that knows what it is writing to.
//
// It is separate from Store because a hold and the row it guards must commit
// together, in a transaction the *caller* owns, and a driver's transaction
// handle cannot pass through an interface that both a Postgres and an in-memory
// store satisfy. So the port carries the operations, and a driver package
// exposes the enlistment: see pgledger.Store.Bind.
type Ops interface {
	// Credit adds to a holder. Idempotent on Ref, which is required: money
	// entering from outside always has an external reference to dedupe on.
	Credit(ctx context.Context, p Posting) error
	// Debit takes from a holder. Idempotent on Ref when one is given, which a
	// caller that intends to retry must give. It does not refuse for want of
	// balance: whether the work may happen is asked before the work.
	Debit(ctx context.Context, p Posting) error
	// Adjust corrects an entry after the fact, up or down.
	Adjust(ctx context.Context, p Posting, up bool) error
	// Transfer moves credit between two holders atomically, refusing with
	// ErrInsufficient when the source cannot cover it.
	Transfer(ctx context.Context, t Transfer) error
	// Hold commits credit to a unit of work about to start, refusing with
	// ErrInsufficient when there is not that much available.
	Hold(ctx context.Context, p Posting) error
	// Settle releases a group's hold and debits what the work cost, exactly
	// once. False means another writer settled first, which is not an error.
	Settle(ctx context.Context, s Settlement) (bool, error)
	// Reverse undoes a referenced entry, idempotent on the reversal's own
	// reference, and reports the balance either side so a product can act on a
	// crossing this package has no opinion about.
	Reverse(ctx context.Context, r Reversal) (Effect, error)
}

// Store is the ledger: the writes, the reads, and a way to run several writes
// atomically when the caller has no transaction of its own.
//
// Reads take no caller. A balance is shown by surfaces that have already
// decided who may look, and the enforcement path is nobody. Authorization is
// the product's; this package takes a holder and an amount.
type Store interface {
	Ops

	// Balance is everything a holder received less everything spent. It
	// excludes holds: a hold has not been spent, and a holder whose only
	// commitment is work in flight has not yet paid for it.
	Balance(ctx context.Context, h Holder) (money.Micro, error)
	// Available is Balance less every open hold: what a holder may still
	// commit. This is the number an admission gate compares against, and the
	// number a transfer out may not exceed, because credit already committed to
	// running work cannot be pulled out from under it.
	Available(ctx context.Context, h Holder) (money.Micro, error)
	// BalancesFor folds several holders in one pass, for a table that would
	// otherwise issue a query per row.
	BalancesFor(ctx context.Context, hs []Holder) (map[Holder]money.Micro, error)
	// Entries lists a holder's statement, newest first.
	Entries(ctx context.Context, h Holder, p Page) ([]Entry, error)
	// EntryByRef finds the entry carrying ref. A reversal uses it to undo a
	// purchase by the exact amount it moved.
	EntryByRef(ctx context.Context, ref string) (Entry, bool, error)
	// NegativeHolders returns every holder in a namespace whose settled balance
	// is below zero: the accounts that owe. Most-owed first.
	NegativeHolders(ctx context.Context, namespace string) ([]HolderBalance, error)
	// TotalOutstanding is every credit sold and not yet delivered, across the
	// given namespaces or all of them. It is the solvency figure an operator
	// watches against what the processor account holds.
	TotalOutstanding(ctx context.Context, namespaces ...string) (money.Micro, error)

	// Within runs fn as one atomic unit against ledger-owned storage, for a
	// caller with no transaction of its own to enlist in.
	Within(ctx context.Context, fn func(context.Context, Ops) error) error
}
