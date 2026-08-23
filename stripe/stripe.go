// Package stripe is the Stripe implementation of pay.Provider.
//
// It is the only place in the component that names the vendor: everything
// upstream depends on the port, so a second processor is a sibling package
// rather than an edit here. The checkout is always created in USD; an EU
// customer sees euros through the account's Adaptive Pricing setting, which is
// Stripe configuration rather than code, and the USD a converted charge is
// actually worth arrives on the session as currency_conversion.
//
// See docs/stripe-operations.md for the account settings this adapter
// assumes, and docs/adapters.md for writing a sibling.
package stripe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	stripe "github.com/stripe/stripe-go/v85"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

// ErrForeignCustomer reports a pay.CustomerRef minted by another processor.
//
// A handle is provider-tagged precisely so a migration cannot replay one
// account's customer id against a different processor's API, where it would
// either 404 or, worse, collide with an unrelated object.
var ErrForeignCustomer = errors.New("pay/stripe: customer reference belongs to another processor")

// ErrNotUSD reports a delivery whose amount is not in USD.
//
// pay.Event.Gross is micro-USD by definition. Reporting a EUR minor unit as
// though it were USD would credit a wallet a hundredfold, so the adapter
// refuses rather than guesses. Adaptive Pricing makes this unreachable in a
// correctly configured account: the session is created in USD and
// currency_conversion carries that USD total back.
var ErrNotUSD = errors.New("pay/stripe: amount is not in USD")

// ErrNoReference reports a delivery that moved money but carries no reference
// to dedupe on, or a reversal whose reference is not distinct from the
// purchase's.
//
// The ledger's idempotency is keyed on the processor's reference; without one a
// credit posts again on every redelivery. Stripe does not send such a delivery,
// but the signature only proves who posted the bytes, so the adapter fails
// closed rather than trusting the shape.
var ErrNoReference = errors.New("pay/stripe: delivery carries no reference to dedupe on")

// setupFutureUsageOffSession stores the method for a later charge with nobody
// present. It is a plain string on the create params in stripe-go v85, so the
// closed vocabulary is declared here rather than left as a literal at the call
// site.
const setupFutureUsageOffSession = "off_session"

// defaultProductName is the line item's name on the payment page when the
// caller's CheckoutParams carry no Description.
const defaultProductName = "credit"

// Config is what a deployment needs to sell.
//
// With either secret absent, New returns a refusing adapter: a local run and
// every test that is not about payment want a service that boots and declines
// to sell, not one that panics.
type Config struct {
	// SecretKey is the Stripe API key (sk_test_… or sk_live_…).
	SecretKey string
	// WebhookSecret is this product's own endpoint signing secret (whsec_…).
	// One Stripe account carries several endpoints, one per product, and a
	// product's secret is never shared: a leaked secret then forges deliveries
	// only for the service it belongs to.
	WebhookSecret string
	// Tax turns Stripe Tax on. It is off by default, which keeps the charge
	// equal to exactly what the product quoted before the redirect. Turning it
	// on is what makes the adapter declare pay.CapTax and what lets a caller's
	// pay.TaxAutomatic take effect.
	Tax bool
	// ProductName overrides the line item's name on the payment page when a
	// checkout carries no Description.
	ProductName string

	// backend replaces the HTTP backend the SDK calls. Unexported: it exists so
	// this package's own tests can point the SDK at an httptest server, and
	// widening it to the public surface would invite a caller to reach past the
	// port.
	backend stripe.Backend
}

// Adapter implements pay.Provider over Stripe.
type Adapter struct {
	sc            *stripe.Client
	webhookSecret string
	productName   string
	caps          []pay.Capability
	configured    bool
}

var _ pay.Provider = (*Adapter)(nil)

