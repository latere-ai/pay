package stripe

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	stripe "github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

// signatureHeader is where Stripe puts the delivery's signature.
const signatureHeader = "Stripe-Signature"

// The event types this adapter models. Exactly the list an operator subscribes
// the endpoint to in docs/stripe-operations.md; anything else reduces to
// pay.KindIgnored so an unexpected delivery is acknowledged rather than retried
// forever.
const (
	// eventSessionCompleted is the synchronous card path. It is also delivered
	// for an async method, unpaid, which is why payment_status is checked.
	eventSessionCompleted = "checkout.session.completed"
	// eventSessionAsyncPaid confirms SEPA Direct Debit, iDEAL and Bancontact,
	// which leave `completed` unpaid and settle later.
	eventSessionAsyncPaid = "checkout.session.async_payment_succeeded"
	eventChargeRefunded   = "charge.refunded"
	eventDisputeCreated   = "charge.dispute.created"
	// eventPaymentFailed is auto-recharge telemetry, never a ledger write.
	eventPaymentFailed = "payment_intent.payment_failed"
)

// ParseWebhook authenticates a delivery and reduces it to a pay.Event.
//
// It reads Stripe-Signature from the header set rather than taking a bare
// string, which is where generalising to a processor that spreads verification
// across several headers costs nothing.
//
// IgnoreAPIVersionMismatch is on: the signature is what authenticates the
// delivery and only long-stable fields are read (metadata, payment_intent,
// payment_status, amounts), so an endpoint registered on a different API
// version than the pinned SDK must not crash crediting. A bad signature still
// fails closed.
func (a *Adapter) ParseWebhook(payload []byte, h http.Header) (pay.Event, error) {
	if !a.configured {
		return pay.Event{}, pay.ErrUnconfigured
	}
	ev, err := webhook.ConstructEventWithOptions(payload, h.Get(signatureHeader), a.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return pay.Event{}, constructError(err)
	}
	raw := eventObject(ev)
	switch ev.Type {
	case eventSessionCompleted, eventSessionAsyncPaid:
		return a.session(raw, payload)
	case eventChargeRefunded:
		return refunded(raw, payload)
	case eventDisputeCreated:
		return disputed(raw, payload)
	case eventPaymentFailed:
		return paymentFailed(raw, payload)
	default:
		return ignored(payload), nil
	}
}

// eventObject is the delivery's data.object, or nil when there is none.
//
// A verified envelope carrying no `data` member is not hypothetical: the
// signature covers whatever bytes were posted, so anyone holding the signing
// secret can post one, and the SDK leaves Data nil rather than erroring. The
// nil then decodes to a plain error in each branch instead of panicking the
// endpoint.
func eventObject(ev stripe.Event) []byte {
	if ev.Data == nil {
		return nil
	}
	return ev.Data.Raw
}

// constructError classifies what went wrong before an event existed.
//
// Both outcomes fail closed and both are terminal, so the distinction is for
// whoever reads the log: a signature that did not verify is an authentication
// problem worth investigating, and a body that did not parse is a corrupt
// delivery that a redelivery might fix.
func constructError(err error) error {
	for _, sig := range []error{
		webhook.ErrNotSigned,
		webhook.ErrInvalidHeader,
		webhook.ErrNoValidSignature,
		webhook.ErrTooOld,
	} {
		if errors.Is(err, sig) {
			return fmt.Errorf("pay/stripe: %w: %v", pay.ErrBadSignature, err)
		}
	}
	return fmt.Errorf("pay/stripe: decode delivery: %w", err)
}

// ignored is the benign event: acknowledged, and nothing posted.
func ignored(payload []byte) pay.Event {
	return pay.Event{Kind: pay.KindIgnored, Provider: pay.Stripe, Raw: payload}
}

