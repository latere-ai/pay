package stripe

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

func TestParseWebhook_VerifiesTheSignature(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != pay.KindPaid {
		t.Errorf("Kind = %q, want paid", ev.Kind)
	}
	if string(ev.Raw) != string(payload) {
		t.Error("Raw is not the verified payload")
	}
}

func TestParseWebhook_RefusesASignatureFromAnotherSecret(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	// One Stripe account, several endpoints, one signing secret each. A
	// delivery signed for another product's endpoint posts nothing here.
	h := signedHeader("whsec_some_other_product", payload, time.Now())
	if _, err := a.ParseWebhook(payload, h); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("ParseWebhook with a foreign secret = %v, want ErrBadSignature", err)
	}
}

func TestParseWebhook_RefusesATamperedPayload(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())
	h := signedNow(payload)

	tampered := []byte(strings.Replace(string(payload), `"amount_total":2000`, `"amount_total":9000`, 1))
	if string(tampered) == string(payload) {
		t.Fatal("the fixture did not change; the test proves nothing")
	}
	if _, err := a.ParseWebhook(tampered, h); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("ParseWebhook on a tampered payload = %v, want ErrBadSignature", err)
	}
}

func TestParseWebhook_RefusesASignatureOutsideTheToleranceWindow(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	// A replay of a captured delivery is only useful while its timestamp is
	// fresh, which is what the tolerance window is for.
	old := signedHeader(testWebhookSecret, payload, time.Now().Add(-31*time.Minute))
	if _, err := a.ParseWebhook(payload, old); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("ParseWebhook on a stale signature = %v, want ErrBadSignature", err)
	}
}

func TestParseWebhook_RefusesAnUnsignedDelivery(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	if _, err := a.ParseWebhook(payload, http.Header{}); !errors.Is(err, pay.ErrBadSignature) {
		t.Fatalf("ParseWebhook with no header = %v, want ErrBadSignature", err)
	}
}

func TestParseWebhook_ToleratesAnEndpointOnAnotherAPIVersion(t *testing.T) {
	// Every fixture in this suite carries a 2019 api_version, so a passing run
	// is a standing test that IgnoreAPIVersionMismatch is on. This one names
	// the requirement so a change that turns it off fails somewhere legible.
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("a delivery from an endpoint on an older API version failed: %v", err)
	}
	if ev.Kind != pay.KindPaid {
		t.Errorf("Kind = %q, want paid", ev.Kind)
	}
}

func TestParseWebhook_UnmodelledEventsAreIgnoredNotRefused(t *testing.T) {
	a := newAdapter(t, newStub(t))
	for _, typ := range []string{
		"customer.discount.created",
		"invoice.paid",
		"checkout.session.expired",
		"checkout.session.async_payment_failed",
	} {
		t.Run(typ, func(t *testing.T) {
			payload := eventPayload(t, typ, map[string]any{"id": "obj_1"})
			ev, err := a.ParseWebhook(payload, signedNow(payload))
			if err != nil {
				t.Fatalf("ParseWebhook: %v", err)
			}
			// Not an error: an event nobody can act on will not become
			// actionable on the fourth redelivery.
			if ev.Kind != pay.KindIgnored {
				t.Errorf("Kind = %q, want KindIgnored", ev.Kind)
			}
			if ev.Provider != pay.Stripe {
				t.Errorf("Provider = %q, want stripe", ev.Provider)
			}
		})
	}
}

func TestParseWebhook_PaidSessionCarriesWhatTheLedgerNeeds(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Ref != "pi_test_1" {
		t.Errorf("Ref = %q, want the payment intent", ev.Ref)
	}
	if ev.Gross != 20*money.Dollar {
		t.Errorf("Gross = %v, want %v", ev.Gross, 20*money.Dollar)
	}
	if ev.Email != "buyer@example.test" {
		t.Errorf("Email = %q", ev.Email)
	}
	if want := (pay.CustomerRef{Provider: pay.Stripe, ID: "cus_test_1"}); ev.Customer != want {
		t.Errorf("Customer = %+v, want %+v", ev.Customer, want)
	}
	if ev.Meta["credited_micro"] != "18700000" {
		t.Errorf("Meta = %v; the credited amount must survive the round trip", ev.Meta)
	}
	// Tax is not reported by a deployment that does not compute it, and zero is
	// how the port says "not reported".
	if ev.Tax != 0 || ev.Net != 0 {
		t.Errorf("Tax = %v, Net = %v; want zero without CapTax", ev.Tax, ev.Net)
	}
}

