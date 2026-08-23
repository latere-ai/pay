package stripe

import (
	"context"
	"maps"
	"net/http"
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

// Three regressions, each named for the failure it prevents. They come from
// the only Stripe integration this port was extracted from that has taken a
// real payment, and each was learned in production rather than reasoned out.
// See specs/003-stripe-adapter.md, "Requirements with a proven
// implementation behind them".

// TestRegression_ManagedPaymentsLeftOnOverchargesTheCustomer pins the one
// parameter that decides whether a customer pays what the app quoted.
//
// Managed Payments is default-on for new Stripe accounts. Left on, it demands a
// product tax code and adds tax on top of the total, so the charge exceeds the
// quote the person agreed to before the redirect. It is disabled per session
// rather than by account setting, because an account default can be changed in
// the dashboard by somebody who does not know what it breaks. The exact
// parameter name came from Stripe's own error message.
func TestRegression_ManagedPaymentsLeftOnOverchargesTheCustomer(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	if _, err := a.CreateCheckout(context.Background(), topUp()); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	form := s.calledOnce(http.MethodPost, sessionsPath).form

	// Sent at all, and sent as false.
	got, ok := form["managed_payments[enabled]"]
	if !ok {
		t.Fatalf("managed_payments[enabled] was not sent; the customer would be charged tax on top of the quote: %v", form)
	}
	// Exactly once. The SDK grew a typed ManagedPayments field after this
	// adapter was written; setting both would send two values and leave which
	// one wins to Stripe.
	if len(got) != 1 {
		t.Fatalf("managed_payments[enabled] sent %d times (%v); one of them is redundant", len(got), got)
	}
	if got[0] != "false" {
		t.Errorf("managed_payments[enabled] = %q, want false", got[0])
	}
}

// TestRegression_AnAsyncPurchasePaysTwiceAndMustCreditOnce pins the pair of
// deliveries that a SEPA, iDEAL or Bancontact purchase produces.
//
// A card pays synchronously, so checkout.session.completed is already paid. The
// bank-debit methods EU customers reach for leave completed *unpaid* and
// confirm later with async_payment_succeeded, carrying the same payment intent.
// Crediting on completed regardless would credit an unpaid purchase and then
// credit it again; crediting only on a paid session makes the ledger's dedupe
// the second line of defence rather than the only one.
func TestRegression_AnAsyncPurchasePaysTwiceAndMustCreditOnce(t *testing.T) {
	a := newAdapter(t, newStub(t))

	// Delivery one: the SEPA debit has been submitted but not settled.
	unpaid := map[string]any{
		"id":             "cs_sepa",
		"object":         "checkout.session",
		"amount_total":   2000,
		"currency":       "usd",
		"payment_status": "unpaid",
		"payment_intent": "pi_sepa",
		"metadata":       map[string]string{"email": "eu@example.test", "credited_micro": "18700000"},
	}
	first := eventPayload(t, eventSessionCompleted, unpaid)
	ev, err := a.ParseWebhook(first, signedNow(first))
	if err != nil {
		t.Fatalf("ParseWebhook on the unpaid completed: %v", err)
	}
	if ev.Kind != pay.KindIgnored {
		t.Fatalf("an unpaid checkout.session.completed produced %q; a SEPA purchase would credit before the money arrived", ev.Kind)
	}

	// Delivery two: the debit settled, days later, on the same payment intent.
	paid := map[string]any{}
	maps.Copy(paid, unpaid)
	paid["payment_status"] = "paid"
	second := eventPayload(t, eventSessionAsyncPaid, paid)
	ev, err = a.ParseWebhook(second, signedNow(second))
	if err != nil {
		t.Fatalf("ParseWebhook on async_payment_succeeded: %v", err)
	}
	if ev.Kind != pay.KindPaid {
		t.Fatalf("async_payment_succeeded produced %q; an EU bank transfer would never credit", ev.Kind)
	}
	if ev.Ref != "pi_sepa" {
		t.Errorf("Ref = %q, want the same payment intent the first delivery carried", ev.Ref)
	}
	if ev.Gross != 20*money.Dollar {
		t.Errorf("Gross = %v, want %v", ev.Gross, 20*money.Dollar)
	}
	if ev.Meta["credited_micro"] != "18700000" {
		t.Errorf("Meta = %v; the credited amount computed before the redirect must survive", ev.Meta)
	}

	// And the mirror image: a card session arrives paid on completed, so
	// dropping that branch would mean a card purchase never credits.
	card := eventPayload(t, eventSessionCompleted, paidSession())
	ev, err = a.ParseWebhook(card, signedNow(card))
	if err != nil {
		t.Fatalf("ParseWebhook on the card path: %v", err)
	}
	if ev.Kind != pay.KindPaid {
		t.Fatalf("a paid checkout.session.completed produced %q; a card purchase would never credit", ev.Kind)
	}
}

// TestRegression_ARefundReversesWithItsOwnReference pins the two references a
// reversal carries.
//
// Ref points at the purchase so the ledger can find what it credited;
// ReversalRef is the refund's own id so the reversal dedupes independently. A
// reversal that reused the purchase's reference would be swallowed by the
// ledger's idempotency and the clawback would never post.
//
// The fixture carries two refunds because Stripe returns list objects
// newest-first: taking the last element picks the *oldest* refund, so a second
// partial refund would re-emit a reference the ledger already saw and the
// second reversal would vanish. A single-refund fixture passes either way and
// proves nothing.
func TestRegression_ARefundReversesWithItsOwnReference(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventChargeRefunded, map[string]any{
		"id":              "ch_test_1",
		"object":          "charge",
		"currency":        "usd",
		"amount":          2000,
		"amount_refunded": 1200,
		"payment_intent":  "pi_test_1",
		"refunds": map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "re_second", "object": "refund", "amount": 700, "currency": "usd", "created": 1_700_000_200},
				{"id": "re_first", "object": "refund", "amount": 500, "currency": "usd", "created": 1_700_000_100},
			},
		},
	})

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != pay.KindRefunded {
		t.Fatalf("Kind = %q, want refunded", ev.Kind)
	}
	if ev.Ref != "pi_test_1" {
		t.Errorf("Ref = %q, want the purchase's payment intent so the ledger can find the credit", ev.Ref)
	}
	if ev.ReversalRef != "re_second" {
		t.Errorf("ReversalRef = %q, want re_second, the newest refund; re_first is a reversal the ledger already posted", ev.ReversalRef)
	}
	if ev.ReversalRef == ev.Ref {
		t.Error("the reversal reused the purchase's reference; the ledger's dedupe would swallow it")
	}
	if ev.Gross != 7*money.Dollar {
		t.Errorf("Gross = %v, want %v, this refund's own amount and not the charge's cumulative %v",
			ev.Gross, 7*money.Dollar, 12*money.Dollar)
	}
}

