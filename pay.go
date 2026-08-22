package pay

import (
	"context"
	"errors"
	"net/http"

	"latere.ai/x/pay/money"
)

// Name identifies the processor an adapter speaks to.
//
// A closed vocabulary: it is recorded as part of the actor string on ledger
// entries, and a typo there is a reconciliation that never balances.
type Name string

// The processors this port has, or expects to have, an adapter for.
const (
	Stripe Name = "stripe"
	PayPal Name = "paypal"
	Paddle Name = "paddle"
	// Memory is the in-memory fake, for tests and for a deployment that does
	// not sell anything.
	Memory Name = "memory"
)

// Capability is one thing a processor can do.
//
// A consumer asks before offering a feature, rather than discovering
// ErrUnsupported at the moment a person clicks.
type Capability string

// The capabilities an adapter may declare.
const (
	// CapCheckout is a hosted payment page. Every adapter has it.
	CapCheckout Capability = "checkout"
	// CapSavedMethod is an off-session charge against a stored method: what
	// auto-recharge needs.
	CapSavedMethod Capability = "saved_method"
	// CapRefund means the adapter reports refunds and disputes as events.
	CapRefund Capability = "refund"
	// CapTax means the processor computes and reports tax on the charge.
	CapTax Capability = "tax"
	// CapMerchantOfRecord means the processor is the seller, so the tax and
	// invoice fields on an Event are authoritative rather than ours.
	CapMerchantOfRecord Capability = "merchant_of_record"
)

// Errors this package returns.
var (
	// ErrUnconfigured reports an operation on a deployment with no processor
	// keys. It is not a failure: a local run and every test that is not about
	// payment want a service that boots and refuses to sell, rather than one
	// that panics.
	ErrUnconfigured = errors.New("pay: payments are not configured")
	// ErrBadSignature reports a webhook whose signature did not verify. A
	// delivery that cannot be authenticated posts nothing.
	ErrBadSignature = errors.New("pay: webhook signature does not verify")
	// ErrUnsupported reports a capability this adapter does not have.
	ErrUnsupported = errors.New("pay: this processor does not support that")
	// ErrDeclined reports a charge the processor refused. It is distinct from a
	// transport error because a decline must never be retried.
	ErrDeclined = errors.New("pay: the payment method was declined")
)

// TaxMode says how a charge is taxed.
type TaxMode string

// The tax modes a checkout may be created in.
const (
	// TaxNone keeps the charge equal to what the product quoted. This is the
	// default, and it is what a deployment without a tax registration wants.
	TaxNone TaxMode = "none"
	// TaxAutomatic lets the processor compute tax and report it on the Event.
	TaxAutomatic TaxMode = "automatic"
)

// CustomerRef is a durable handle for a payer at one processor. It carries the
// processor's name so a stored handle cannot be replayed against a different
// adapter after a migration.
type CustomerRef struct {
	Provider Name   `json:"provider"`
	ID       string `json:"id"`
}

// Zero reports a customer reference that names nobody.
func (c CustomerRef) Zero() bool { return c.ID == "" }

// CheckoutParams describe a purchase somebody is about to pay for.
type CheckoutParams struct {
	Email    string
	Customer CustomerRef // optional; binds the session to a saved customer
	Amount   money.Micro
	Currency money.Currency
	// Description is what the line item is called on the payment page.
	Description string
	SuccessURL  string
	CancelURL   string
	// Meta rides on the session and comes back on the Event. This is how the
	// credited amount reaches the webhook without being recomputed, so an
	// operator editing the spread mid-flight cannot change what an in-flight
	// purchase credits.
	Meta map[string]string
	// IdempotencyKey makes a retried create return the same session rather than
	// a second one. Optional when a human clicks once; required in practice for
	// anything a daemon drives.
	IdempotencyKey string
	// Tax says how the charge is taxed.
	Tax TaxMode
	// SaveMethod asks the processor to store the method for later off-session
	// use. Ignored by adapters without CapSavedMethod.
	SaveMethod bool
}

// Checkout is an opened payment page.
type Checkout struct {
	URL       string
	SessionID string
}

// SavedChargeParams describe an off-session charge against a stored method.
type SavedChargeParams struct {
	Customer    CustomerRef
	Method      string // adapter-specific handle; empty means the default
	Amount      money.Micro
	Currency    money.Currency
	Description string
	Meta        map[string]string
	// IdempotencyKey is required here, unlike checkout: nobody is present to
	// notice a double charge.
	IdempotencyKey string
}

