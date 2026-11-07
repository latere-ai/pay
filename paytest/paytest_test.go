// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package paytest_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/paytest"
)

// bare is a minimal adapter that is not the in-memory fake. It exists so the
// contract suite's non-fake branches are exercised: the suite must work against
// an adapter it knows nothing about, which is the whole point of it.
type bare struct {
	caps []pay.Capability
}

func (b *bare) Name() pay.Name { return pay.Name("bare") }

func (b *bare) Has(c pay.Capability) bool {
	return slices.Contains(b.caps, c)
}

func (b *bare) CreateCheckout(_ context.Context, p pay.CheckoutParams) (pay.Checkout, error) {
	if !b.Has(pay.CapCheckout) {
		return pay.Checkout{}, pay.ErrUnsupported
	}
	if p.Amount <= 0 {
		return pay.Checkout{}, pay.ErrNotPositive
	}
	return pay.Checkout{URL: "https://bare.test/pay", SessionID: "cs_bare"}, nil
}

func (b *bare) EnsureCustomer(context.Context, string, map[string]string) (pay.CustomerRef, error) {
	if !b.Has(pay.CapSavedMethod) {
		return pay.CustomerRef{}, pay.ErrUnsupported
	}
	return pay.CustomerRef{Provider: b.Name(), ID: "cus_bare"}, nil
}

func (b *bare) ChargeSaved(_ context.Context, p pay.SavedChargeParams) (pay.Charge, error) {
	if !b.Has(pay.CapSavedMethod) {
		return pay.Charge{}, pay.ErrUnsupported
	}
	if p.Amount <= 0 {
		return pay.Charge{}, pay.ErrNotPositive
	}
	return pay.Charge{Ref: "pi_bare", Status: pay.ChargeSucceeded}, nil
}

// ParseWebhook models a real adapter: it refuses an unsigned delivery and
// reduces anything it does not model to KindIgnored.
func (b *bare) ParseWebhook(payload []byte, h http.Header) (pay.Event, error) {
	if h.Get("X-Bare-Signature") == "" && len(payload) > 0 && payload[0] == '{' && h.Get("X-Bare-Allow") == "" {
		return pay.Event{}, pay.ErrBadSignature
	}
	return pay.Event{Kind: pay.KindIgnored, Provider: b.Name()}, nil
}

// An adapter that can only open a page must still pass, with every undeclared
// capability answering ErrUnsupported.
func TestContractAgainstACheckoutOnlyAdapter(t *testing.T) {
	caps := []pay.Capability{pay.CapCheckout}
	paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
		return &bare{caps: caps}
	}, caps)
}

// And one that does everything.
func TestContractAgainstAFullAdapter(t *testing.T) {
	caps := []pay.Capability{pay.CapCheckout, pay.CapSavedMethod, pay.CapRefund}
	paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
		return &bare{caps: caps}
	}, caps)
}

// An adapter that declares nothing at all still has to behave: every operation
// refuses, and the suite passes because the refusals are the correct answer.
func TestContractAgainstAnAdapterThatDeclaresNothing(t *testing.T) {
	paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
		return &bare{}
	}, nil)
}

// passes runs the suite on an adapter and reports whether it passed, without
// failing the calling test. The suite takes a *testing.T, so the only way to
// watch it fail is to run it as a test of its own; its report goes to the null
// device because it is the expected outcome here.
func passes(run func(t *testing.T)) bool {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		panic("paytest: open " + os.DevNull + ": " + err.Error())
	}
	stdout := os.Stdout
	os.Stdout = null
	defer func() {
		os.Stdout = stdout
		if err := null.Close(); err != nil {
			panic("paytest: close " + os.DevNull + ": " + err.Error())
		}
	}()
	return testing.RunTests(
		func(string, string) (bool, error) { return true, nil },
		[]testing.InternalTest{{Name: "contract", F: run}},
	)
}

// signing is an adapter with a real signature scheme: HMAC-free, but bound to
// the body, so a changed byte breaks it the way a processor's signature does.
type signing struct {
	bare
	// verify decides whether a delivery authenticates. Nil means the adapter
	// verifies nothing.
	verify func(body []byte, h http.Header) bool
	// refuseAll makes the adapter refuse even a correctly signed delivery.
	refuseAll bool
}

const signingHeader = "X-Signing"

func sign(body []byte) string { return string(body) + "|ok" }

func (s *signing) ParseWebhook(body []byte, h http.Header) (pay.Event, error) {
	if s.refuseAll {
		return pay.Event{}, pay.ErrBadSignature
	}
	if s.verify != nil && !s.verify(body, h) {
		return pay.Event{}, pay.ErrBadSignature
	}
	return pay.Event{Kind: pay.KindPaid, Provider: s.Name(), Ref: "pi_contract_paid"}, nil
}

