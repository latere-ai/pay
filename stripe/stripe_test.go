package stripe

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"latere.ai/x/pay"
)

// An unconfigured deployment is the default posture: the service boots, every
// operation refuses, and nothing sells. It is also what every test in every
// product that is not about payment runs against, so it has to be the one
// behaviour that cannot be got wrong.
func TestNew_WithoutBothSecretsTheAdapterRefusesEverything(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no keys at all", Config{}},
		{"no api key", Config{WebhookSecret: testWebhookSecret}},
		{"no webhook secret", Config{SecretKey: testSecretKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(tc.cfg)
			if a == nil {
				t.Fatal("New returned nil; a caller would dereference a missing processor")
			}
			// Identity still answers: a deployment that does not sell still
			// knows which processor it would have been.
			if a.Name() != pay.Stripe {
				t.Errorf("Name = %q, want stripe even unconfigured", a.Name())
			}
			for _, c := range []pay.Capability{pay.CapCheckout, pay.CapSavedMethod, pay.CapRefund, pay.CapTax} {
				if a.Has(c) {
					t.Errorf("an unconfigured adapter declares %s", c)
				}
			}

			ctx := context.Background()
			if _, err := a.CreateCheckout(ctx, topUp()); !errors.Is(err, pay.ErrUnconfigured) {
				t.Errorf("CreateCheckout = %v, want ErrUnconfigured", err)
			}
			if _, err := a.EnsureCustomer(ctx, "buyer@example.test", nil); !errors.Is(err, pay.ErrUnconfigured) {
				t.Errorf("EnsureCustomer = %v, want ErrUnconfigured", err)
			}
			if _, err := a.ChargeSaved(ctx, recharge()); !errors.Is(err, pay.ErrUnconfigured) {
				t.Errorf("ChargeSaved = %v, want ErrUnconfigured", err)
			}
			payload := eventPayload(t, eventSessionCompleted, paidSession())
			if _, err := a.ParseWebhook(payload, signedNow(payload)); !errors.Is(err, pay.ErrUnconfigured) {
				t.Errorf("ParseWebhook = %v, want ErrUnconfigured", err)
			}
			// ErrUnconfigured is not a failure: the mounted endpoint
			// acknowledges rather than asking Stripe to retry forever.
			rec := serveWebhook(t, a, payload)
			if rec.Code != http.StatusOK {
				t.Errorf("the webhook endpoint answered %d, want 200 so the delivery stops being retried", rec.Code)
			}
		})
	}
}

// TestNew_WithBothSecretsBuildsAConfiguredAdapter covers the production
// construction path, which points at Stripe's real API host. Nothing is called,
// so no network is touched.
func TestNew_WithBothSecretsBuildsAConfiguredAdapter(t *testing.T) {
	a := New(Config{SecretKey: testSecretKey, WebhookSecret: testWebhookSecret})
	if !a.configured {
		t.Fatal("an adapter with both secrets is not configured")
	}
	if a.sc == nil {
		t.Error("no SDK client was built")
	}
	if !a.Has(pay.CapCheckout) {
		t.Error("a configured adapter does not declare CapCheckout")
	}
	// A delivery still has to authenticate before anything else happens.
	if _, err := a.ParseWebhook([]byte(`{}`), http.Header{}); !errors.Is(err, pay.ErrBadSignature) {
		t.Errorf("ParseWebhook with no signature = %v, want ErrBadSignature", err)
	}
}

// TestWebhookHandler_CreditsThroughTheAdapter is the end-to-end path a product
// mounts: bytes off the wire, through the port's handler, into one verified
// event.
func TestWebhookHandler_CreditsThroughTheAdapter(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())

	var got pay.Event
	h := pay.WebhookHandler(a, func(_ context.Context, e pay.Event) error {
		got = e
		return nil
	})
	rec := serveHandler(t, h, a, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got.Kind != pay.KindPaid || got.Ref != "pi_test_1" {
		t.Errorf("event = %+v, want a paid pi_test_1", got)
	}

	// And a forged delivery posts nothing, with a 400 so Stripe stops.
	got = pay.Event{}
	rec = serveUnsigned(t, h, payload)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status on a forged delivery = %d, want 400", rec.Code)
	}
	if got.Kind != pay.KindIgnored {
		t.Errorf("a forged delivery reached the handler as %+v", got)
	}
}
