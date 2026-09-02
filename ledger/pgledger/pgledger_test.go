// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pgledger_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/ledger/ledgertest"
	"latere.ai/x/pay/ledger/pgledger"
	"latere.ai/x/pay/money"
)

// openTest connects to TEST_DATABASE_URL and applies the schema, or skips.
//
// The variable name matters: a database-free run silently drops every promise
// this file asserts, which is how a ledger can look far less covered, and far
// less proven, than it is. CI always sets it.
func openTest(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the Postgres ledger tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Each subtest starts from an empty ledger, so the contract's balance
	// assertions mean what they say.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS ledger_entries, ledger_migrations`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := pgledger.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func TestPGStoreSatisfiesTheContract(t *testing.T) {
	ledgertest.RunStoreContract(t, func(t *testing.T) ledger.Store {
		return pgledger.New(openTest(t))
	})
}

// Migrate is idempotent: a rolling deploy runs it from several pods at once,
// and the loser must not die on "relation already exists".
func TestMigrateIsIdempotent(t *testing.T) {
	pool := openTest(t)
	ctx := context.Background()
	for i := range 3 {
		if err := pgledger.Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
	}
}

// A statement pages backwards through time. Ordering is the store's job, and
// asserting it needs a deterministic clock rather than a sleep.
func TestEntriesPagesNewestFirst(t *testing.T) {
	pool := openTest(t)
	s := pgledger.New(pool)
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	n := 0
	s.SetClock(func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Minute)
	})
	ctx := context.Background()
	h := ledger.NewHolder("user", "pager@example.test")
	for i := range 4 {
		if err := s.Credit(ctx, ledger.Posting{
			Holder: h, Amount: money.Dollar, Ref: "p" + strconv.Itoa(i), Reason: ledger.Reason("n" + strconv.Itoa(i)),
		}); err != nil {
			t.Fatalf("Credit: %v", err)
		}
	}
	all, err := s.Entries(ctx, h, ledger.Page{})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d entries, want 4", len(all))
	}
	if all[0].Reason != "n3" {
		t.Errorf("newest reason = %q, want n3", all[0].Reason)
	}
	older, err := s.Entries(ctx, h, ledger.Page{Before: all[0].CreatedAt})
	if err != nil {
		t.Fatalf("Entries(before): %v", err)
	}
	if len(older) != 3 {
		t.Errorf("paging before the newest returned %d rows, want 3", len(older))
	}
}

// A ledger that cannot reach its database must return an error from every
// operation, never a plausible zero. A balance that silently reads as $0 during
// an outage is how a gate lets everything through.
func TestEveryOperationFailsClosedOnADeadPool(t *testing.T) {
	pool := openTest(t)
	s := pgledger.New(pool)
	pool.Close()
	ctx := context.Background()
	h := ledger.NewHolder("user", "dead@example.test")

	if _, err := s.Balance(ctx, h); err == nil {
		t.Error("Balance on a closed pool returned no error")
	}
	if _, err := s.Available(ctx, h); err == nil {
		t.Error("Available on a closed pool returned no error")
	}
	if _, err := s.BalancesFor(ctx, []ledger.Holder{h}); err == nil {
		t.Error("BalancesFor on a closed pool returned no error")
	}
	if _, err := s.Entries(ctx, h, ledger.Page{}); err == nil {
		t.Error("Entries on a closed pool returned no error")
	}
	if _, _, err := s.EntryByRef(ctx, "x"); err == nil {
		t.Error("EntryByRef on a closed pool returned no error")
	}
	if _, err := s.NegativeHolders(ctx, "user"); err == nil {
		t.Error("NegativeHolders on a closed pool returned no error")
	}
	if _, err := s.TotalOutstanding(ctx); err == nil {
		t.Error("TotalOutstanding on a closed pool returned no error")
	}
	if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "r"}); err == nil {
		t.Error("Credit on a closed pool returned no error")
	}
	if err := s.Debit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "r2"}); err == nil {
		t.Error("Debit on a closed pool returned no error")
	}
	if err := s.Adjust(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "r3"}, true); err == nil {
		t.Error("Adjust on a closed pool returned no error")
	}
	if err := s.Hold(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Group: "g"}); err == nil {
		t.Error("Hold on a closed pool returned no error")
	}
	if err := s.Transfer(ctx, ledger.Transfer{From: h, To: ledger.NewHolder("user", "b"), Amount: money.Dollar}); err == nil {
		t.Error("Transfer on a closed pool returned no error")
	}
	if _, err := s.Settle(ctx, ledger.Settlement{Holder: h, Group: "g"}); err == nil {
		t.Error("Settle on a closed pool returned no error")
	}
	if _, err := s.Reverse(ctx, ledger.Reversal{Of: "a", Ref: "b"}); err == nil {
		t.Error("Reverse on a closed pool returned no error")
	}
	if err := s.Within(ctx, func(context.Context, ledger.Ops) error { return nil }); err == nil {
		t.Error("Within on a closed pool returned no error")
	}
	if err := pgledger.Migrate(ctx, pool); err == nil {
		t.Error("Migrate on a closed pool returned no error")
	}
}