// New builds an adapter from c.
//
// It never returns nil. With no keys the result refuses every operation with
// pay.ErrUnconfigured, which is the default deployment posture in
// docs/stripe-operations.md: the service boots and nothing sells.
func New(c Config) *Adapter {
	if c.SecretKey == "" || c.WebhookSecret == "" {
		return &Adapter{}
	}
	caps := []pay.Capability{pay.CapCheckout, pay.CapSavedMethod, pay.CapRefund}
	if c.Tax {
		caps = append(caps, pay.CapTax)
	}
	name := c.ProductName
	if name == "" {
		name = defaultProductName
	}
	return &Adapter{
		sc:            newClient(c),
		webhookSecret: c.WebhookSecret,
		productName:   name,
		caps:          caps,
		configured:    true,
	}
}

// newClient builds a per-adapter SDK client.
//
// Per-adapter rather than the SDK's package-level globals: two products in one
// process may hold different keys, and a global key is a data race waiting for
// the second one.
func newClient(c Config) *stripe.Client {
	if c.backend == nil {
		return stripe.NewClient(c.SecretKey)
	}
	return stripe.NewClient(c.SecretKey, stripe.WithBackends(&stripe.Backends{
		API:         c.backend,
		Connect:     c.backend,
		Uploads:     c.backend,
		MeterEvents: c.backend,
	}))
}

// Name reports the processor this adapter speaks to.
func (a *Adapter) Name() pay.Name { return pay.Stripe }

// Has reports a declared capability. An unconfigured adapter declares none.
func (a *Adapter) Has(c pay.Capability) bool { return slices.Contains(a.caps, c) }

// CreateCheckout opens a hosted Checkout Session in payment mode.
//
// One-off and inline: a credit top-up is an arbitrary amount somebody chose, so
// the line item carries price_data rather than a pre-created Price. There is
// nothing in the dashboard to keep in sync with this code, which is the point.
func (a *Adapter) CreateCheckout(ctx context.Context, p pay.CheckoutParams) (pay.Checkout, error) {
	if !a.configured {
		return pay.Checkout{}, pay.ErrUnconfigured
	}
	cur, err := currency(p.Currency)
	if err != nil {
		return pay.Checkout{}, err
	}
	name := p.Description
	if name == "" {
		name = a.productName
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String(p.SuccessURL),
		CancelURL:  stripe.String(p.CancelURL),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency: stripe.String(string(cur)),
				// MinorUp, not MinorDown: a fraction of a cent is charged rather
				// than absorbed, and the bias always favours the platform.
				UnitAmount: stripe.Int64(p.Amount.MinorUp(cur)),
				ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name: stripe.String(name),
				},
			},
		}},
		// Automatic tax stays off unless the deployment turned Stripe Tax on and
		// this checkout asked for it. Off keeps the charge equal to exactly what
		// the product quoted before the redirect.
		AutomaticTax: &stripe.CheckoutSessionCreateAutomaticTaxParams{
			Enabled: stripe.Bool(p.Tax == pay.TaxAutomatic && a.Has(pay.CapTax)),
		},
	}
	// Managed Payments is default-on for new accounts, has no typed field in
	// the SDK version this was first written against, and when left on it
	// demands a product tax code and adds tax on top of the quoted total, so the
	// customer is charged more than the app said. Disabled per session rather
	// than by account setting, because an account default can be changed in the
	// dashboard by somebody who does not know what it breaks. The exact
	// parameter name came from Stripe's own error message.
	params.AddExtra("managed_payments[enabled]", "false")
	if err := bindCustomer(params, p); err != nil {
		return pay.Checkout{}, err
	}
	if p.SaveMethod {
		params.PaymentIntentData = &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			SetupFutureUsage: stripe.String(setupFutureUsageOffSession),
		}
	}
	// Email first, then Meta, so a caller can override the metadata email
	// deliberately and cannot do so by accident.
	params.AddMetadata("email", p.Email)
	for k, v := range p.Meta {
		params.AddMetadata(k, v)
	}
	if p.IdempotencyKey != "" {
		params.SetIdempotencyKey(p.IdempotencyKey)
	}
	sess, err := a.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return pay.Checkout{}, fmt.Errorf("pay/stripe: create checkout session: %w", err)
	}
	return pay.Checkout{URL: sess.URL, SessionID: sess.ID}, nil
}