func TestRegression_ARefundWithNoRefundListStillReverses(t *testing.T) {
	// A payload trimmed of its refund list still has to reverse, and still has
	// to carry a reference distinct from the purchase's.
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventChargeRefunded, map[string]any{
		"id":              "ch_test_2",
		"object":          "charge",
		"currency":        "usd",
		"amount_refunded": 2000,
		"payment_intent":  "pi_test_2",
	})

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.ReversalRef != "ch_test_2" || ev.Ref != "pi_test_2" {
		t.Errorf("Ref = %q, ReversalRef = %q, want pi_test_2 and ch_test_2", ev.Ref, ev.ReversalRef)
	}
	if ev.Gross != 20*money.Dollar {
		t.Errorf("Gross = %v, want %v", ev.Gross, 20*money.Dollar)
	}
}

func TestRegression_AReversalInAnotherCurrencyStillPosts(t *testing.T) {
	// Gross is advisory on a reversal: the ledger reverses the exact micro-USD
	// it credited, found through Ref, because a EUR charge converted at
	// purchase time and reversed at a later rate would leave a drift nothing
	// can account for. So an amount this package cannot express in USD is
	// reported as zero — "not reported" — and the reversal still posts. The
	// paid path fails closed instead, because there Gross *is* the credit.
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventChargeRefunded, map[string]any{
		"id":              "ch_eur",
		"object":          "charge",
		"currency":        "eur",
		"amount_refunded": 1840,
		"payment_intent":  "pi_eur",
	})

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != pay.KindRefunded {
		t.Fatalf("Kind = %q; dropping the reversal would leave a clawed-back credit standing", ev.Kind)
	}
	if ev.Gross != 0 {
		t.Errorf("Gross = %v, want zero: a EUR amount is not micro-USD", ev.Gross)
	}
}

func TestRegression_ARefundInheritsTheChargeCurrencyWhenItOmitsItsOwn(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventChargeRefunded, map[string]any{
		"id":             "ch_test_3",
		"object":         "charge",
		"currency":       "usd",
		"payment_intent": "pi_test_3",
		"refunds": map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "re_bare", "object": "refund", "amount": 500, "created": 1_700_000_300}},
		},
	})

	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Gross != 5*money.Dollar {
		t.Errorf("Gross = %v, want %v", ev.Gross, 5*money.Dollar)
	}
}
