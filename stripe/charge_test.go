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

const intentsPath = "/v1/payment_intents"

func recharge() pay.SavedChargeParams {
	return pay.SavedChargeParams{
		Customer:       pay.CustomerRef{Provider: pay.Stripe, ID: "cus_test_1"},
		Amount:         5 * money.Dollar,
		Currency:       money.USD,
		Description:    "auto-recharge",
		Meta:           map[string]string{"credited_micro": "4700000"},
		IdempotencyKey: "recharge-7",
	}
}

func TestChargeSaved_ShapesTheOffSessionRequest(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, intentsPath, map[string]any{
		"id": "pi_ok", "object": "payment_intent", "status": "succeeded",
	})
	a := newAdapter(t, s)

	got, err := a.ChargeSaved(context.Background(), recharge())
	if err != nil {
		t.Fatalf("ChargeSaved: %v", err)
	}
	if got.Ref != "pi_ok" || got.Status != pay.ChargeSucceeded {
		t.Errorf("charge = %+v, want pi_ok succeeded", got)
	}
	c := s.calledOnce(http.MethodPost, intentsPath)
	for _, tc := range []struct{ key, want string }{
		{"amount", "500"},
		{"currency", "usd"},
		{"customer", "cus_test_1"},
		{"off_session", "true"},
		{"confirm", "true"},
		{"description", "auto-recharge"},
		{"metadata[credited_micro]", "4700000"},
	} {
		if got := c.form.Get(tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
	if c.idempotencyKey != "recharge-7" {
		t.Errorf("Idempotency-Key = %q, want recharge-7", c.idempotencyKey)
	}
}

func TestChargeSaved_UsesTheNamedMethodWhenThereIsOne(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, intentsPath, map[string]any{
		"id": "pi_ok", "object": "payment_intent", "status": "succeeded",
	})
	a := newAdapter(t, s)

	p := recharge()
	p.Method = "pm_card_visa"
	if _, err := a.ChargeSaved(context.Background(), p); err != nil {
		t.Fatalf("ChargeSaved: %v", err)
	}
	if got := s.calledOnce(http.MethodPost, intentsPath).form.Get("payment_method"); got != "pm_card_visa" {
		t.Errorf("payment_method = %q, want pm_card_visa", got)
	}
}

func TestChargeSaved_OmitsTheMethodSoStripeUsesTheDefault(t *testing.T) {
	s := newStub(t)
	s.json(http.MethodPost, intentsPath, map[string]any{
		"id": "pi_ok", "object": "payment_intent", "status": "succeeded",
	})
	a := newAdapter(t, s)

	if _, err := a.ChargeSaved(context.Background(), recharge()); err != nil {
		t.Fatalf("ChargeSaved: %v", err)
	}
	if s.calledOnce(http.MethodPost, intentsPath).form.Has("payment_method") {
		t.Error("payment_method sent for a charge that named none")
	}
}

// A decline is terminal. Retrying one is how a card ends up locked and a
// customer ends up angry, so the mapping is checked and so is the call count.
func TestChargeSaved_DeclineIsErrDeclinedAndIsNotRetried(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPost, intentsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusPaymentRequired, stripe.ErrorTypeCard, stripe.ErrorCodeCardDeclined,
			"Your card was declined.", func(body map[string]any) {
				body["decline_code"] = "insufficient_funds"
				body["payment_intent"] = map[string]any{"id": "pi_declined", "object": "payment_intent", "status": "requires_payment_method"}
			})
	})
	a := newAdapter(t, s)

	got, err := a.ChargeSaved(context.Background(), recharge())
	if !errors.Is(err, pay.ErrDeclined) {
		t.Fatalf("ChargeSaved on a card_error = %v, want ErrDeclined", err)
	}
	if got.Status != pay.ChargeFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Ref != "pi_declined" {
		t.Errorf("Ref = %q, want pi_declined so the attempt can be recorded", got.Ref)
	}
	s.calledOnce(http.MethodPost, intentsPath)
}