// ChargeStatus is what became of a charge.
type ChargeStatus string

// The outcomes of an off-session charge.
const (
	ChargeSucceeded ChargeStatus = "succeeded"
	// ChargePending is an async method or an authentication challenge. Wait for
	// the webhook; do not credit yet.
	ChargePending ChargeStatus = "pending"
	ChargeFailed  ChargeStatus = "failed"
)

// Charge is the result of an off-session charge.
type Charge struct {
	// Ref is the processor's reference, which is the ledger's idempotency key.
	Ref    string
	Status ChargeStatus
}

// Kind is what a verified delivery means to a ledger.
type Kind string

// The kinds of event this port models.
const (
	// KindIgnored is every event the port does not model. An adapter returns it
	// rather than an error, so an unknown event is acknowledged and not retried
	// forever.
	KindIgnored Kind = ""
	// KindPaid is money received: credit the holder.
	KindPaid Kind = "paid"
	// KindRefunded and KindDisputed are money clawed back: reverse the credit.
	KindRefunded Kind = "refunded"
	KindDisputed Kind = "disputed"
	// KindPaymentFailed is a charge that did not go through. It is never a
	// ledger write, and it is modelled anyway because auto-recharge needs to
	// know its attempt failed. Reducing it to KindIgnored would mean
	// WebhookHandler dropped it and a product could never observe a recharge
	// that silently stopped working.
	KindPaymentFailed Kind = "payment_failed"
)

// Event is a verified delivery reduced to what a ledger needs.
//
// Flat and vendor-free: the adapter does the vendor work — the signature check,
// following a charge to its balance transaction for the USD a non-USD charge is
// actually worth — so a handler sees one trustworthy shape and never a redirect
// parameter.
type Event struct {
	Kind     Kind
	Provider Name
	Email    string
	Customer CustomerRef
	// Ref is the purchase's reference, stable across deliveries of the same
	// purchase. It is the ledger's idempotency key.
	Ref string
	// ReversalRef identifies a refund or dispute, distinct from Ref, so a
	// reversal dedupes on its own reference.
	ReversalRef string
	// Gross is what the charge is worth in micro-USD, at the processor's own
	// rate on this charge.
	Gross money.Micro
	// Tax and Net are filled only by an adapter with CapTax or
	// CapMerchantOfRecord. Zero means "not reported", which is not the same as
	// "no tax"; a consumer that needs the distinction asks Has(CapTax).
	Tax money.Micro
	Net money.Micro
	// InvoiceID and SellerOfRecord let a merchant-of-record adapter point a
	// person at their own invoice without this platform issuing one.
	InvoiceID      string
	SellerOfRecord string
	// Meta is the session metadata echoed back.
	Meta map[string]string
	// Raw is the verified payload, for a product that needs a field this port
	// does not model. Reading it couples that product to a vendor, so it is a
	// documented escape hatch rather than a normal path.
	Raw []byte
}

// Provider is the port.
//
// An unconfigured deployment's Provider returns ErrUnconfigured from every
// operation rather than being nil, so a caller never dereferences a missing
// processor.
type Provider interface {
	// Name is the processor this adapter speaks to.
	Name() Name
	// Has reports whether this adapter declares a capability.
	Has(c Capability) bool
	// CreateCheckout opens a hosted payment page.
	CreateCheckout(ctx context.Context, p CheckoutParams) (Checkout, error)
	// EnsureCustomer resolves a durable customer handle for an email, creating
	// one if needed. Idempotent on email. ErrUnsupported when the adapter has no
	// customer concept.
	EnsureCustomer(ctx context.Context, email string, meta map[string]string) (CustomerRef, error)
	// ChargeSaved charges a method the customer already authorised, with no page
	// and nobody present. A decline is ErrDeclined and must not be retried.
	ChargeSaved(ctx context.Context, p SavedChargeParams) (Charge, error)
	// ParseWebhook authenticates a delivery and reduces it to an Event.
	//
	// It takes the whole header set rather than one signature string: Stripe
	// signs in Stripe-Signature, PayPal spreads verification across five
	// headers, and a one-string signature would have to be re-generalised the
	// day a second adapter lands.
	ParseWebhook(payload []byte, h http.Header) (Event, error)
}