func TestParseWebhook_ReportsTaxOnlyWithCapTax(t *testing.T) {
	a := newAdapter(t, newStub(t), withTax)
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Tax != 3*money.Dollar {
		t.Errorf("Tax = %v, want %v", ev.Tax, 3*money.Dollar)
	}
	if ev.Net != 17*money.Dollar {
		t.Errorf("Net = %v, want %v", ev.Net, 17*money.Dollar)
	}
}

func TestParseWebhook_AdaptivePricingReportsTheUSDTheChargeWasCreatedIn(t *testing.T) {
	// A EUR customer sees euros; the session was created in USD and
	// currency_conversion carries that total back. The ledger holds the USD,
	// at Stripe's own rate on this charge rather than one looked up later.
	s := paidSession()
	s["currency"] = "eur"
	s["amount_total"] = 1840
	s["currency_conversion"] = map[string]any{
		"amount_subtotal": 2000,
		"amount_total":    2000,
		"fx_rate":         "0.92",
		"source_currency": "usd",
	}
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, s)

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Gross != 20*money.Dollar {
		t.Errorf("Gross = %v, want the $20 the session was created in, not the €18.40 presented", ev.Gross)
	}
}

func TestParseWebhook_RefusesToCreditANonUSDSession(t *testing.T) {
	// money.FromMinor on a currency it does not know multiplies by a million
	// rather than ten thousand, so a GBP session would credit a hundredfold.
	// Fail closed: a delivery an operator has to look at beats a wrong credit.
	s := paidSession()
	s["currency"] = "gbp"
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, s)

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if !errors.Is(err, ErrNotUSD) {
		t.Fatalf("ParseWebhook on a GBP session = %v, want ErrNotUSD", err)
	}
	if ev.Kind != pay.KindIgnored {
		t.Errorf("Kind = %q; a refused session must credit nothing", ev.Kind)
	}
}

func TestParseWebhook_FallsBackForEmailAndReference(t *testing.T) {
	s := map[string]any{
		"id":               "cs_no_intent",
		"object":           "checkout.session",
		"amount_total":     500,
		"currency":         "usd",
		"payment_status":   "paid",
		"customer_details": map[string]any{"email": "collected@example.test"},
	}
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, s)

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Ref != "cs_no_intent" {
		t.Errorf("Ref = %q, want the session id when there is no payment intent", ev.Ref)
	}
	if ev.Email != "collected@example.test" {
		t.Errorf("Email = %q, want what Checkout collected", ev.Email)
	}
	if !ev.Customer.Zero() {
		t.Errorf("Customer = %+v, want zero", ev.Customer)
	}
}

func TestParseWebhook_FallsBackToTheSessionsOwnEmailField(t *testing.T) {
	s := map[string]any{
		"id":             "cs_email_only",
		"object":         "checkout.session",
		"amount_total":   500,
		"currency":       "usd",
		"payment_status": "paid",
		"customer_email": "prefilled@example.test",
	}
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, s)

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Email != "prefilled@example.test" {
		t.Errorf("Email = %q", ev.Email)
	}
}

func TestParseWebhook_DisputeReversesWithItsOwnReference(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventDisputeCreated, map[string]any{
		"id":             "dp_test_1",
		"object":         "dispute",
		"amount":         2000,
		"currency":       "usd",
		"payment_intent": "pi_test_1",
	})

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != pay.KindDisputed {
		t.Errorf("Kind = %q, want disputed", ev.Kind)
	}
	if ev.Ref != "pi_test_1" {
		t.Errorf("Ref = %q, want the purchase's payment intent", ev.Ref)
	}
	if ev.ReversalRef != "dp_test_1" {
		t.Errorf("ReversalRef = %q, want the dispute id", ev.ReversalRef)
	}
	if ev.Gross != 20*money.Dollar {
		t.Errorf("Gross = %v, want %v", ev.Gross, 20*money.Dollar)
	}
}

func TestParseWebhook_PaymentFailedIsTelemetryNotALedgerWrite(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventPaymentFailed, map[string]any{
		"id":       "pi_failed",
		"object":   "payment_intent",
		"status":   "requires_payment_method",
		"metadata": map[string]string{"holder": "acme"},
	})

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != pay.KindIgnored {
		t.Errorf("Kind = %q; a failed charge moves no money", ev.Kind)
	}
	// The reference still rides along for a caller driving ParseWebhook
	// itself, which is what auto-recharge telemetry reads.
	if ev.Ref != "pi_failed" {
		t.Errorf("Ref = %q, want pi_failed", ev.Ref)
	}
	if ev.Meta["holder"] != "acme" {
		t.Errorf("Meta = %v", ev.Meta)
	}
}