// session reduces a checkout session to a credit, or to nothing.
//
// A synchronous card pays immediately, so `completed` is already `paid`. An
// async method — SEPA Direct Debit, iDEAL, Bancontact, the ones EU customers
// reach for — leaves `completed` unpaid and confirms later with
// `async_payment_succeeded`. Only a *paid* session credits, so the two
// deliveries for one async purchase credit exactly once and the ledger's dedupe
// on the payment intent is the second line of defence rather than the only one.
func (a *Adapter) session(raw, payload []byte) (pay.Event, error) {
	var s stripe.CheckoutSession
	if err := json.Unmarshal(raw, &s); err != nil {
		return pay.Event{}, fmt.Errorf("pay/stripe: decode checkout session: %w", err)
	}
	if s.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
		return ignored(payload), nil
	}
	ref := sessionRef(&s)
	if ref == "" {
		// The ledger dedupes on this. A credit with nothing to dedupe on posts
		// again on every redelivery, so refuse it rather than credit it.
		return pay.Event{}, fmt.Errorf("%w: a paid checkout session", ErrNoReference)
	}
	gross, err := sessionGross(&s)
	if err != nil {
		return pay.Event{}, err
	}
	e := pay.Event{
		Kind:     pay.KindPaid,
		Provider: pay.Stripe,
		Email:    sessionEmail(&s),
		Ref:      ref,
		Gross:    gross,
		Meta:     s.Metadata,
		Raw:      payload,
	}
	if s.Customer != nil && s.Customer.ID != "" {
		e.Customer = pay.CustomerRef{Provider: pay.Stripe, ID: s.Customer.ID}
	}
	// Tax and Net stay zero unless this deployment computes tax, because zero
	// means "not reported" and a consumer that needs the distinction asks
	// Has(CapTax).
	if a.Has(pay.CapTax) && s.TotalDetails != nil {
		e.Tax = money.FromMinor(s.TotalDetails.AmountTax, money.USD)
		e.Net = gross - e.Tax
	}
	return e, nil
}

// sessionGross is what the purchase is worth in micro-USD.
//
// Under Adaptive Pricing the session is created in USD and presented to the
// customer in their own currency: `currency`/`amount_total` are then the
// customer's, and `currency_conversion` carries the creation currency and the
// total in it. That is the figure the ledger holds, and it is Stripe's own rate
// on this charge rather than one looked up later.
func sessionGross(s *stripe.CheckoutSession) (money.Micro, error) {
	cur, minor := money.Currency(s.Currency), s.AmountTotal
	if s.CurrencyConversion != nil {
		cur = money.Currency(s.CurrencyConversion.SourceCurrency)
		minor = s.CurrencyConversion.AmountTotal
	}
	if cur != money.USD {
		// Fail closed. money.FromMinor on a currency it does not know inflates
		// the amount a hundredfold, and a wrong credit is worse than a delivery
		// the operator has to look at.
		return 0, fmt.Errorf("%w: session %s is in %s", ErrNotUSD, s.ID, cur)
	}
	return money.FromMinor(minor, money.USD), nil
}

// sessionEmail prefers the metadata the app wrote, because that is the identity
// the purchase was quoted against; what Checkout collected is the fallback.
func sessionEmail(s *stripe.CheckoutSession) string {
	if e := s.Metadata["email"]; e != "" {
		return e
	}
	if s.CustomerDetails != nil && s.CustomerDetails.Email != "" {
		return s.CustomerDetails.Email
	}
	return s.CustomerEmail
}

// sessionRef is the purchase's reference, stable across both deliveries of an
// async purchase. The payment intent is what a refund later points back at; the
// session id is only a fallback for a session that somehow carries no intent.
func sessionRef(s *stripe.CheckoutSession) string {
	if s.PaymentIntent != nil && s.PaymentIntent.ID != "" {
		return s.PaymentIntent.ID
	}
	return s.ID
}

// chargePayload is the minimal shape read from a charge.
//
// Hand-declared rather than decoded through the SDK's expandable types, so an
// SDK bump cannot change the shape underneath a reversal. Every field here has
// been stable in the Stripe API for years.
type chargePayload struct {
	ID             string            `json:"id"`
	Currency       string            `json:"currency"`
	AmountRefunded int64             `json:"amount_refunded"`
	PaymentIntent  string            `json:"payment_intent"`
	Metadata       map[string]string `json:"metadata"`
	Refunds        struct {
		Data []struct {
			ID       string `json:"id"`
			Amount   int64  `json:"amount"`
			Created  int64  `json:"created"`
			Currency string `json:"currency"`
		} `json:"data"`
	} `json:"refunds"`
}

