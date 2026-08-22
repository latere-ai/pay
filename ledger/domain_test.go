package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/money"
)

func TestHolder(t *testing.T) {
	h := ledger.NewHolder("  User  ", "  alice@example.test  ")
	if h != "user:alice@example.test" {
		t.Errorf("NewHolder = %q, want a lowercased namespace and a trimmed id", h)
	}
	if h.String() != string(h) {
		t.Error("String does not round-trip")
	}
	if h.Namespace() != "user" {
		t.Errorf("Namespace = %q", h.Namespace())
	}
	ns, id, ok := h.Split()
	if !ok || ns != "user" || id != "alice@example.test" {
		t.Errorf("Split = %q, %q, %v", ns, id, ok)
	}
	// An id containing a colon splits at the first one, so "org:a:b" is org "a:b".
	nested := ledger.Holder("org:tenant:1")
	if ns, id, ok := nested.Split(); !ok || ns != "org" || id != "tenant:1" {
		t.Errorf("nested Split = %q, %q, %v", ns, id, ok)
	}
	for _, bad := range []ledger.Holder{"", "nocolon", ":id", "user:", ":"} {
		if bad.Valid() {
			t.Errorf("%q reports Valid", bad)
		}
		if bad.Namespace() != "" {
			t.Errorf("%q has a namespace", bad)
		}
	}
}

func TestKindSignAndValidity(t *testing.T) {
	// The sign is the kind's, not the caller's: this is what stops a negative
	// magnitude from turning a debit into a credit.
	cases := map[ledger.Kind]int{
		ledger.KindCredit:   1,
		ledger.KindRelease:  1,
		ledger.KindDebit:    -1,
		ledger.KindHold:     -1,
		ledger.KindTransfer: 0,
		ledger.KindReverse:  0,
		ledger.KindAdjust:   0,
	}
	for k, want := range cases {
		if got := k.Sign(); got != want {
			t.Errorf("%s.Sign() = %d, want %d", k, got, want)
		}
		if !k.Valid() {
			t.Errorf("%s reports invalid", k)
		}
	}
	if ledger.Kind("nonsense").Valid() {
		t.Error("an unknown kind reports valid")
	}
	if got := ledger.Kind("nonsense").Sign(); got != 0 {
		t.Errorf("unknown kind Sign = %d, want 0", got)
	}
}

func TestRollupRefIsStableAndOrdered(t *testing.T) {
	h := ledger.NewHolder("principal", "abc")
	w := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	a := ledger.RollupRef(h, w, 0)
	if a != ledger.RollupRef(h, w, 0) {
		t.Error("RollupRef is not stable for the same window and sequence")
	}
	if a == ledger.RollupRef(h, w, 1) {
		t.Error("two sequences in one window share a reference")
	}
	if a == ledger.RollupRef(h, w.Add(time.Hour), 0) {
		t.Error("two windows share a reference")
	}
	// The window is normalised to UTC, so a caller in another zone produces the
	// same key for the same instant and does not double-post.
	loc := time.FixedZone("elsewhere", 5*3600)
	if ledger.RollupRef(h, w.In(loc), 0) != a {
		t.Error("RollupRef depends on the caller's time zone")
	}
	if ledger.RollupRef(h, w, -1) == ledger.RollupRef(h, w, 1) {
		t.Error("a negative sequence collides with its positive twin")
	}
}

func TestSettleValidation(t *testing.T) {
	s, ctx := ledger.NewMemStore(), context.Background()
	if _, err := s.Settle(ctx, ledger.Settlement{Holder: "bad", Group: "g"}); !errors.Is(err, ledger.ErrNoHolder) {
		t.Errorf("Settle with a malformed holder = %v, want ErrNoHolder", err)
	}
	if _, err := s.Settle(ctx, ledger.Settlement{Holder: "user:a", Group: "  "}); !errors.Is(err, ledger.ErrNoGroup) {
		t.Errorf("Settle with no group = %v, want ErrNoGroup", err)
	}
	if _, err := s.Settle(ctx, ledger.Settlement{Holder: "user:a", Group: "g", Cost: -1}); !errors.Is(err, ledger.ErrNotPositive) {
		t.Errorf("Settle with a negative cost = %v, want ErrNotPositive", err)
	}
}

