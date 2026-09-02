// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package paytest_test

import (
	"context"
	"net/http"
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

func (b *bare) CreateCheckout(context.Context, pay.CheckoutParams) (pay.Checkout, error) {
	if !b.Has(pay.CapCheckout) {
		return pay.Checkout{}, pay.ErrUnsupported
	}
	return pay.Checkout{URL: "https://bare.test/pay", SessionID: "cs_bare"}, nil
}

func (b *bare) EnsureCustomer(context.Context, string, map[string]string) (pay.CustomerRef, error) {
	if !b.Has(pay.CapSavedMethod) {
		return pay.CustomerRef{}, pay.ErrUnsupported
	}
	return pay.CustomerRef{Provider: b.Name(), ID: "cus_bare"}, nil
}

func (b *bare) ChargeSaved(context.Context, pay.SavedChargeParams) (pay.Charge, error) {
	if !b.Has(pay.CapSavedMethod) {
		return pay.Charge{}, pay.ErrUnsupported
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