// refunded reverses a credit.
//
// The payment intent finds the original purchase; the refund's own id is this
// reversal's reference, so a reversal dedupes independently of the purchase and
// a second partial refund is not mistaken for a replay of the first.
//
// Gross is advisory here and may be zero. The ledger reverses the exact
// micro-USD it credited, found through Ref, rather than the amount refunded: a
// EUR charge converted at purchase time and reversed at a later rate would
// otherwise leave a drift nothing can account for.
func refunded(raw, payload []byte) (pay.Event, error) {
	var c chargePayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return pay.Event{}, fmt.Errorf("pay/stripe: decode charge: %w", err)
	}
	ref, amount, cur := latestRefund(&c)
	if err := reversible(c.PaymentIntent, ref); err != nil {
		return pay.Event{}, err
	}
	return pay.Event{
		Kind:        pay.KindRefunded,
		Provider:    pay.Stripe,
		Ref:         c.PaymentIntent,
		ReversalRef: ref,
		Gross:       usdOrZero(amount, cur),
		Meta:        c.Metadata,
		Raw:         payload,
	}, nil
}

// latestRefund picks the refund this delivery is about.
//
// By newest `created`, not by position: Stripe returns list objects
// newest-first, so taking the last element picks the *oldest* refund and a
// second partial refund would re-emit a reference the ledger has already
// deduped, silently dropping the reversal. Ties keep the earlier element, which
// is the newer one under that ordering. A charge with no refunds in the payload
// falls back to the charge itself, which is still distinct from the purchase's
// payment intent.
func latestRefund(c *chargePayload) (ref string, amount int64, cur string) {
	ref, amount, cur = c.ID, c.AmountRefunded, c.Currency
	newest := int64(-1)
	for _, r := range c.Refunds.Data {
		if r.Created > newest {
			// A refund that omits its own currency inherits the charge's.
			newest, ref, amount, cur = r.Created, r.ID, r.Amount, cmp.Or(r.Currency, c.Currency)
		}
	}
	return ref, amount, cur
}

// reversible refuses a clawback that cannot dedupe.
//
// A reversal needs both references: the purchase's, to find what was credited,
// and its own, distinct one, so a second partial refund is not mistaken for a
// replay of the first. Missing or identical, the reversal would either post
// nothing or post forever.
func reversible(ref, reversalRef string) error {
	switch {
	case ref == "":
		return fmt.Errorf("%w: a reversal with no purchase to reverse", ErrNoReference)
	case reversalRef == "":
		return fmt.Errorf("%w: a reversal with none of its own", ErrNoReference)
	case ref == reversalRef:
		return fmt.Errorf("%w: a reversal reusing the purchase's reference %s", ErrNoReference, ref)
	}
	return nil
}

// disputed reverses a credit a card network clawed back. The dispute's id is
// the reversal's own reference.
func disputed(raw, payload []byte) (pay.Event, error) {
	var d struct {
		ID            string            `json:"id"`
		Amount        int64             `json:"amount"`
		Currency      string            `json:"currency"`
		PaymentIntent string            `json:"payment_intent"`
		Metadata      map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return pay.Event{}, fmt.Errorf("pay/stripe: decode dispute: %w", err)
	}
	if err := reversible(d.PaymentIntent, d.ID); err != nil {
		return pay.Event{}, err
	}
	return pay.Event{
		Kind:        pay.KindDisputed,
		Provider:    pay.Stripe,
		Ref:         d.PaymentIntent,
		ReversalRef: d.ID,
		Gross:       usdOrZero(d.Amount, d.Currency),
		Meta:        d.Metadata,
		Raw:         payload,
	}, nil
}

// paymentFailed carries a failed off-session charge.
//
// It is subscribed to for auto-recharge telemetry and is never a ledger write,
// so it reduces to pay.KindIgnored — the port has no kind for "money did not
// move". The intent's reference and metadata still ride along for a caller that
// drives ParseWebhook itself; pay.WebhookHandler acknowledges and drops it.
func paymentFailed(raw, payload []byte) (pay.Event, error) {
	var pi struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &pi); err != nil {
		return pay.Event{}, fmt.Errorf("pay/stripe: decode payment intent: %w", err)
	}
	e := ignored(payload)
	e.Kind = pay.KindPaymentFailed
	e.Ref = pi.ID
	e.Meta = pi.Metadata
	return e, nil
}

// usdOrZero converts a minor amount, reporting zero for any currency that is
// not USD. Zero means "not reported", which on a reversal is safe: the ledger
// reverses by what it credited, keyed on Ref.
func usdOrZero(minor int64, cur string) money.Micro {
	if money.Currency(cur) != money.USD {
		return 0
	}
	return money.FromMinor(minor, money.USD)
}
