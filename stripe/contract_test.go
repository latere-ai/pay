package stripe

import (
	"bytes"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/paytest"
)

// fakeAccount is a Stripe account with enough memory to answer the conformance
// suite honestly.
//
// The suite asks EnsureCustomer for the same address twice and requires one
// handle back, so a stub that hands out a canned customer would pass without
// proving anything. This one keys customers by email and by idempotency key,
// which is the two-sided guarantee the adapter actually rests on.
type fakeAccount struct {
	mu        sync.Mutex
	seq       int
	byEmail   map[string]string
	byKey     map[string]string
	sessions  int
	emailFrom *regexp.Regexp
}

func newFakeAccount(t *testing.T, s *stub) *fakeAccount {
	t.Helper()
	f := &fakeAccount{
		byEmail:   map[string]string{},
		byKey:     map[string]string{},
		emailFrom: regexp.MustCompile(`^email:'(.*)'$`),
	}
	s.on(http.MethodGet, searchPath, f.search)
	s.on(http.MethodPost, customersPath, f.createCustomer)
	s.on(http.MethodPost, sessionsPath, f.createSession)
	s.json(http.MethodPost, intentsPath, map[string]any{
		"id": "pi_contract", "object": "payment_intent", "status": "succeeded",
	})
	return f
}

func (f *fakeAccount) search(w http.ResponseWriter, r *http.Request) {
	m := f.emailFrom.FindStringSubmatch(r.Form.Get("query"))
	if m == nil {
		writeJSON(w, searchResult())
		return
	}
	f.mu.Lock()
	id, ok := f.byEmail[m[1]]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, searchResult())
		return
	}
	writeJSON(w, searchResult(map[string]any{"id": id, "object": "customer", "email": m[1]}))
}

func (f *fakeAccount) createCustomer(w http.ResponseWriter, r *http.Request) {
	email := r.Form.Get("email")
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	defer f.mu.Unlock()
	// Stripe replays the original response for a repeated key. That is what
	// stops a create racing an eventually-consistent search from minting a
	// second customer for one person.
	id, ok := f.byKey[key]
	if !ok {
		f.seq++
		id = "cus_contract_" + strconv.Itoa(f.seq)
		f.byKey[key] = id
		f.byEmail[email] = id
	}
	writeJSON(w, map[string]any{"id": id, "object": "customer", "email": email})
}

func (f *fakeAccount) createSession(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.sessions++
	n := f.sessions
	f.mu.Unlock()
	id := "cs_contract_" + strconv.Itoa(n)
	writeJSON(w, map[string]any{
		"id":     id,
		"object": "checkout.session",
		"url":    "https://checkout.stripe.com/c/pay/" + id,
	})
}

// TestProviderContract runs the port's conformance suite against the adapter,
// declaring exactly the capabilities it claims. An adapter that says it has
// CapSavedMethod and returns ErrUnsupported fails here.
func TestProviderContract(t *testing.T) {
	paytest.RunProviderContract(t, func(t *testing.T) pay.Provider {
		s := newStub(t)
		newFakeAccount(t, s)
		return newAdapter(t, s)
	}, []pay.Capability{pay.CapCheckout, pay.CapSavedMethod, pay.CapRefund})
}

// TestProviderContract_WithTax runs the same suite against a deployment that
// turned Stripe Tax on, which is the only difference in what the adapter
// declares.
func TestProviderContract_WithTax(t *testing.T) {
	paytest.RunProviderContract(t, func(t *testing.T) pay.Provider {
		s := newStub(t)
		newFakeAccount(t, s)
		return newAdapter(t, s, withTax)
	}, []pay.Capability{pay.CapCheckout, pay.CapSavedMethod, pay.CapRefund, pay.CapTax})
}

func TestHas_DeclaresTaxOnlyWhenTheDeploymentHasIt(t *testing.T) {
	s := newStub(t)
	plain, taxed := newAdapter(t, s), newAdapter(t, s, withTax)

	for _, c := range []pay.Capability{pay.CapCheckout, pay.CapSavedMethod, pay.CapRefund} {
		if !plain.Has(c) {
			t.Errorf("Has(%s) = false", c)
		}
	}
	if plain.Has(pay.CapTax) {
		t.Error("a deployment without Stripe Tax declares CapTax")
	}
	if !taxed.Has(pay.CapTax) {
		t.Error("a deployment with Stripe Tax does not declare CapTax")
	}
	// Nobody here is the seller of record; Managed Payments is the thing this
	// adapter goes out of its way to keep off.
	if plain.Has(pay.CapMerchantOfRecord) || taxed.Has(pay.CapMerchantOfRecord) {
		t.Error("the adapter declares CapMerchantOfRecord")
	}
}

// FuzzParseWebhook drives the decode path with unconstrained bytes.
//
// The payload is signed inside the fuzz function with the harness secret,
// because an unsigned input dies at the HMAC and would leave the JSON decoding
// — the part that actually takes arbitrary bytes off the internet — untouched.
func FuzzParseWebhook(f *testing.F) {
	f.Add(eventPayload(f, eventSessionCompleted, paidSession()))
	f.Add(eventPayload(f, eventChargeRefunded, map[string]any{
		"id": "ch_1", "currency": "usd", "amount_refunded": 100, "payment_intent": "pi_1",
	}))
	f.Add(eventPayload(f, eventDisputeCreated, map[string]any{"id": "dp_1", "payment_intent": "pi_1"}))
	f.Add(eventPayload(f, eventPaymentFailed, map[string]any{"id": "pi_1"}))
	f.Add([]byte(`{"type":"checkout.session.completed","data":{"object":{}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))

	// No backend: ParseWebhook never leaves the process, so the adapter needs
	// no stub and the fuzzer needs no server.
	a := New(Config{SecretKey: testSecretKey, WebhookSecret: testWebhookSecret})

	f.Fuzz(func(t *testing.T, payload []byte) {
		ev, err := a.ParseWebhook(payload, signedNow(payload))
		if err != nil {
			if ev.Kind != pay.KindIgnored {
				t.Fatalf("a failed parse returned Kind %q; nothing may post on an error", ev.Kind)
			}
			return
		}
		switch ev.Kind {
		case pay.KindIgnored:
			// Acknowledged and dropped.
		case pay.KindPaid:
			if ev.Ref == "" {
				t.Fatal("a credit with no reference: the ledger has nothing to dedupe on")
			}
			if ev.Gross < 0 {
				t.Fatalf("a credit of %v", ev.Gross)
			}
		case pay.KindPaymentFailed:
			// Telemetry: it carries a reference and never an amount, because
			// nothing moved.
			if ev.Gross != 0 {
				t.Fatalf("a failed payment reported a gross of %d", ev.Gross)
			}
		case pay.KindRefunded, pay.KindDisputed:
			if ev.ReversalRef == "" {
				t.Fatal("a reversal with no reference of its own")
			}
			if ev.ReversalRef == ev.Ref {
				t.Fatalf("a reversal reusing the purchase's reference %q", ev.Ref)
			}
		default:
			t.Fatalf("Kind = %q, which the port does not define", ev.Kind)
		}
		if ev.Provider != pay.Stripe {
			t.Fatalf("Provider = %q", ev.Provider)
		}
		if !bytes.Equal(ev.Raw, payload) {
			t.Fatal("Raw is not the verified payload")
		}
	})
}