func TestParseWebhook_ReportsAnUndecodableObject(t *testing.T) {
	// Every modelled branch decodes its object. A payload that verifies but
	// carries the wrong shape is an error rather than a silent zero event: an
	// event that cannot be read is not an event that moved no money.
	for _, typ := range []string{eventSessionCompleted, eventSessionAsyncPaid, eventChargeRefunded, eventDisputeCreated, eventPaymentFailed} {
		t.Run(typ, func(t *testing.T) {
			a := newAdapter(t, newStub(t))
			// A well-formed envelope whose object has an id of the wrong type.
			payload := eventPayload(t, typ, map[string]any{"id": 123})

			ev, err := a.ParseWebhook(payload, signedNow(payload))
			if err == nil {
				t.Fatalf("ParseWebhook on a malformed object returned %+v and no error", ev)
			}
			if errors.Is(err, pay.ErrBadSignature) {
				t.Errorf("a decode failure was reported as a bad signature: %v", err)
			}
			if ev.Kind != pay.KindIgnored {
				t.Errorf("Kind = %q; a delivery that did not decode must credit nothing", ev.Kind)
			}
		})
	}
}

func TestParseWebhook_ABodyThatDoesNotParseIsNotABadSignature(t *testing.T) {
	// The signature verifies, so this is a corrupt delivery rather than a
	// forged one, and the log should not send an operator hunting a secret.
	a := newAdapter(t, newStub(t))
	payload := []byte(`{"id":"evt_1","object":"event","type":"checkout.session.completed","data":{"object":"a string"}}`)

	_, err := a.ParseWebhook(payload, signedNow(payload))
	if err == nil {
		t.Fatal("ParseWebhook on an unparsable body returned no error")
	}
	if errors.Is(err, pay.ErrBadSignature) {
		t.Errorf("a parse failure was reported as a bad signature: %v", err)
	}
}

// TestParseWebhook_RefusesADeliveryWithNothingToDedupeOn is a regression the
// fuzzer found. The signature proves who posted the bytes, not that they have
// the shape Stripe sends, and a credit with no reference posts again on every
// redelivery because the ledger's idempotency has nothing to key on.
func TestParseWebhook_RefusesADeliveryWithNothingToDedupeOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  string
		obj  map[string]any
	}{
		{
			name: "a paid session with no intent and no id",
			typ:  eventSessionCompleted,
			obj:  map[string]any{"object": "checkout.session", "payment_status": "paid", "currency": "usd"},
		},
		{
			name: "a refund with no purchase to reverse",
			typ:  eventChargeRefunded,
			obj:  map[string]any{"id": "ch_1", "object": "charge", "currency": "usd", "amount_refunded": 100},
		},
		{
			name: "a refund with no reference of its own",
			typ:  eventChargeRefunded,
			obj:  map[string]any{"object": "charge", "currency": "usd", "payment_intent": "pi_1"},
		},
		{
			name: "a dispute reusing the purchase's reference",
			typ:  eventDisputeCreated,
			obj:  map[string]any{"id": "pi_1", "object": "dispute", "payment_intent": "pi_1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t, newStub(t))
			payload := eventPayload(t, tc.typ, tc.obj)

			ev, err := a.ParseWebhook(payload, signedNow(payload))
			if !errors.Is(err, ErrNoReference) {
				t.Fatalf("ParseWebhook = %v, want ErrNoReference", err)
			}
			if ev.Kind != pay.KindIgnored {
				t.Errorf("Kind = %q; a refused delivery must post nothing", ev.Kind)
			}
		})
	}
}

// TestParseWebhook_AnEventWithNoDataMemberDoesNotPanic is a regression the
// fuzzer found. The signature covers whatever bytes were posted, so anyone
// holding the signing secret can post an envelope with no `data`; the SDK
// leaves Event.Data nil, and reading through it crashed the endpoint.
func TestParseWebhook_AnEventWithNoDataMemberDoesNotPanic(t *testing.T) {
	a := newAdapter(t, newStub(t))
	for _, typ := range []string{
		eventSessionCompleted, eventSessionAsyncPaid, eventChargeRefunded,
		eventDisputeCreated, eventPaymentFailed, "customer.discount.created",
	} {
		t.Run(typ, func(t *testing.T) {
			payload := []byte(`{"object":"event","type":"` + typ + `"}`)
			ev, err := a.ParseWebhook(payload, signedNow(payload))
			if err == nil && ev.Kind != pay.KindIgnored {
				t.Errorf("Kind = %q; an event with no object may not credit", ev.Kind)
			}
		})
	}
}