// bindCustomer attaches the session to a saved customer, or pre-fills the email
// when there is none.
//
// Stripe rejects a session carrying both `customer` and `customer_email`, so
// this is one branch and not two independent assignments. Saving a method in
// payment mode needs a Customer to attach it to, so a session that asks for one
// without a saved customer tells Checkout to create it.
func bindCustomer(params *stripe.CheckoutSessionCreateParams, p pay.CheckoutParams) error {
	if !p.Customer.Zero() {
		if p.Customer.Provider != pay.Stripe {
			return fmt.Errorf("%w: %s", ErrForeignCustomer, p.Customer.Provider)
		}
		params.Customer = stripe.String(p.Customer.ID)
		return nil
	}
	if p.Email != "" {
		params.CustomerEmail = stripe.String(p.Email)
	}
	if p.SaveMethod {
		params.CustomerCreation = stripe.String(string(stripe.CheckoutSessionCustomerCreationAlways))
	}
	return nil
}

// EnsureCustomer resolves a durable Stripe customer for email, creating one if
// the search finds none.
//
// Idempotency rests on the create's idempotency key, not on the search. Stripe
// search is eventually consistent — a customer can take up to a minute to
// become searchable — so a retry moments after the first call may well miss and
// reach the create. A key derived from the email makes that create return the
// original customer rather than mint a second one.
func (a *Adapter) EnsureCustomer(ctx context.Context, email string, meta map[string]string) (pay.CustomerRef, error) {
	if !a.configured {
		return pay.CustomerRef{}, pay.ErrUnconfigured
	}
	if email == "" {
		return pay.CustomerRef{}, errors.New("pay/stripe: EnsureCustomer needs an email")
	}
	found := a.sc.V1Customers.Search(ctx, &stripe.CustomerSearchParams{
		Query:  fmt.Sprintf("email:'%s'", escapeQuery(email)),
		Limit:  stripe.Int64(1),
		Single: true,
	})
	if err := found.Err(); err != nil {
		return pay.CustomerRef{}, fmt.Errorf("pay/stripe: search customers: %w", err)
	}
	if data := found.Data(); len(data) > 0 {
		return pay.CustomerRef{Provider: pay.Stripe, ID: data[0].ID}, nil
	}
	params := &stripe.CustomerCreateParams{Email: stripe.String(email)}
	for k, v := range meta {
		params.AddMetadata(k, v)
	}
	params.SetIdempotencyKey(customerKey(email))
	cust, err := a.sc.V1Customers.Create(ctx, params)
	if err != nil {
		return pay.CustomerRef{}, fmt.Errorf("pay/stripe: create customer: %w", err)
	}
	return pay.CustomerRef{Provider: pay.Stripe, ID: cust.ID}, nil
}

// customerKey is the idempotency key for creating email's customer.
//
// Derived rather than random, and hashed rather than the raw address, so the
// key is stable across processes and restarts without putting a personal
// identifier in Stripe's request log.
func customerKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return "pay-customer-" + hex.EncodeToString(sum[:])
}

