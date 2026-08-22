package stripe

import (
	"context"
	"errors"
	"net/http"
	"testing"

	stripe "github.com/stripe/stripe-go/v85"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

const sessionsPath = "/v1/checkout/sessions"

// sessionCreated is what Stripe answers a create with.
var sessionCreated = map[string]any{
	"id":     "cs_test_created",
	"object": "checkout.session",
	"url":    "https://checkout.stripe.com/c/pay/cs_test_created",
}

func topUp() pay.CheckoutParams {
	return pay.CheckoutParams{
		Email:       "buyer@example.test",
		Amount:      20 * money.Dollar,
		Currency:    money.USD,
		Description: "wallet credit",
		SuccessURL:  "https://product.test/wallet?ok=1",
		CancelURL:   "https://product.test/wallet",
		Meta:        map[string]string{"credited_micro": "18700000"},
		Tax:         pay.TaxNone,
	}
}

func TestCreateCheckout_ShapesTheInlinePriceRequest(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	got, err := a.CreateCheckout(context.Background(), topUp())
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if got.URL != sessionCreated["url"] {
		t.Errorf("URL = %q, want %q", got.URL, sessionCreated["url"])
	}
	if got.SessionID != "cs_test_created" {
		t.Errorf("SessionID = %q, want cs_test_created", got.SessionID)
	}

	// The nested form encoding is the whole point of this test: there are no
	// pre-created Prices, so the amount only reaches Stripe through price_data.
	form := s.calledOnce(http.MethodPost, sessionsPath).form
	for _, tc := range []struct{ key, want string }{
		{"mode", "payment"},
		{"line_items[0][quantity]", "1"},
		{"line_items[0][price_data][currency]", "usd"},
		{"line_items[0][price_data][unit_amount]", "2000"},
		{"line_items[0][price_data][product_data][name]", "wallet credit"},
		{"success_url", "https://product.test/wallet?ok=1"},
		{"cancel_url", "https://product.test/wallet"},
		{"customer_email", "buyer@example.test"},
		{"metadata[email]", "buyer@example.test"},
		{"metadata[credited_micro]", "18700000"},
		{"automatic_tax[enabled]", "false"},
	} {
		if got := form.Get(tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
	// A pre-created Price would silently ignore the amount somebody chose.
	if form.Has("line_items[0][price]") {
		t.Errorf("line_items[0][price] was sent; a wallet top-up has no SKU: %v", form)
	}
	if form.Has("setup_future_usage") || form.Has("payment_intent_data[setup_future_usage]") {
		t.Errorf("setup_future_usage sent for a checkout that did not ask to save a method")
	}
}

func TestCreateCheckout_RoundsAFractionOfACentUp(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.Amount = 20*money.Dollar + 1 // $20.000001
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	// Rounded away from zero: the platform never absorbs the fraction.
	if got := s.calledOnce(http.MethodPost, sessionsPath).form.Get("line_items[0][price_data][unit_amount]"); got != "2001" {
		t.Errorf("unit_amount = %q, want 2001", got)
	}
}

func TestCreateCheckout_SaveMethodAsksForAnOffSessionMandate(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.SaveMethod = true
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	form := s.calledOnce(http.MethodPost, sessionsPath).form
	if got := form.Get("payment_intent_data[setup_future_usage]"); got != "off_session" {
		t.Errorf("payment_intent_data[setup_future_usage] = %q, want off_session", got)
	}
	// Payment mode needs a Customer to attach the method to, or auto-recharge
	// has nothing to charge later.
	if got := form.Get("customer_creation"); got != "always" {
		t.Errorf("customer_creation = %q, want always", got)
	}
}

func TestCreateCheckout_BindsASavedCustomerInsteadOfTheEmail(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.Customer = pay.CustomerRef{Provider: pay.Stripe, ID: "cus_test_9"}
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	form := s.calledOnce(http.MethodPost, sessionsPath).form
	if got := form.Get("customer"); got != "cus_test_9" {
		t.Errorf("customer = %q, want cus_test_9", got)
	}
	// Stripe rejects a session carrying both.
	if form.Has("customer_email") {
		t.Errorf("customer_email sent alongside customer: %v", form)
	}
}

func TestCreateCheckout_RefusesAnotherProcessorsCustomer(t *testing.T) {
	s := newStub(t)
	a := newAdapter(t, s)

	p := topUp()
	p.Customer = pay.CustomerRef{Provider: pay.PayPal, ID: "BILLING-9"}
	if _, err := a.CreateCheckout(context.Background(), p); !errors.Is(err, ErrForeignCustomer) {
		t.Fatalf("CreateCheckout with a PayPal handle = %v, want ErrForeignCustomer", err)
	}
	if n := len(s.callsTo(http.MethodPost, sessionsPath)); n != 0 {
		t.Errorf("a foreign handle reached Stripe: %d calls", n)
	}
}

func TestCreateCheckout_SendsTheIdempotencyKey(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.IdempotencyKey = "top-up-42"
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if got := s.calledOnce(http.MethodPost, sessionsPath).idempotencyKey; got != "top-up-42" {
		t.Errorf("Idempotency-Key = %q, want top-up-42", got)
	}
}

func TestCreateCheckout_WithoutACallerKeyEachAttemptIsItsOwn(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	for range 2 {
		if _, err := a.CreateCheckout(context.Background(), topUp()); err != nil {
			t.Fatalf("CreateCheckout: %v", err)
		}
	}
	calls := s.callsTo(http.MethodPost, sessionsPath)
	// stripe-go mints a key per attempt so its own network retries are safe.
	// Two separate creates must not share one: a person who clicks twice is
	// buying twice, and collapsing that would lose a purchase.
	if calls[0].idempotencyKey == "" || calls[1].idempotencyKey == "" {
		t.Fatalf("an unkeyed create sent no Idempotency-Key: %q, %q", calls[0].idempotencyKey, calls[1].idempotencyKey)
	}
	if calls[0].idempotencyKey == calls[1].idempotencyKey {
		t.Errorf("two unkeyed creates shared the key %q", calls[0].idempotencyKey)
	}
}

func TestCreateCheckout_TheCallersKeyIsSentOnEveryAttempt(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.IdempotencyKey = "top-up-42"
	for range 2 {
		if _, err := a.CreateCheckout(context.Background(), p); err != nil {
			t.Fatalf("CreateCheckout: %v", err)
		}
	}
	for i, c := range s.callsTo(http.MethodPost, sessionsPath) {
		if c.idempotencyKey != "top-up-42" {
			t.Errorf("attempt %d sent Idempotency-Key %q, want top-up-42", i, c.idempotencyKey)
		}
	}
}

func TestCreateCheckout_AutomaticTaxOnlyWhenTheDeploymentHasIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		taxable bool
		mode    pay.TaxMode
		want    string
	}{
		{"tax off, checkout asks for none", false, pay.TaxNone, "false"},
		{"tax off, checkout asks for automatic", false, pay.TaxAutomatic, "false"},
		{"tax on, checkout asks for none", true, pay.TaxNone, "false"},
		{"tax on, checkout asks for automatic", true, pay.TaxAutomatic, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.json(http.MethodPost, sessionsPath, sessionCreated)
			mutate := []func(*Config){}
			if tc.taxable {
				mutate = append(mutate, withTax)
			}
			a := newAdapter(t, s, mutate...)

			p := topUp()
			p.Tax = tc.mode
			if _, err := a.CreateCheckout(context.Background(), p); err != nil {
				t.Fatalf("CreateCheckout: %v", err)
			}
			if got := s.calledOnce(http.MethodPost, sessionsPath).form.Get("automatic_tax[enabled]"); got != tc.want {
				t.Errorf("automatic_tax[enabled] = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCreateCheckout_NamesTheLineItemWhenThereIsNoDescription(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s, func(c *Config) { c.ProductName = "lux credits" })

	p := topUp()
	p.Description = ""
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if got := s.calledOnce(http.MethodPost, sessionsPath).form.Get("line_items[0][price_data][product_data][name]"); got != "lux credits" {
		t.Errorf("product_data[name] = %q, want lux credits", got)
	}
}

func TestCreateCheckout_FallsBackToADefaultProductName(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.Description = ""
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if got := s.calledOnce(http.MethodPost, sessionsPath).form.Get("line_items[0][price_data][product_data][name]"); got != defaultProductName {
		t.Errorf("product_data[name] = %q, want %q", got, defaultProductName)
	}
}

func TestCreateCheckout_DefaultsTheCurrencyToUSD(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, sessionsPath, sessionCreated)
	a := newAdapter(t, s)

	p := topUp()
	p.Currency = ""
	if _, err := a.CreateCheckout(context.Background(), p); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if got := s.calledOnce(http.MethodPost, sessionsPath).form.Get("line_items[0][price_data][currency]"); got != "usd" {
		t.Errorf("currency = %q, want usd", got)
	}
}

func TestCreateCheckout_RefusesACurrencyMoneyDoesNotKnow(t *testing.T) {
	s := newStub(t)
	a := newAdapter(t, s)

	p := topUp()
	p.Currency = "gbp"
	if _, err := a.CreateCheckout(context.Background(), p); !errors.Is(err, money.ErrCurrency) {
		t.Fatalf("CreateCheckout in gbp = %v, want money.ErrCurrency", err)
	}
}

func TestCreateCheckout_ReportsATransportFailure(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPost, sessionsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusServiceUnavailable, stripe.ErrorTypeAPI, "", "the gateway is down")
	})
	a := newAdapter(t, s)

	_, err := a.CreateCheckout(context.Background(), topUp())
	if err == nil {
		t.Fatal("CreateCheckout on a 503 returned no error")
	}
	// A transport failure is not a decline and not a misconfiguration; the
	// caller retries it, which is only true if it maps to neither sentinel.
	if errors.Is(err, pay.ErrDeclined) || errors.Is(err, pay.ErrUnconfigured) {
		t.Errorf("a 503 mapped to a payment sentinel: %v", err)
	}
}