// Off-session 3-D Secure does not arrive as a 200 with requires_action: Stripe
// answers 402 with a card_error whose code is authentication_required. Mapping
// that to a decline turns every EU challenge into a permanent failure.
func TestChargeSaved_AuthenticationRequiredIsPendingNotDeclined(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPost, intentsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusPaymentRequired, stripe.ErrorTypeCard, stripe.ErrorCodeAuthenticationRequired,
			"This payment requires authentication.", func(body map[string]any) {
				body["payment_intent"] = map[string]any{"id": "pi_3ds", "object": "payment_intent", "status": "requires_action"}
			})
	})
	a := newAdapter(t, s)

	got, err := a.ChargeSaved(context.Background(), recharge())
	if err != nil {
		t.Fatalf("ChargeSaved on authentication_required = %v, want no error", err)
	}
	if got.Status != pay.ChargePending {
		t.Errorf("status = %q, want pending; the webhook decides", got.Status)
	}
	if got.Ref != "pi_3ds" {
		t.Errorf("Ref = %q, want pi_3ds", got.Ref)
	}
}

func TestChargeSaved_AChallengeWithNoIntentStillReportsPending(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPost, intentsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusPaymentRequired, stripe.ErrorTypeCard, stripe.ErrorCodeAuthenticationRequired,
			"This payment requires authentication.")
	})
	a := newAdapter(t, s)

	got, err := a.ChargeSaved(context.Background(), recharge())
	if err != nil || got.Status != pay.ChargePending {
		t.Fatalf("charge = %+v, err = %v; want pending and no error", got, err)
	}
}

func TestChargeSaved_TransportFailureIsNotADecline(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPost, intentsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeStripeError(w, http.StatusInternalServerError, stripe.ErrorTypeAPI, "", "something went wrong")
	})
	a := newAdapter(t, s)

	got, err := a.ChargeSaved(context.Background(), recharge())
	if err == nil {
		t.Fatal("ChargeSaved on a 500 returned no error")
	}
	// The distinction is the whole point: this one is safe to retry under the
	// same idempotency key, and a decline never is.
	if errors.Is(err, pay.ErrDeclined) {
		t.Errorf("a 500 mapped to ErrDeclined: %v", err)
	}
	if got != (pay.Charge{}) {
		t.Errorf("charge = %+v, want the zero value", got)
	}
}

func TestChargeSaved_MapsEveryIntentStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   pay.ChargeStatus
	}{
		{"succeeded", pay.ChargeSucceeded},
		{"requires_action", pay.ChargePending},
		{"requires_confirmation", pay.ChargePending},
		{"requires_capture", pay.ChargePending},
		{"processing", pay.ChargePending},
		{"requires_payment_method", pay.ChargeFailed},
		{"canceled", pay.ChargeFailed},
	} {
		t.Run(tc.status, func(t *testing.T) {
			s := newStub(t)
			s.json(http.MethodPost, intentsPath, map[string]any{
				"id": "pi_x", "object": "payment_intent", "status": tc.status,
			})
			a := newAdapter(t, s)

			got, err := a.ChargeSaved(context.Background(), recharge())
			if err != nil {
				t.Fatalf("ChargeSaved: %v", err)
			}
			if got.Status != tc.want {
				t.Errorf("status %q mapped to %q, want %q", tc.status, got.Status, tc.want)
			}
		})
	}
}

func TestChargeSaved_RefusesWhatItCannotChargeSafely(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     pay.SavedChargeParams
		check func(*testing.T, error)
	}{
		{
			name: "no idempotency key",
			p:    func() pay.SavedChargeParams { p := recharge(); p.IdempotencyKey = ""; return p }(),
		},
		{
			name: "no customer",
			p:    func() pay.SavedChargeParams { p := recharge(); p.Customer = pay.CustomerRef{}; return p }(),
		},
		{
			name: "another processor's customer",
			p: func() pay.SavedChargeParams {
				p := recharge()
				p.Customer = pay.CustomerRef{Provider: pay.PayPal, ID: "BILLING-9"}
				return p
			}(),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrForeignCustomer) {
					t.Errorf("err = %v, want ErrForeignCustomer", err)
				}
			},
		},
		{
			name: "a currency money does not know",
			p:    func() pay.SavedChargeParams { p := recharge(); p.Currency = "gbp"; return p }(),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, money.ErrCurrency) {
					t.Errorf("err = %v, want money.ErrCurrency", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			a := newAdapter(t, s)

			_, err := a.ChargeSaved(context.Background(), tc.p)
			if err == nil {
				t.Fatal("ChargeSaved returned no error")
			}
			if tc.check != nil {
				tc.check(t, err)
			}
			if n := len(s.callsTo(http.MethodPost, intentsPath)); n != 0 {
				t.Errorf("the charge reached Stripe anyway: %d calls", n)
			}
		})
	}
}
