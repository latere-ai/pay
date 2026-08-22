// Package ledgertest is the contract every ledger.Store must satisfy.
//
// The in-memory store and the Postgres store are different implementations of
// the same promises about money, and the only way to keep them honest is to
// drive both through one suite. A guarantee asserted here is a guarantee; one
// asserted in a single store's own tests is a coincidence.
package ledgertest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/money"
)

// Factory builds an empty store. It is called once per subtest, so each starts
// with no entries.
type Factory func(t *testing.T) ledger.Store

const (
	alice = ledger.Holder("user:alice@example.test")
	bob   = ledger.Holder("user:bob@example.test")
	pot   = ledger.Holder("project:p1")
)

// RunStoreContract drives a store through every promise the port makes.
func RunStoreContract(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("a credit is idempotent on its reference", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		p := ledger.Posting{Holder: alice, Amount: 20 * money.Dollar, Ref: "pi_1", Reason: "topup", Actor: "stripe"}
		mustCredit(t, s, ctx, p)
		// The same delivery again, as a retried webhook would.
		mustCredit(t, s, ctx, p)
		if got := balance(t, s, ctx, alice); got != 20*money.Dollar {
			t.Errorf("balance = %s, want $20: a retried reference moved the balance twice", got.String(money.USD))
		}
		if n := len(entries(t, s, ctx, alice)); n != 1 {
			t.Errorf("wrote %d entries for one reference, want 1", n)
		}
	})

	t.Run("a credit needs a reference", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		err := s.Credit(ctx, ledger.Posting{Holder: alice, Amount: money.Dollar})
		if !errors.Is(err, ledger.ErrNoRef) {
			t.Errorf("Credit without a ref = %v, want ErrNoRef", err)
		}
	})

	t.Run("a write takes a magnitude and refuses a negative", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		// -1 by name: it is the platform's unpriceable-model sentinel, and a
		// signed API would post it as a one-micro credit.
		for _, amount := range []money.Micro{-1, 0} {
			if err := s.Debit(ctx, ledger.Posting{Holder: alice, Amount: amount, Ref: "d"}); !errors.Is(err, ledger.ErrNotPositive) {
				t.Errorf("Debit(%d) = %v, want ErrNotPositive", amount, err)
			}
			if err := s.Credit(ctx, ledger.Posting{Holder: alice, Amount: amount, Ref: "c"}); !errors.Is(err, ledger.ErrNotPositive) {
				t.Errorf("Credit(%d) = %v, want ErrNotPositive", amount, err)
			}
			if err := s.Hold(ctx, ledger.Posting{Holder: alice, Amount: amount, Group: "g"}); !errors.Is(err, ledger.ErrNotPositive) {
				t.Errorf("Hold(%d) = %v, want ErrNotPositive", amount, err)
			}
		}
	})

	t.Run("a write needs a well-formed holder", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		for _, h := range []ledger.Holder{"", "nocolon", "user:", ":id"} {
			if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "x"}); !errors.Is(err, ledger.ErrNoHolder) {
				t.Errorf("Credit to %q = %v, want ErrNoHolder", h, err)
			}
		}
	})

	t.Run("a hold is invisible to the balance and subtracted from available", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 10 * money.Dollar, Ref: "seed"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 4 * money.Dollar, Group: "job-1"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if got := balance(t, s, ctx, pot); got != 10*money.Dollar {
			t.Errorf("balance = %s, want $10: a hold is not a spend", got.String(money.USD))
		}
		if got := available(t, s, ctx, pot); got != 6*money.Dollar {
			t.Errorf("available = %s, want $6", got.String(money.USD))
		}
	})

	t.Run("a hold cannot exceed what is available", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 5 * money.Dollar, Ref: "seed"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 6 * money.Dollar, Group: "g"}); !errors.Is(err, ledger.ErrInsufficient) {
			t.Errorf("over-hold = %v, want ErrInsufficient", err)
		}
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: money.Dollar}); !errors.Is(err, ledger.ErrNoGroup) {
			t.Errorf("Hold without a group = %v, want ErrNoGroup", err)
		}
	})

	t.Run("concurrent holds cannot all be admitted against one balance", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		// $10 available, $3 a piece: exactly three may start, whatever the
		// interleaving. This is the promise the advisory lock exists for.
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 10 * money.Dollar, Ref: "seed"})
		const n = 8
		var wg sync.WaitGroup
		admitted := make([]bool, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 3 * money.Dollar, Group: "job-" + itoa(i)})
				admitted[i] = err == nil
			}(i)
		}
		wg.Wait()
		count := 0
		for _, ok := range admitted {
			if ok {
				count++
			}
		}
		if count != 3 {
			t.Errorf("%d holds admitted, want exactly 3", count)
		}
		if got := available(t, s, ctx, pot); got != money.Dollar {
			t.Errorf("available after the burst = %s, want $1", got.String(money.USD))
		}
	})

	t.Run("settlement releases the hold and charges the real cost, exactly once", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 10 * money.Dollar, Ref: "seed"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 4 * money.Dollar, Group: "job-1"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		ok, err := s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "job-1", Cost: 150 * money.Cent})
		if err != nil || !ok {
			t.Fatalf("Settle = %v, %v", ok, err)
		}
		if got := balance(t, s, ctx, pot); got != 10*money.Dollar-150*money.Cent {
			t.Errorf("balance = %s, want $8.50", got.String(money.USD))
		}
		if got := available(t, s, ctx, pot); got != 10*money.Dollar-150*money.Cent {
			t.Errorf("available = %s: the hold was not released", got.String(money.USD))
		}
		// A second settlement is a no-op, not a second release.
		ok, err = s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "job-1", Cost: 150 * money.Cent})
		if err != nil {
			t.Fatalf("second Settle: %v", err)
		}
		if ok {
			t.Error("a second settlement reported that it settled")
		}
		if got := balance(t, s, ctx, pot); got != 10*money.Dollar-150*money.Cent {
			t.Errorf("balance after a double settle = %s: it charged twice", got.String(money.USD))
		}
	})

	t.Run("concurrent settlements of one group produce one debit", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 10 * money.Dollar, Ref: "seed"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 4 * money.Dollar, Group: "job-1"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		const n = 6
		var wg sync.WaitGroup
		settled := make([]bool, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				ok, _ := s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "job-1", Cost: money.Dollar})
				settled[i] = ok
			}(i)
		}
		wg.Wait()
		count := 0
		for _, ok := range settled {
			if ok {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%d settlements reported success, want exactly 1", count)
		}
		if got := balance(t, s, ctx, pot); got != 9*money.Dollar {
			t.Errorf("balance = %s, want $9: the group was charged more than once", got.String(money.USD))
		}
	})

	t.Run("a zero-cost settlement still closes the group", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 5 * money.Dollar, Ref: "seed"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 2 * money.Dollar, Group: "g"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if ok, err := s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "g", Cost: 0}); err != nil || !ok {
			t.Fatalf("Settle = %v, %v", ok, err)
		}
		if got := available(t, s, ctx, pot); got != 5*money.Dollar {
			t.Errorf("available = %s, want the hold returned in full", got.String(money.USD))
		}
		// And it is closed: a retry must not release again.
		if ok, _ := s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "g", Cost: 0}); ok {
			t.Error("a zero-cost settlement did not close the group")
		}
	})

	t.Run("settling a group that never held anything is still exactly once", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 5 * money.Dollar, Ref: "seed"})
		if ok, err := s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "never-held", Cost: money.Dollar}); err != nil || !ok {
			t.Fatalf("Settle = %v, %v", ok, err)
		}
		if got := balance(t, s, ctx, pot); got != 4*money.Dollar {
			t.Errorf("balance = %s, want $4", got.String(money.USD))
		}
		if ok, _ := s.Settle(ctx, ledger.Settlement{Holder: pot, Group: "never-held", Cost: money.Dollar}); ok {
			t.Error("the group settled twice")
		}
	})

	t.Run("a transfer moves both sides or neither", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "seed"})
		if err := s.Transfer(ctx, ledger.Transfer{From: alice, To: pot, Amount: 4 * money.Dollar, Reason: "allocate", Actor: "alice"}); err != nil {
			t.Fatalf("Transfer: %v", err)
		}
		if got := balance(t, s, ctx, alice); got != 6*money.Dollar {
			t.Errorf("source = %s, want $6", got.String(money.USD))
		}
		if got := balance(t, s, ctx, pot); got != 4*money.Dollar {
			t.Errorf("destination = %s, want $4", got.String(money.USD))
		}
		if err := s.Transfer(ctx, ledger.Transfer{From: alice, To: pot, Amount: 100 * money.Dollar}); !errors.Is(err, ledger.ErrInsufficient) {
			t.Errorf("over-transfer = %v, want ErrInsufficient", err)
		}
		if err := s.Transfer(ctx, ledger.Transfer{From: alice, To: alice, Amount: money.Dollar}); !errors.Is(err, ledger.ErrSameHolder) {
			t.Errorf("self-transfer = %v, want ErrSameHolder", err)
		}
		// Nothing moved on either refusal.
		if got := balance(t, s, ctx, alice); got != 6*money.Dollar {
			t.Errorf("source after refusals = %s, want $6", got.String(money.USD))
		}
	})

	t.Run("a transfer may not spend what a hold has committed", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 10 * money.Dollar, Ref: "seed"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 8 * money.Dollar, Group: "running"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		// The balance says $10, but $8 is committed to work in flight.
		if err := s.Transfer(ctx, ledger.Transfer{From: pot, To: alice, Amount: 5 * money.Dollar}); !errors.Is(err, ledger.ErrInsufficient) {
			t.Errorf("transfer out of held credit = %v, want ErrInsufficient", err)
		}
	})

	t.Run("a transfer is idempotent on its reference", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "seed"})
		mv := ledger.Transfer{From: alice, To: pot, Amount: 3 * money.Dollar, Ref: "mv_1"}
		if err := s.Transfer(ctx, mv); err != nil {
			t.Fatalf("Transfer: %v", err)
		}
		if err := s.Transfer(ctx, mv); err != nil {
			t.Fatalf("Transfer again: %v", err)
		}
		if got := balance(t, s, ctx, pot); got != 3*money.Dollar {
			t.Errorf("destination = %s, want $3: the move applied twice", got.String(money.USD))
		}
	})

	t.Run("a reversal undoes the exact amount and dedupes on its own reference", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 1_870 * money.Cent, Ref: "pi_1", Reason: "topup"})
		eff, err := s.Reverse(ctx, ledger.Reversal{Of: "pi_1", Ref: "re_1", Reason: "refund"})
		if err != nil {
			t.Fatalf("Reverse: %v", err)
		}
		if !eff.Applied {
			t.Error("the reversal reported that it did nothing")
		}
		if eff.Amount != 1_870*money.Cent {
			t.Errorf("reversed %s, want the exact credited amount", eff.Amount.String(money.USD))
		}
		if eff.Before != 1_870*money.Cent || eff.After != 0 {
			t.Errorf("effect = %s → %s, want $18.70 → $0", eff.Before.String(money.USD), eff.After.String(money.USD))
		}
		// The same refund delivered twice reverses once.
		again, err := s.Reverse(ctx, ledger.Reversal{Of: "pi_1", Ref: "re_1"})
		if err != nil {
			t.Fatalf("Reverse again: %v", err)
		}
		if again.Applied {
			t.Error("a replayed refund applied a second time")
		}
		if got := balance(t, s, ctx, alice); got != 0 {
			t.Errorf("balance = %s, want $0", got.String(money.USD))
		}
	})

	t.Run("a reversal may take a holder negative, which is the debt it leaves", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "pi_1"})
		if err := s.Debit(ctx, ledger.Posting{Holder: alice, Amount: 9 * money.Dollar, Ref: "spend"}); err != nil {
			t.Fatalf("Debit: %v", err)
		}
		eff, err := s.Reverse(ctx, ledger.Reversal{Of: "pi_1", Ref: "re_1"})
		if err != nil {
			t.Fatalf("Reverse: %v", err)
		}
		if eff.Before < 0 || eff.After >= 0 {
			t.Errorf("effect = %s → %s, want a crossing into the red", eff.Before.String(money.USD), eff.After.String(money.USD))
		}
		if got := balance(t, s, ctx, alice); got != -9*money.Dollar {
			t.Errorf("balance = %s, want -$9", got.String(money.USD))
		}
	})

	t.Run("reversing something unknown is refused, not guessed at", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		if _, err := s.Reverse(ctx, ledger.Reversal{Of: "nope", Ref: "re_1"}); !errors.Is(err, ledger.ErrNotFound) {
			t.Errorf("Reverse of an unknown reference = %v, want ErrNotFound", err)
		}
		if _, err := s.Reverse(ctx, ledger.Reversal{Of: "", Ref: "re"}); !errors.Is(err, ledger.ErrNoRef) {
			t.Errorf("Reverse without an original = %v, want ErrNoRef", err)
		}
		if _, err := s.Reverse(ctx, ledger.Reversal{Of: "x", Ref: ""}); !errors.Is(err, ledger.ErrNoRef) {
			t.Errorf("Reverse without its own ref = %v, want ErrNoRef", err)
		}
	})

	t.Run("rollup debits fold the same with replays as without", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 100 * money.Dollar, Ref: "seed"})
		window := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
		// Six flushes of the window's delta, every other one delivered twice.
		var want money.Micro
		for seq := 0; seq < 6; seq++ {
			delta := money.Micro(1_000 * (seq + 1))
			want += delta
			p := ledger.Posting{
				Holder: alice, Amount: delta, Reason: "gateway",
				Ref: ledger.RollupRef(alice, window, seq), Actor: ledger.ActorSystem,
			}
			if err := s.Debit(ctx, p); err != nil {
				t.Fatalf("rollup flush %d: %v", seq, err)
			}
			if seq%2 == 0 {
				if err := s.Debit(ctx, p); err != nil {
					t.Fatalf("rollup replay %d: %v", seq, err)
				}
			}
		}
		if got := balance(t, s, ctx, alice); got != 100*money.Dollar-want {
			t.Errorf("balance = %s, want %s: replays were not deduped",
				got.String(money.USD), (100*money.Dollar - want).String(money.USD))
		}
	})

	t.Run("an adjustment corrects in either direction", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "seed"})
		if err := s.Adjust(ctx, ledger.Posting{Holder: alice, Amount: 2 * money.Dollar, Ref: "adj_down", Reason: "reconcile"}, false); err != nil {
			t.Fatalf("Adjust down: %v", err)
		}
		if err := s.Adjust(ctx, ledger.Posting{Holder: alice, Amount: money.Dollar, Ref: "adj_up", Reason: "reconcile"}, true); err != nil {
			t.Fatalf("Adjust up: %v", err)
		}
		if got := balance(t, s, ctx, alice); got != 9*money.Dollar {
			t.Errorf("balance = %s, want $9", got.String(money.USD))
		}
	})

	t.Run("a statement reads newest first and honours its limit", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		for i := 0; i < 5; i++ {
			mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: money.Dollar, Ref: "r" + itoa(i), Reason: ledger.Reason("n" + itoa(i))})
		}
		got := entries(t, s, ctx, alice)
		if len(got) != 5 {
			t.Fatalf("got %d entries, want 5", len(got))
		}
		if got[0].Reason != "n4" {
			t.Errorf("newest entry reason = %q, want n4: the statement is not newest-first", got[0].Reason)
		}
		limited, err := s.Entries(ctx, alice, ledger.Page{Limit: 2})
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		if len(limited) != 2 {
			t.Errorf("limit 2 returned %d rows", len(limited))
		}
		// Another holder's entries never leak in.
		if n := len(entries(t, s, ctx, bob)); n != 0 {
			t.Errorf("an unrelated holder has %d entries", n)
		}
	})

	t.Run("an entry is found by its external reference", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 7 * money.Dollar, Ref: "pi_9", Reason: "topup"})
		got, ok, err := s.EntryByRef(ctx, "pi_9")
		if err != nil || !ok {
			t.Fatalf("EntryByRef = %v, %v", ok, err)
		}
		if got.Holder != alice || got.Amount != 7*money.Dollar || got.Kind != ledger.KindCredit {
			t.Errorf("entry = %+v", got)
		}
		if _, ok, err := s.EntryByRef(ctx, "absent"); err != nil || ok {
			t.Errorf("EntryByRef(absent) = %v, %v", ok, err)
		}
	})

	t.Run("labels are stored, returned, and never interpreted", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{
			Holder: alice, Amount: money.Dollar, Ref: "r",
			Labels: map[string]string{"project_name": "A study", "team": "core"},
		})
		got := entries(t, s, ctx, alice)
		if len(got) != 1 {
			t.Fatalf("got %d entries", len(got))
		}
		if got[0].Labels["project_name"] != "A study" || got[0].Labels["team"] != "core" {
			t.Errorf("labels = %v", got[0].Labels)
		}
		// A returned entry is a copy: writing through it must not reach the store.
		got[0].Labels["project_name"] = "mutated"
		if again := entries(t, s, ctx, alice); again[0].Labels["project_name"] != "A study" {
			t.Error("the store handed out a live reference to its own label map")
		}
	})

	t.Run("several holders fold in one pass", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 5 * money.Dollar, Ref: "a"})
		mustCredit(t, s, ctx, ledger.Posting{Holder: bob, Amount: 3 * money.Dollar, Ref: "b"})
		if err := s.Hold(ctx, ledger.Posting{Holder: alice, Amount: money.Dollar, Group: "g"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		got, err := s.BalancesFor(ctx, []ledger.Holder{alice, bob, pot})
		if err != nil {
			t.Fatalf("BalancesFor: %v", err)
		}
		if got[alice] != 5*money.Dollar {
			t.Errorf("alice = %s, want $5: a hold leaked into a settled balance", got[alice].String(money.USD))
		}
		if got[bob] != 3*money.Dollar {
			t.Errorf("bob = %s, want $3", got[bob].String(money.USD))
		}
		// A holder with no entry is absent, not zero-valued.
		if _, present := got[pot]; present {
			t.Error("a holder with no entries was returned")
		}
	})

	t.Run("holders in the red are found, most-owed first", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: money.Dollar, Ref: "a"})
		if err := s.Debit(ctx, ledger.Posting{Holder: alice, Amount: 3 * money.Dollar, Ref: "da"}); err != nil {
			t.Fatalf("Debit: %v", err)
		}
		if err := s.Debit(ctx, ledger.Posting{Holder: bob, Amount: 5 * money.Dollar, Ref: "db"}); err != nil {
			t.Fatalf("Debit: %v", err)
		}
		// A pot in another namespace is excluded by the namespace filter.
		if err := s.Debit(ctx, ledger.Posting{Holder: pot, Amount: 9 * money.Dollar, Ref: "dp"}); err != nil {
			t.Fatalf("Debit: %v", err)
		}
		got, err := s.NegativeHolders(ctx, "user")
		if err != nil {
			t.Fatalf("NegativeHolders: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d debtors, want 2 (a different namespace leaked in)", len(got))
		}
		if got[0].Holder != bob {
			t.Errorf("first debtor = %s, want the most-owed (bob at -$5)", got[0].Holder)
		}
		if got[0].Balance != -5*money.Dollar || got[1].Balance != -2*money.Dollar {
			t.Errorf("balances = %v", got)
		}
	})

	t.Run("outstanding credit sums across or within namespaces", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "a"})
		mustCredit(t, s, ctx, ledger.Posting{Holder: pot, Amount: 4 * money.Dollar, Ref: "p"})
		if err := s.Hold(ctx, ledger.Posting{Holder: pot, Amount: 2 * money.Dollar, Group: "g"}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		all, err := s.TotalOutstanding(ctx)
		if err != nil {
			t.Fatalf("TotalOutstanding: %v", err)
		}
		if all != 14*money.Dollar {
			t.Errorf("total = %s, want $14: a hold was counted as spent", all.String(money.USD))
		}
		users, err := s.TotalOutstanding(ctx, "user")
		if err != nil {
			t.Fatalf("TotalOutstanding(user): %v", err)
		}
		if users != 10*money.Dollar {
			t.Errorf("user namespace = %s, want $10", users.String(money.USD))
		}
	})

	t.Run("several writes run as one unit", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		err := s.Within(ctx, func(ctx context.Context, ops ledger.Ops) error {
			if err := ops.Credit(ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "w1"}); err != nil {
				return err
			}
			return ops.Transfer(ctx, ledger.Transfer{From: alice, To: pot, Amount: 4 * money.Dollar})
		})
		if err != nil {
			t.Fatalf("Within: %v", err)
		}
		if got := balance(t, s, ctx, pot); got != 4*money.Dollar {
			t.Errorf("destination = %s, want $4", got.String(money.USD))
		}
	})

	t.Run("a unit that fails moves nothing", func(t *testing.T) {
		s, ctx := newStore(t), context.Background()
		mustCredit(t, s, ctx, ledger.Posting{Holder: alice, Amount: 10 * money.Dollar, Ref: "seed"})
		sentinel := errors.New("caller changed its mind")
		err := s.Within(ctx, func(ctx context.Context, ops ledger.Ops) error {
			if err := ops.Transfer(ctx, ledger.Transfer{From: alice, To: pot, Amount: 4 * money.Dollar}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("Within = %v, want the caller's error", err)
		}
		if got := balance(t, s, ctx, pot); got != 0 {
			t.Errorf("destination = %s, want $0: a failed unit left money behind", got.String(money.USD))
		}
		if got := balance(t, s, ctx, alice); got != 10*money.Dollar {
			t.Errorf("source = %s, want $10 back", got.String(money.USD))
		}
	})
}

func mustCredit(t *testing.T, s ledger.Store, ctx context.Context, p ledger.Posting) {
	t.Helper()
	if err := s.Credit(ctx, p); err != nil {
		t.Fatalf("Credit(%s, %s): %v", p.Holder, p.Amount.String(money.USD), err)
	}
}

func balance(t *testing.T, s ledger.Store, ctx context.Context, h ledger.Holder) money.Micro {
	t.Helper()
	v, err := s.Balance(ctx, h)
	if err != nil {
		t.Fatalf("Balance(%s): %v", h, err)
	}
	return v
}

func available(t *testing.T, s ledger.Store, ctx context.Context, h ledger.Holder) money.Micro {
	t.Helper()
	v, err := s.Available(ctx, h)
	if err != nil {
		t.Fatalf("Available(%s): %v", h, err)
	}
	return v
}

func entries(t *testing.T, s ledger.Store, ctx context.Context, h ledger.Holder) []ledger.Entry {
	t.Helper()
	got, err := s.Entries(ctx, h, ledger.Page{})
	if err != nil {
		t.Fatalf("Entries(%s): %v", h, err)
	}
	return got
}

func itoa(n int) string { return strconv.Itoa(n) }
