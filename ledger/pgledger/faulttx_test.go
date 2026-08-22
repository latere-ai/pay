package pgledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/ledger/pgledger"
	"latere.ai/x/pay/money"
)

// errFault is the injected failure.
var errFault = errors.New("injected database fault")

// faultTx is a pgx.Tx that behaves normally for the first n statements and
// then fails every one after.
//
// Bind takes the pgx.Tx interface, which is what makes this possible without
// touching the driver. It exists to reach the error branches that sit *after*
// a successful statement: a settlement whose debit lands and whose hold lookup
// then fails, a reversal that finds its original and cannot read the balance.
// Those are the paths where a half-applied write would be worst, so leaving
// them unexercised is not acceptable in a ledger.
type faultTx struct {
	pgx.Tx
	remaining int
}

func (f *faultTx) step() bool {
	if f.remaining <= 0 {
		return false
	}
	f.remaining--
	return true
}

func (f *faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !f.step() {
		return pgconn.CommandTag{}, errFault
	}
	return f.Tx.Exec(ctx, sql, args...)
}

func (f *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !f.step() {
		return errRow{}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

// errRow is a pgx.Row that always fails to scan.
type errRow struct{}

func (errRow) Scan(...any) error { return errFault }

// TestOperationsPropagateAFaultAtEveryStep walks the fault point outward
// through each operation. Every position must produce an error, and none may
// report success.
func TestOperationsPropagateAFaultAtEveryStep(t *testing.T) {
	pool := openTest(t)
	s := pgledger.New(pool)
	ctx := context.Background()
	h := ledger.NewHolder("project", "fault")
	other := ledger.NewHolder("user", "fault")

	// Seed outside the fault harness so the operations have something to act on.
	if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: 100 * money.Dollar, Ref: "seed"}); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := s.Hold(ctx, ledger.Posting{Holder: h, Amount: 10 * money.Dollar, Group: "held"}); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	run := func(t *testing.T, allow int, fn func(ledger.Ops) error) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		return fn(s.Bind(&faultTx{Tx: tx, remaining: allow}))
	}

	cases := []struct {
		name  string
		steps int // how many statements each operation issues before it is done
		call  func(ledger.Ops) error
	}{
		{"credit", 1, func(o ledger.Ops) error {
			return o.Credit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "f-credit"})
		}},
		{"debit", 1, func(o ledger.Ops) error {
			return o.Debit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "f-debit"})
		}},
		{"adjust", 1, func(o ledger.Ops) error {
			return o.Adjust(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: "f-adj"}, true)
		}},
		{"hold", 3, func(o ledger.Ops) error {
			return o.Hold(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Group: "f-hold"})
		}},
		{"transfer", 4, func(o ledger.Ops) error {
			return o.Transfer(ctx, ledger.Transfer{From: h, To: other, Amount: money.Dollar, Ref: "f-mv"})
		}},
		{"settle", 3, func(o ledger.Ops) error {
			_, err := o.Settle(ctx, ledger.Settlement{Holder: h, Group: "held", Cost: money.Dollar})
			return err
		}},
		{"reverse", 5, func(o ledger.Ops) error {
			_, err := o.Reverse(ctx, ledger.Reversal{Of: "seed", Ref: "f-rev"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for allow := 0; allow < tc.steps; allow++ {
				err := run(t, allow, tc.call)
				if err == nil {
					t.Errorf("failing at statement %d reported success", allow+1)
				}
			}
		})
	}
}

// A settlement whose debit lands and whose release then fails must report the
// failure, so the caller's transaction rolls the debit back too rather than
// charging for work whose hold is still outstanding.
func TestSettleReportsAFailureAfterItsDebitLands(t *testing.T) {
	pool := openTest(t)
	s := pgledger.New(pool)
	ctx := context.Background()
	h := ledger.NewHolder("project", "half")
	if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: 10 * money.Dollar, Ref: "seed"}); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := s.Hold(ctx, ledger.Posting{Holder: h, Amount: 4 * money.Dollar, Group: "g"}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Two statements land (the debit insert and the held lookup); the release
	// insert is the third and fails.
	ops := s.Bind(&faultTx{Tx: tx, remaining: 2})
	if _, err := ops.Settle(ctx, ledger.Settlement{Holder: h, Group: "g", Cost: money.Dollar}); err == nil {
		t.Error("Settle reported success although its release failed")
	}
	_ = tx.Rollback(ctx)

	// The rollback undid everything: the group is still open and still held.
	if got, _ := s.Available(ctx, h); got != 6*money.Dollar {
		t.Errorf("available = %s, want $6: the failed settlement left state behind", got.String(money.USD))
	}
	if ok, err := s.Settle(ctx, ledger.Settlement{Holder: h, Group: "g", Cost: money.Dollar}); err != nil || !ok {
		t.Errorf("re-settling after a rollback = %v, %v, want it to succeed", ok, err)
	}
}
