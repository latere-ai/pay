// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pay_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
	"latere.ai/x/pay/paytest"
)

func TestMemProviderSatisfiesTheContract(t *testing.T) {
	paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
		return &pay.MemProvider{Secret: "shh"}
	}, []pay.Capability{pay.CapCheckout, pay.CapRefund})
}

// The same fake, declaring the saved-method capability, must pass the branch of
// the contract that exercises auto-recharge.
func TestMemProviderWithSavedMethodsSatisfiesTheContract(t *testing.T) {
	caps := []pay.Capability{pay.CapCheckout, pay.CapRefund, pay.CapSavedMethod}
	paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
		return &pay.MemProvider{Secret: "shh", Caps: caps}
	}, caps)
}

func TestCapabilityGating(t *testing.T) {
	ctx := context.Background()
	// No saved-method capability: both operations refuse rather than pretending.
	m := &pay.MemProvider{}
	if _, err := m.EnsureCustomer(ctx, "a@b.test", nil); !errors.Is(err, pay.ErrUnsupported) {
		t.Errorf("EnsureCustomer = %v, want ErrUnsupported", err)
	}
	if _, err := m.ChargeSaved(ctx, pay.SavedChargeParams{}); !errors.Is(err, pay.ErrUnsupported) {
		t.Errorf("ChargeSaved = %v, want ErrUnsupported", err)
	}
	// An adapter declaring nothing cannot even open a page.
	none := &pay.MemProvider{Caps: []pay.Capability{}}
	if _, err := none.CreateCheckout(ctx, pay.CheckoutParams{}); !errors.Is(err, pay.ErrUnsupported) {
		t.Errorf("CreateCheckout = %v, want ErrUnsupported", err)
	}
	if none.Has(pay.CapCheckout) {
		t.Error("an adapter with an explicit empty capability set declared checkout")
	}
}

func TestUnconfiguredRefusesEverything(t *testing.T) {
	ctx := context.Background()
	m := &pay.MemProvider{Unconfigured: true, Caps: []pay.Capability{pay.CapCheckout, pay.CapSavedMethod}}
	if _, err := m.CreateCheckout(ctx, pay.CheckoutParams{}); !errors.Is(err, pay.ErrUnconfigured) {
		t.Errorf("CreateCheckout = %v, want ErrUnconfigured", err)
	}
	if _, err := m.EnsureCustomer(ctx, "a@b.test", nil); !errors.Is(err, pay.ErrUnconfigured) {
		t.Errorf("EnsureCustomer = %v, want ErrUnconfigured", err)
	}
	if _, err := m.ChargeSaved(ctx, pay.SavedChargeParams{}); !errors.Is(err, pay.ErrUnconfigured) {
		t.Errorf("ChargeSaved = %v, want ErrUnconfigured", err)
	}
	if _, err := m.ParseWebhook(nil, http.Header{}); !errors.Is(err, pay.ErrUnconfigured) {
		t.Errorf("ParseWebhook = %v, want ErrUnconfigured", err)
	}
}

func TestCheckoutRecordsParamsAndMintsDistinctSessions(t *testing.T) {
	m := &pay.MemProvider{URL: "https://pay.test/x"}
	ctx := context.Background()
	for range 2 {
		if _, err := m.CreateCheckout(ctx, pay.CheckoutParams{Amount: 5 * money.Dollar, Currency: money.USD}); err != nil {
			t.Fatalf("CreateCheckout: %v", err)
		}
	}
	got := m.Checkouts()
	if len(got) != 2 {
		t.Fatalf("recorded %d checkouts, want 2", len(got))
	}
	if got[0].Amount != 5*money.Dollar {
		t.Errorf("recorded amount = %d", got[0].Amount)
	}
	// The returned slice is a copy: mutating it must not corrupt the fake.
	got[0].Amount = 0
	if m.Checkouts()[0].Amount != 5*money.Dollar {
		t.Error("Checkouts() handed out the backing array")
	}
	a, _ := m.CreateCheckout(ctx, pay.CheckoutParams{})
	b, _ := m.CreateCheckout(ctx, pay.CheckoutParams{})
	if a.SessionID == b.SessionID {
		t.Errorf("two checkouts share a session id %q", a.SessionID)
	}
	if a.URL != "https://pay.test/x" {
		t.Errorf("URL = %q, want the configured one", a.URL)
	}
}

func TestChargeSavedOutcomes(t *testing.T) {
	caps := []pay.Capability{pay.CapSavedMethod}
	ctx := context.Background()

	pending := &pay.MemProvider{Caps: caps, ChargeResult: pay.ChargePending}
	got, err := pending.ChargeSaved(ctx, pay.SavedChargeParams{IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("ChargeSaved: %v", err)
	}
	if got.Status != pay.ChargePending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if len(pending.Charges()) != 1 {
		t.Error("the attempt was not recorded")
	}

	// A decline is an error, not a failed-status result, so a caller cannot
	// mistake it for something worth retrying.
	declined := &pay.MemProvider{Caps: caps, ChargeErr: pay.ErrDeclined}
	if _, err := declined.ChargeSaved(ctx, pay.SavedChargeParams{}); !errors.Is(err, pay.ErrDeclined) {
		t.Errorf("ChargeSaved = %v, want ErrDeclined", err)
	}
	// Even a refused attempt is recorded, which is what makes a retry loop
	// visible in a test.
	if len(declined.Charges()) != 1 {
		t.Error("a declined attempt was not recorded")
	}
	// Charges() is a copy too.
	c := declined.Charges()
	c[0].Amount = 999
	if declined.Charges()[0].Amount == 999 {
		t.Error("Charges() handed out the backing array")
	}
}

func TestParseWebhookSignatureAndDecoding(t *testing.T) {
	m := &pay.MemProvider{Secret: "shh"}
	signed := http.Header{}
	signed.Set(pay.MemHeader, "shh")

	if _, err := m.ParseWebhook(pay.MemEvent(pay.KindPaid, "pi_1", money.Dollar, nil), http.Header{}); !errors.Is(err, pay.ErrBadSignature) {
		t.Errorf("unsigned delivery = %v, want ErrBadSignature", err)
	}
	ev, err := m.ParseWebhook(pay.MemEvent(pay.KindPaid, "pi_1", 20*money.Dollar, map[string]string{"credited_micro": "18700000"}), signed)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != pay.KindPaid || ev.Ref != "pi_1" || ev.Gross != 20*money.Dollar {
		t.Errorf("event = %+v", ev)
	}
	if ev.Provider != pay.Memory {
		t.Errorf("Provider = %q, want the adapter's own name", ev.Provider)
	}
	if ev.Meta["credited_micro"] != "18700000" {
		t.Error("session metadata did not survive the round trip")
	}
	if len(ev.Raw) == 0 {
		t.Error("Raw was not populated")
	}
	if _, err := m.ParseWebhook([]byte("{not json"), signed); err == nil {
		t.Error("malformed payload decoded without error")
	}
	// An empty secret accepts anything, which is what an offline test wants.
	open := &pay.MemProvider{}
	if _, err := open.ParseWebhook(pay.MemEvent(pay.KindPaid, "x", 0, nil), http.Header{}); err != nil {
		t.Errorf("open fake refused an unsigned delivery: %v", err)
	}
}

func TestCustomerRefZero(t *testing.T) {
	if !(pay.CustomerRef{}).Zero() {
		t.Error("the zero CustomerRef does not report Zero")
	}
	if (pay.CustomerRef{Provider: pay.Stripe, ID: "cus_1"}).Zero() {
		t.Error("a populated CustomerRef reports Zero")
	}
}