// escapeQuery makes email safe inside a single-quoted Stripe search clause.
func escapeQuery(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// ChargeSaved charges a method the customer already authorised, with no page
// and nobody present.
//
// This is what auto-recharge runs on. Three outcomes matter and they are not
// interchangeable: a success credits, a decline is pay.ErrDeclined and must
// never be retried, and an authentication challenge is pay.ChargePending with a
// webhook to follow. Off-session 3-D Secure does not arrive as a 200 with a
// requires_action intent; Stripe returns a 402 card_error whose code is
// authentication_required, so that code is separated before the generic decline
// mapping. Getting that wrong turns every EU challenge into a permanent
// decline.
func (a *Adapter) ChargeSaved(ctx context.Context, p pay.SavedChargeParams) (pay.Charge, error) {
	if !a.configured {
		return pay.Charge{}, pay.ErrUnconfigured
	}
	if p.IdempotencyKey == "" {
		return pay.Charge{}, errors.New("pay/stripe: ChargeSaved needs an idempotency key; nobody is present to notice a double charge")
	}
	if p.Customer.Zero() {
		return pay.Charge{}, errors.New("pay/stripe: ChargeSaved needs a customer")
	}
	if p.Customer.Provider != pay.Stripe {
		return pay.Charge{}, fmt.Errorf("%w: %s", ErrForeignCustomer, p.Customer.Provider)
	}
	cur, err := currency(p.Currency)
	if err != nil {
		return pay.Charge{}, err
	}
	params := &stripe.PaymentIntentCreateParams{
		Amount:     stripe.Int64(p.Amount.MinorUp(cur)),
		Currency:   stripe.String(string(cur)),
		Customer:   stripe.String(p.Customer.ID),
		Confirm:    stripe.Bool(true),
		OffSession: stripe.Bool(true),
	}
	if p.Description != "" {
		params.Description = stripe.String(p.Description)
	}
	if p.Method != "" {
		params.PaymentMethod = stripe.String(p.Method)
	}
	for k, v := range p.Meta {
		params.AddMetadata(k, v)
	}
	params.SetIdempotencyKey(p.IdempotencyKey)
	pi, err := a.sc.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		return classifyChargeError(err)
	}
	return pay.Charge{Ref: pi.ID, Status: chargeStatus(pi.Status)}, nil
}

// classifyChargeError separates the three failures a caller must treat
// differently: an authentication challenge, a decline, and everything else.
func classifyChargeError(err error) (pay.Charge, error) {
	var se *stripe.Error
	if !errors.As(err, &se) || se.Type != stripe.ErrorTypeCard {
		// A transport or API failure. Not a decline: the charge may or may not
		// have happened, and the idempotency key is what makes a retry safe.
		return pay.Charge{}, fmt.Errorf("pay/stripe: charge saved method: %w", err)
	}
	ref := ""
	if se.PaymentIntent != nil {
		ref = se.PaymentIntent.ID
	}
	if se.Code == stripe.ErrorCodeAuthenticationRequired {
		// The 3-D Secure path. Not a failure and not a credit: the customer has
		// to authenticate, and the outcome arrives as a webhook.
		return pay.Charge{Ref: ref, Status: pay.ChargePending}, nil
	}
	// Ref rides along so a caller can record which attempt was refused, but the
	// error is what decides: pay.ErrDeclined must never be retried.
	return pay.Charge{Ref: ref, Status: pay.ChargeFailed}, fmt.Errorf("pay/stripe: %w: %s", pay.ErrDeclined, se.Msg)
}

// chargeStatus reduces a PaymentIntent's status to the port's three outcomes.
func chargeStatus(s stripe.PaymentIntentStatus) pay.ChargeStatus {
	switch s {
	case stripe.PaymentIntentStatusSucceeded:
		return pay.ChargeSucceeded
	case stripe.PaymentIntentStatusRequiresAction,
		stripe.PaymentIntentStatusRequiresConfirmation,
		stripe.PaymentIntentStatusRequiresCapture,
		stripe.PaymentIntentStatusProcessing:
		// Money is not in hand. Wait for the webhook rather than credit.
		return pay.ChargePending
	default:
		return pay.ChargeFailed
	}
}

// currency resolves the presentment currency, defaulting to USD.
func currency(c money.Currency) (money.Currency, error) {
	if c == "" {
		return money.USD, nil
	}
	if !c.Valid() {
		return "", fmt.Errorf("pay/stripe: %w: %s", money.ErrCurrency, c)
	}
	return c, nil
}