// Labels that cannot be encoded are a programming error, and the store must say
// so rather than writing a row with the dimension silently missing.
func TestLabelsRoundTripThroughJSONB(t *testing.T) {
	s := pgledger.New(openTest(t))
	ctx := context.Background()
	h := ledger.NewHolder("project", "labelled")
	want := map[string]string{"project_name": "A study of ünïcode", "team": "core"}
	if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "lab", Labels: want}); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	got, ok, err := s.EntryByRef(ctx, "lab")
	if err != nil || !ok {
		t.Fatalf("EntryByRef = %v, %v", ok, err)
	}
	for k, v := range want {
		if got.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, got.Labels[k], v)
		}
	}
}

// Every write is transactional, so a caller that hands in a transaction which
// has already failed must get an error back, not a silent success. A poisoned
// transaction returns an error on every subsequent statement, and this pins
// that each operation propagates it rather than reporting that it wrote.
func TestBoundOpsPropagateAPoisonedTransaction(t *testing.T) {
	pool := openTest(t)
	s := pgledger.New(pool)
	ctx := context.Background()
	h := ledger.NewHolder("user", "poison@example.test")

	begin := func(t *testing.T) ledger.Ops {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		// Poison it: everything after this errors until rollback.
		if _, err := tx.Exec(ctx, `SELECT * FROM a_table_that_does_not_exist`); err == nil {
			t.Fatal("the poisoning statement unexpectedly succeeded")
		}
		return s.Bind(tx)
	}

	t.Run("credit", func(t *testing.T) {
		if err := begin(t).Credit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "p1"}); err == nil {
			t.Error("Credit reported success inside a failed transaction")
		}
	})
	t.Run("debit", func(t *testing.T) {
		if err := begin(t).Debit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "p2"}); err == nil {
			t.Error("Debit reported success inside a failed transaction")
		}
	})
	t.Run("adjust", func(t *testing.T) {
		if err := begin(t).Adjust(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "p3"}, true); err == nil {
			t.Error("Adjust reported success inside a failed transaction")
		}
	})
	t.Run("hold", func(t *testing.T) {
		if err := begin(t).Hold(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Group: "g"}); err == nil {
			t.Error("Hold reported success inside a failed transaction")
		}
	})
	t.Run("transfer", func(t *testing.T) {
		other := ledger.NewHolder("user", "other@example.test")
		if err := begin(t).Transfer(ctx, ledger.Transfer{From: h, To: other, Amount: money.Dollar, Ref: "p4"}); err == nil {
			t.Error("Transfer reported success inside a failed transaction")
		}
	})
	t.Run("settle", func(t *testing.T) {
		if _, err := begin(t).Settle(ctx, ledger.Settlement{Holder: h, Group: "g"}); err == nil {
			t.Error("Settle reported success inside a failed transaction")
		}
	})
	t.Run("reverse", func(t *testing.T) {
		if _, err := begin(t).Reverse(ctx, ledger.Reversal{Of: "a", Ref: "b"}); err == nil {
			t.Error("Reverse reported success inside a failed transaction")
		}
	})
}

// A ledger.Ops bound to the caller's transaction commits with it, which is the
// whole reason Bind exists: a unit of work can never be recorded without the
// money that guards it.
func TestBindCommitsWithTheCallersTransaction(t *testing.T) {
	pool := openTest(t)
	s := pgledger.New(pool)
	ctx := context.Background()
	h := ledger.NewHolder("project", "bound")
	if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: 10 * money.Dollar, Ref: "seed"}); err != nil {
		t.Fatalf("Credit: %v", err)
	}

	// A caller's transaction that rolls back takes the hold with it.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := s.Bind(tx).Hold(ctx, ledger.Posting{Holder: h, Amount: 4 * money.Dollar, Group: "rolled-back"}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got, _ := s.Available(ctx, h); got != 10*money.Dollar {
		t.Errorf("available after rollback = %s, want $10: the hold outlived its transaction", got.String(money.USD))
	}

	// And one that commits keeps it.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := s.Bind(tx2).Hold(ctx, ledger.Posting{Holder: h, Amount: 4 * money.Dollar, Group: "committed"}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got, _ := s.Available(ctx, h); got != 6*money.Dollar {
		t.Errorf("available after commit = %s, want $6", got.String(money.USD))
	}
}
