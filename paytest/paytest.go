// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

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

// Option adjusts how RunProviderContract drives an adapter.
type Option func(*config)

type config struct {
	paid func(t *testing.T, ref string) ([]byte, http.Header)
}

// WithPaidDelivery gives the suite a way to make the adapter a delivery it
// must accept: a correctly signed, paid delivery whose Event.Ref is ref.
//
// The suite cannot sign for an adapter it knows nothing about, so without this
// option it can only check that an unsigned delivery is refused. With it, the
// suite also asserts that the delivery parses to a pay.KindPaid event carrying
// ref, and that the same body with one byte changed is refused with
// pay.ErrBadSignature. An adapter whose verification is wrong in either
// direction, accepting a forgery or refusing its own processor, fails.
//
// The function is called on a provider built by the same Factory, so it may
// read the secret from the test's own harness. It returns the body and the
// headers to pass to ParseWebhook, and should call t.Fatal if it cannot build
// the delivery. MemProvider needs no option: the suite builds its delivery.
func WithPaidDelivery(f func(t *testing.T, ref string) ([]byte, http.Header)) Option {
	return func(c *config) { c.paid = f }
}

// paidRef is the reference the suite's paid delivery carries.
const paidRef = "pi_contract_paid"

// RunProviderContract drives a provider through every operation its declared
// capabilities imply, and asserts that everything it does not declare returns
// pay.ErrUnsupported.
//
// caps is what the adapter claims. The suite cross-checks the claim against
// Has, so an adapter cannot pass by declaring nothing.
//
// The factory must configure the webhook secret: a provider that verifies
// nothing accepts the suite's unsigned delivery and fails.
func RunProviderContract(t *testing.T, newProvider Factory, caps []pay.Capability, opts ...Option) {
	t.Helper()
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}

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

	t.Run("an amount that is not positive is refused", func(t *testing.T) {
		ctx := context.Background()
		for name, amount := range map[string]money.Micro{"zero": 0, "negative": -5 * money.Dollar} {
			if slices.Contains(caps, pay.CapCheckout) {
				p := newProvider(t)
				params := validCheckout()
				params.Amount = amount
				if _, err := p.CreateCheckout(ctx, params); err == nil {
					t.Errorf("CreateCheckout accepted a %s amount", name)
				}
			}
			if slices.Contains(caps, pay.CapSavedMethod) {
				p := newProvider(t)
				ref, err := p.EnsureCustomer(ctx, "buyer@example.test", nil)
				if err != nil {
					t.Fatalf("EnsureCustomer: %v", err)
				}
				_, err = p.ChargeSaved(ctx, pay.SavedChargeParams{
					Customer:       ref,
					Amount:         amount,
					Currency:       money.USD,
					IdempotencyKey: "contract-" + name,
				})
				if err == nil {
					t.Errorf("ChargeSaved accepted a %s amount", name)
				}
			}
		}
	})

	t.Run("a delivery that does not authenticate is refused", func(t *testing.T) {
		p := newProvider(t)
		_, err := p.ParseWebhook(forgery(p), http.Header{})
		if err == nil {
			t.Fatal("ParseWebhook accepted a delivery with no signature; an adapter that verifies nothing credits whatever anyone posts (does the factory configure a webhook secret?)")
		}
		if !errors.Is(err, pay.ErrBadSignature) && !errors.Is(err, pay.ErrUnconfigured) {
			t.Errorf("ParseWebhook with no signature = %v, want ErrBadSignature or ErrUnconfigured", err)
		}
	})

	t.Run("a good paid delivery is accepted and a tampered one is refused", func(t *testing.T) {
		p := newProvider(t)
		body, header, ok := paidDelivery(t, p, cfg)
		if !ok {
			t.Skip("no way to sign a delivery for this adapter; pass paytest.WithPaidDelivery")
		}
		ev, err := p.ParseWebhook(body, header)
		if err != nil {
			t.Fatalf("ParseWebhook on a correctly signed paid delivery: %v", err)
		}
		if ev.Kind != pay.KindPaid {
			t.Errorf("Kind = %q, want KindPaid", ev.Kind)
		}
		if ev.Ref != paidRef {
			t.Errorf("Ref = %q, want %q", ev.Ref, paidRef)
		}
		if cfg.paid == nil {
			// MemProvider signs the header, not the body, so there is no
			// body-bound signature to break.
			return
		}
		tampered := slices.Clone(body)
		tampered[len(tampered)/2] ^= 1
		if _, err := p.ParseWebhook(tampered, header); !errors.Is(err, pay.ErrBadSignature) {
			t.Errorf("ParseWebhook on a tampered body = %v, want ErrBadSignature", err)
		}
	})

	t.Run("an unmodeled event reduces to KindIgnored, not an error", func(t *testing.T) {
		p := newProvider(t)
		if p.Name() != pay.Memory {
			t.Skip("an unmodeled event can be signed only for MemProvider")
		}
		ev, err := p.ParseWebhook(pay.MemEvent(pay.KindIgnored, "", 0, nil), signedHeader(p))
		if err != nil {
			t.Fatalf("ParseWebhook on an unmodeled event returned an error: %v", err)
		}
		if ev.Kind != pay.KindIgnored {
			t.Errorf("Kind = %q, want KindIgnored so the delivery is acknowledged and dropped", ev.Kind)
		}
	})
}

// paidDelivery builds the delivery the adapter must accept: from the option
// when there is one, by itself for MemProvider, and not at all otherwise.
func paidDelivery(t *testing.T, p pay.Provider, cfg config) ([]byte, http.Header, bool) {
	t.Helper()
	if cfg.paid != nil {
		body, header := cfg.paid(t, paidRef)
		return body, header, true
	}
	if p.Name() == pay.Memory {
		return pay.MemEvent(pay.KindPaid, paidRef, 20*money.Dollar, nil), signedHeader(p), true
	}
	return nil, nil, false
}

// forgery is a paid delivery nobody signed.
func forgery(p pay.Provider) []byte {
	if p.Name() == pay.Memory {
		return pay.MemEvent(pay.KindPaid, "pi_forged", 1000*money.Dollar, nil)
	}
	return []byte(`{"kind":"paid"}`)
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

// signedHeader is the header a MemProvider accepts.
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
