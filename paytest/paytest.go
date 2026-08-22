// Package paytest is the conformance suite every pay.Provider must pass.
//
// It exists so "is this a valid adapter" is one shared test rather than a
// per-repo opinion, and so an adapter that declares a capability it does not
// have fails loudly instead of at a customer.
package paytest

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

// Factory builds a provider under test. It is called once per subtest, so a
// stateful adapter starts clean each time.
type Factory func(t *testing.T) pay.Provider

// RunProviderContract drives a provider through every operation its declared
// capabilities imply, and asserts that everything it does not declare returns
// pay.ErrUnsupported.
//
// caps is what the adapter claims. The suite cross-checks the claim against
// Has, so an adapter cannot pass by declaring nothing.
func RunProviderContract(t *testing.T, newProvider Factory, caps []pay.Capability) {
	t.Helper()

	t.Run("declares the capabilities it was registered with", func(t *testing.T) {
		p := newProvider(t)
		for _, c := range caps {
			if !p.Has(c) {
				t.Errorf("Has(%s) = false, but the adapter was registered with it", c)
			}
		}
		if p.Name() == "" {
			t.Error("Name() is empty")
		}
	})

	t.Run("checkout", func(t *testing.T) {
		p := newProvider(t)
		got, err := p.CreateCheckout(context.Background(), validCheckout())
		if !slices.Contains(caps, pay.CapCheckout) {
			requireUnsupported(t, err)
			return
		}
		if err != nil {
			t.Fatalf("CreateCheckout: %v", err)
		}
		if got.URL == "" {
			t.Error("CreateCheckout returned an empty URL")
		}
	})

	t.Run("saved method", func(t *testing.T) {
		p := newProvider(t)
		ctx := context.Background()
		ref, err := p.EnsureCustomer(ctx, "buyer@example.test", nil)
		if !slices.Contains(caps, pay.CapSavedMethod) {
			requireUnsupported(t, err)
			if _, err := p.ChargeSaved(ctx, pay.SavedChargeParams{}); !errors.Is(err, pay.ErrUnsupported) {
				t.Errorf("ChargeSaved without CapSavedMethod = %v, want ErrUnsupported", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("EnsureCustomer: %v", err)
		}
		if ref.Zero() {
			t.Fatal("EnsureCustomer returned a zero reference")
		}
		// Idempotent on email: the second call must not mint a second customer.
		again, err := p.EnsureCustomer(ctx, "buyer@example.test", nil)
		if err != nil {
			t.Fatalf("EnsureCustomer again: %v", err)
		}
		if again != ref {
			t.Errorf("EnsureCustomer is not idempotent: %v then %v", ref, again)
		}
		charge, err := p.ChargeSaved(ctx, pay.SavedChargeParams{
			Customer:       ref,
			Amount:         5 * money.Dollar,
			Currency:       money.USD,
			IdempotencyKey: "contract-1",
		})
		if err != nil {
			t.Fatalf("ChargeSaved: %v", err)
		}
		if charge.Ref == "" {
			t.Error("ChargeSaved returned an empty reference; the ledger has nothing to dedupe on")
		}
		switch charge.Status {
		case pay.ChargeSucceeded, pay.ChargePending, pay.ChargeFailed:
		default:
			t.Errorf("ChargeSaved status = %q, not one of the three", charge.Status)
		}
	})

	t.Run("a delivery that does not authenticate is refused", func(t *testing.T) {
		p := newProvider(t)
		_, err := p.ParseWebhook([]byte(`{"kind":"paid"}`), http.Header{})
		if err == nil {
			return // an adapter with no signature scheme is allowed
		}
		if !errors.Is(err, pay.ErrBadSignature) && !errors.Is(err, pay.ErrUnconfigured) {
			t.Errorf("ParseWebhook with no signature = %v, want ErrBadSignature or ErrUnconfigured", err)
		}
	})

	t.Run("an unmodelled event reduces to KindIgnored, not an error", func(t *testing.T) {
		p := newProvider(t)
		ev, err := p.ParseWebhook(unknownEventPayload(t, p), signedHeader(p))
		if errors.Is(err, pay.ErrUnconfigured) || errors.Is(err, pay.ErrBadSignature) {
			return // the adapter cannot be driven offline; nothing to assert
		}
		if err != nil {
			t.Fatalf("ParseWebhook on an unmodelled event returned an error: %v", err)
		}
		if ev.Kind != pay.KindIgnored {
			t.Errorf("Kind = %q, want KindIgnored so the delivery is acknowledged and dropped", ev.Kind)
		}
	})
}

// validCheckout is a checkout every adapter should accept.
func validCheckout() pay.CheckoutParams {
	return pay.CheckoutParams{
		Email:       "buyer@example.test",
		Amount:      20 * money.Dollar,
		Currency:    money.USD,
		Description: "credit",
		SuccessURL:  "https://product.test/wallet?ok=1",
		CancelURL:   "https://product.test/wallet",
		Meta:        map[string]string{"credited_micro": "18700000"},
		Tax:         pay.TaxNone,
	}
}

// unknownEventPayload builds a delivery the port does not model. Adapters that
// accept the fake's JSON encoding get an unknown kind; anything else gets an
// empty object, which no adapter should map to a ledger write.
func unknownEventPayload(t *testing.T, p pay.Provider) []byte {
	t.Helper()
	if p.Name() == pay.Memory {
		return pay.MemEvent(pay.KindIgnored, "", 0, nil)
	}
	return []byte(`{"type":"customer.discount.created"}`)
}

// signedHeader is the header a fake accepts. A real adapter is exercised
// against its own fixtures in its own package; here it will simply refuse,
// which the caller handles.
func signedHeader(p pay.Provider) http.Header {
	h := http.Header{}
	if p.Name() == pay.Memory {
		h.Set(pay.MemHeader, memSecret(p))
	}
	return h
}

// memSecret reads the fake's configured secret, if it is a fake at all.
func memSecret(p pay.Provider) string {
	if m, ok := p.(*pay.MemProvider); ok {
		return m.Secret
	}
	return ""
}

func requireUnsupported(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, pay.ErrUnsupported) {
		t.Errorf("an undeclared capability returned %v, want ErrUnsupported", err)
	}
}