func strict(body []byte, h http.Header) bool { return h.Get(signingHeader) == sign(body) }

func paidFor(t *testing.T, ref string) ([]byte, http.Header) {
	t.Helper()
	body := []byte(`{"type":"paid","ref":"` + ref + `"}`)
	h := http.Header{}
	h.Set(signingHeader, sign(body))
	return body, h
}

var checkoutCaps = []pay.Capability{pay.CapCheckout, pay.CapSavedMethod}

func TestContractRejectsAnAdapterThatAcceptsForgeries(t *testing.T) {
	// An in-memory fake with no secret verifies nothing.
	if passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider { return &pay.MemProvider{} },
			[]pay.Capability{pay.CapCheckout, pay.CapRefund})
	}) {
		t.Error("the suite passed a MemProvider with no secret")
	}
	// So does an adapter that returns a paid event for any bytes at all.
	if passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
			return &signing{caps: checkoutCaps}
		}, checkoutCaps, paytest.WithPaidDelivery(paidFor))
	}) {
		t.Error("the suite passed an adapter that verifies nothing")
	}
	// An adapter that checks the header but not the body accepts a tampered
	// delivery.
	if passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
			return &signing{caps: checkoutCaps, verify: func(_ []byte, h http.Header) bool {
				return h.Get(signingHeader) != ""
			}}
		}, checkoutCaps, paytest.WithPaidDelivery(paidFor))
	}) {
		t.Error("the suite passed an adapter whose signature does not cover the body")
	}
	// The same suite passes an adapter that verifies properly.
	if !passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
			return &signing{caps: checkoutCaps, verify: strict}
		}, checkoutCaps, paytest.WithPaidDelivery(paidFor))
	}) {
		t.Error("the suite failed an adapter that verifies the body")
	}
}

func TestContractRequiresAnAdapterToAcceptAGoodDelivery(t *testing.T) {
	// An adapter that refuses everything passes "refuses a forgery" trivially.
	if passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
			return &signing{caps: checkoutCaps, refuseAll: true}
		}, checkoutCaps, paytest.WithPaidDelivery(paidFor))
	}) {
		t.Error("the suite passed an adapter that refuses its own processor's delivery")
	}
	// The in-memory fake gets its delivery without the option.
	if passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
			return &refusingMem{Secret: "shh"}
		}, []pay.Capability{pay.CapCheckout, pay.CapRefund})
	}) {
		t.Error("the suite passed a MemProvider that refuses a signed delivery")
	}
	// A paid delivery that parses to the wrong kind or reference also fails.
	if passes(func(t *testing.T) {
		paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
			return &wrongRef{signing{caps: checkoutCaps, verify: strict}}
		}, checkoutCaps, paytest.WithPaidDelivery(paidFor))
	}) {
		t.Error("the suite passed an adapter that reports a different reference")
	}
}

// refusingMem is a MemProvider whose webhook refuses even a signed delivery.
type refusingMem struct{ pay.MemProvider }

func (*refusingMem) ParseWebhook([]byte, http.Header) (pay.Event, error) {
	return pay.Event{}, pay.ErrBadSignature
}

// wrongRef verifies correctly and then reports someone else's reference.
type wrongRef struct{ signing }

func (w *wrongRef) ParseWebhook(body []byte, h http.Header) (pay.Event, error) {
	ev, err := w.signing.ParseWebhook(body, h)
	ev.Ref = "pi_other"
	return ev, err
}

// lax is bare without the amount checks.
type lax struct{ signing }

func (l *lax) CreateCheckout(ctx context.Context, p pay.CheckoutParams) (pay.Checkout, error) {
	if !l.Has(pay.CapCheckout) {
		return pay.Checkout{}, pay.ErrUnsupported
	}
	return pay.Checkout{URL: "https://lax.test/pay"}, nil
}

func (l *lax) ChargeSaved(ctx context.Context, p pay.SavedChargeParams) (pay.Charge, error) {
	if !l.Has(pay.CapSavedMethod) {
		return pay.Charge{}, pay.ErrUnsupported
	}
	return pay.Charge{Ref: "pi_lax", Status: pay.ChargeSucceeded}, nil
}

func TestContractRequiresAnAdapterToRefuseANonPositiveAmount(t *testing.T) {
	// Each declared capability is asked on its own, so the adapter declares
	// one at a time and the other cannot be the reason the suite failed.
	for _, caps := range [][]pay.Capability{{pay.CapCheckout}, {pay.CapSavedMethod}} {
		if passes(func(t *testing.T) {
			paytest.RunProviderContract(t, func(*testing.T) pay.Provider {
				return &lax{signing{caps: caps, verify: strict}}
			}, caps, paytest.WithPaidDelivery(paidFor))
		}) {
			t.Errorf("the suite passed an adapter declaring %v that accepts an amount of nothing", caps)
		}
	}
}