func TestTransferValidation(t *testing.T) {
	s, ctx := ledger.NewMemStore(), context.Background()
	cases := []struct {
		name string
		t    ledger.Transfer
		want error
	}{
		{"no source", ledger.Transfer{To: "user:b", Amount: 1}, ledger.ErrNoHolder},
		{"no destination", ledger.Transfer{From: "user:a", Amount: 1}, ledger.ErrNoHolder},
		{"zero amount", ledger.Transfer{From: "user:a", To: "user:b"}, ledger.ErrNotPositive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Transfer(ctx, tc.t); !errors.Is(err, tc.want) {
				t.Errorf("Transfer = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAdjustValidation(t *testing.T) {
	s, ctx := ledger.NewMemStore(), context.Background()
	if err := s.Adjust(ctx, ledger.Posting{Holder: "bad", Amount: 1}, true); !errors.Is(err, ledger.ErrNoHolder) {
		t.Errorf("Adjust with a malformed holder = %v, want ErrNoHolder", err)
	}
	if err := s.Adjust(ctx, ledger.Posting{Holder: "user:a", Amount: -1}, true); !errors.Is(err, ledger.ErrNotPositive) {
		t.Errorf("Adjust with a negative magnitude = %v, want ErrNotPositive", err)
	}
	// An adjustment is idempotent on its reference like every other write.
	h := ledger.Holder("user:a")
	p := ledger.Posting{Holder: h, Amount: 2 * money.Dollar, Ref: "adj"}
	if err := s.Adjust(ctx, p, true); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if err := s.Adjust(ctx, p, true); err != nil {
		t.Fatalf("Adjust again: %v", err)
	}
	got, _ := s.Balance(ctx, h)
	if got != 2*money.Dollar {
		t.Errorf("balance = %s, want $2: a replayed adjustment applied twice", got.String(money.USD))
	}
}

// A statement pages backwards through time, which needs a deterministic clock
// to assert rather than a sleep.
func TestEntriesPagesByTime(t *testing.T) {
	s, ctx := ledger.NewMemStore(), context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	n := 0
	s.SetClock(func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Minute)
	})
	h := ledger.Holder("user:a")
	for i := 0; i < 4; i++ {
		if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: money.Dollar, Ref: string(rune('a' + i))}); err != nil {
			t.Fatalf("Credit: %v", err)
		}
	}
	all, err := s.Entries(ctx, h, ledger.Page{})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d entries", len(all))
	}
	// Everything strictly older than the newest entry: three rows.
	older, err := s.Entries(ctx, h, ledger.Page{Before: all[0].CreatedAt})
	if err != nil {
		t.Fatalf("Entries(before): %v", err)
	}
	if len(older) != 3 {
		t.Errorf("paging before the newest returned %d rows, want 3", len(older))
	}
	if !older[0].CreatedAt.Before(all[0].CreatedAt) {
		t.Error("a paged row is not older than the cursor")
	}
}

// NegativeHolders with no namespace spans all of them, which is the operator's
// whole-platform view.
func TestNegativeHoldersAcrossAllNamespaces(t *testing.T) {
	s, ctx := ledger.NewMemStore(), context.Background()
	if err := s.Debit(ctx, ledger.Posting{Holder: "user:a", Amount: money.Dollar, Ref: "1"}); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	if err := s.Debit(ctx, ledger.Posting{Holder: "project:p", Amount: 2 * money.Dollar, Ref: "2"}); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	got, err := s.NegativeHolders(ctx, "")
	if err != nil {
		t.Fatalf("NegativeHolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d debtors across all namespaces, want 2", len(got))
	}
	if got[0].Holder != "project:p" {
		t.Errorf("first = %s, want the most-owed", got[0].Holder)
	}
}

// A hold whose group later settles releases only what is still outstanding,
// even when the same group was held more than once.
func TestSettleReleasesEveryOpenHoldForTheGroup(t *testing.T) {
	s, ctx := ledger.NewMemStore(), context.Background()
	h := ledger.Holder("project:p")
	if err := s.Credit(ctx, ledger.Posting{Holder: h, Amount: 10 * money.Dollar, Ref: "seed"}); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Hold(ctx, ledger.Posting{Holder: h, Amount: 3 * money.Dollar, Group: "g"}); err != nil {
			t.Fatalf("Hold %d: %v", i, err)
		}
	}
	if got, _ := s.Available(ctx, h); got != 4*money.Dollar {
		t.Fatalf("available with two holds = %s, want $4", got.String(money.USD))
	}
	if ok, err := s.Settle(ctx, ledger.Settlement{Holder: h, Group: "g", Cost: money.Dollar}); err != nil || !ok {
		t.Fatalf("Settle = %v, %v", ok, err)
	}
	if got, _ := s.Available(ctx, h); got != 9*money.Dollar {
		t.Errorf("available after settling = %s, want $9: not every hold was released", got.String(money.USD))
	}
}
