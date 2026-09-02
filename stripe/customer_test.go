// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package stripe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	stripe "github.com/stripe/stripe-go/v85"

	"latere.ai/x/pay"
)

const (
	customersPath = "/v1/customers"
	searchPath    = "/v1/customers/search"
)

// searchResult is Stripe's search envelope around zero or more customers.
func searchResult(data ...map[string]any) map[string]any {
	if data == nil {
		data = []map[string]any{}
	}
	return map[string]any{"object": "search_result", "url": searchPath, "has_more": false, "data": data}
}

func TestEnsureCustomer_ReturnsTheOneTheSearchFound(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodGet, searchPath, searchResult(map[string]any{
		"id": "cus_found", "object": "customer", "email": "buyer@example.test",
	}))
	a := newAdapter(t, s)

	got, err := a.EnsureCustomer(context.Background(), "buyer@example.test", nil)
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if want := (pay.CustomerRef{Provider: pay.Stripe, ID: "cus_found"}); got != want {
		t.Errorf("ref = %+v, want %+v", got, want)
	}
	if n := len(s.callsTo(http.MethodPost, customersPath)); n != 0 {
		t.Errorf("a second customer was created for an email that already had one: %d creates", n)
	}
	if q := s.calledOnce(http.MethodGet, searchPath).form.Get("query"); q != `email:'buyer@example.test'` {
		t.Errorf("query = %q", q)
	}
}

func TestEnsureCustomer_CreatesWhenTheSearchFindsNothing(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodGet, searchPath, searchResult())
	s.json(http.MethodPost, customersPath, map[string]any{"id": "cus_new", "object": "customer"})
	a := newAdapter(t, s)

	got, err := a.EnsureCustomer(context.Background(), "buyer@example.test", map[string]string{"tenant": "acme"})
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if got.ID != "cus_new" {
		t.Errorf("ID = %q, want cus_new", got.ID)
	}
	create := s.calledOnce(http.MethodPost, customersPath)
	if e := create.form.Get("email"); e != "buyer@example.test" {
		t.Errorf("email = %q", e)
	}
	if m := create.form.Get("metadata[tenant]"); m != "acme" {
		t.Errorf("metadata[tenant] = %q, want acme", m)
	}
	// Search is eventually consistent, so a retry seconds later reaches the
	// create. The key is what stops that becoming a second customer.
	if create.idempotencyKey == "" {
		t.Fatal("the create carried no Idempotency-Key; a retry would duplicate the customer")
	}
	if !strings.HasPrefix(create.idempotencyKey, "pay-customer-") {
		t.Errorf("Idempotency-Key = %q, want a derived pay-customer- key", create.idempotencyKey)
	}
	if strings.Contains(create.idempotencyKey, "buyer@example.test") {
		t.Errorf("Idempotency-Key %q leaks the address into Stripe's request log", create.idempotencyKey)
	}
}

func TestEnsureCustomer_TheCreateKeyIsStableAcrossProcesses(t *testing.T) {
	// Derived from the email, not random, so a retry from a different process
	// or after a restart still collapses onto the first customer.
	if customerKey("buyer@example.test") != customerKey("Buyer@Example.Test") {
		t.Error("the key depends on the address's case; the same person would get two customers")
	}
	if customerKey("a@example.test") == customerKey("b@example.test") {
		t.Error("two addresses share a key; the second person would get the first one's customer")
	}
}

func TestEnsureCustomer_EscapesTheSearchQuery(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodGet, searchPath, searchResult())
	s.json(http.MethodPost, customersPath, map[string]any{"id": "cus_odd", "object": "customer"})
	a := newAdapter(t, s)

	odd := `o'br\ien@example.test`
	if _, err := a.EnsureCustomer(context.Background(), odd, nil); err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if q := s.calledOnce(http.MethodGet, searchPath).form.Get("query"); q != `email:'o\'br\\ien@example.test'` {
		t.Errorf("query = %q, want the quote and the backslash escaped", q)
	}
}

func TestEnsureCustomer_NeedsAnEmail(t *testing.T) {
	s := newStub(t)
	a := newAdapter(t, s)

	if _, err := a.EnsureCustomer(context.Background(), "", nil); err == nil {
		t.Fatal("EnsureCustomer with no email returned no error")
	}
	if n := len(s.callsTo(http.MethodGet, searchPath)); n != 0 {
		t.Errorf("an empty email reached Stripe: %d searches", n)
	}
}

func TestEnsureCustomer_ReportsASearchFailure(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, searchPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusInternalServerError, stripe.ErrorTypeAPI, "", "search is unavailable")
	})
	a := newAdapter(t, s)

	if _, err := a.EnsureCustomer(context.Background(), "buyer@example.test", nil); err == nil {
		t.Fatal("EnsureCustomer on a failing search returned no error")
	}
	// It must not fall through to a create: a search that failed is not a
	// search that found nothing.
	if n := len(s.callsTo(http.MethodPost, customersPath)); n != 0 {
		t.Errorf("a failed search created a customer anyway: %d creates", n)
	}
}

func TestEnsureCustomer_ReportsACreateFailure(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodGet, searchPath, searchResult())
	s.on(http.MethodPost, customersPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusBadRequest, stripe.ErrorTypeInvalidRequest, "", "email is invalid")
	})
	a := newAdapter(t, s)

	got, err := a.EnsureCustomer(context.Background(), "buyer@example.test", nil)
	if err == nil {
		t.Fatal("EnsureCustomer on a failing create returned no error")
	}
	if !got.Zero() {
		t.Errorf("a failed create returned a handle: %+v", got)
	}
	if errors.Is(err, pay.ErrUnconfigured) {
		t.Errorf("a create failure mapped to ErrUnconfigured: %v", err)
	}
}
