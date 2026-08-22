package pay_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

// post drives the handler and returns the status it answered with.
func post(t *testing.T, h http.Handler, body string, header http.Header) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhooks/pay", strings.NewReader(body))
	maps.Copy(r.Header, header)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func signed() http.Header {
	h := http.Header{}
	h.Set(pay.MemHeader, "shh")
	return h
}

// The status codes are the contract: getting them wrong loses a purchase or
// double-credits one, so every branch is pinned.
func TestWebhookHandlerStatusCodes(t *testing.T) {
	paid := string(pay.MemEvent(pay.KindPaid, "pi_1", 20*money.Dollar, nil))

	t.Run("a verified event the handler accepts is 200", func(t *testing.T) {
		var got pay.Event
		h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, func(_ context.Context, e pay.Event) error {
			got = e
			return nil
		})
		if code := post(t, h, paid, signed()); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
		if got.Ref != "pi_1" {
			t.Errorf("handler saw %+v", got)
		}
	})

	t.Run("a bad signature is 400 and never reaches the handler", func(t *testing.T) {
		called := false
		h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, func(context.Context, pay.Event) error {
			called = true
			return nil
		})
		if code := post(t, h, paid, http.Header{}); code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
		if called {
			t.Error("an unauthenticated delivery reached the handler")
		}
	})

	t.Run("an unconfigured processor is acknowledged, not retried", func(t *testing.T) {
		h := pay.WebhookHandler(&pay.MemProvider{Unconfigured: true}, func(context.Context, pay.Event) error {
			t.Error("handler ran for an unconfigured processor")
			return nil
		})
		if code := post(t, h, paid, signed()); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("a nil provider is acknowledged", func(t *testing.T) {
		h := pay.WebhookHandler(nil, nil)
		if code := post(t, h, paid, signed()); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("an ignored kind never reaches the handler", func(t *testing.T) {
		called := false
		h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, func(context.Context, pay.Event) error {
			called = true
			return nil
		})
		body := string(pay.MemEvent(pay.KindIgnored, "", 0, nil))
		if code := post(t, h, body, signed()); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
		if called {
			t.Error("an ignored event reached the handler")
		}
	})

	t.Run("a nil handler still acknowledges", func(t *testing.T) {
		h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, nil)
		if code := post(t, h, paid, signed()); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("a handler error is 500 so the processor retries", func(t *testing.T) {
		h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, func(context.Context, pay.Event) error {
			return errors.New("ledger unavailable")
		})
		if code := post(t, h, paid, signed()); code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", code)
		}
	})

	t.Run("a malformed payload is 400", func(t *testing.T) {
		h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, nil)
		if code := post(t, h, "{not json", signed()); code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
	})
}

// The endpoint is unauthenticated until the signature is checked, so the body
// read must be bounded.
func TestWebhookHandlerBoundsTheBody(t *testing.T) {
	h := pay.WebhookHandler(&pay.MemProvider{}, nil, pay.WithMaxBody(8))
	// A body longer than the cap is truncated, so it cannot parse, and the
	// handler refuses rather than buffering it all.
	if code := post(t, h, strings.Repeat("x", 4096), http.Header{}); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	// A non-positive cap is ignored rather than making the endpoint read nothing.
	ok := pay.WebhookHandler(&pay.MemProvider{}, nil, pay.WithMaxBody(0))
	if code := post(t, ok, string(pay.MemEvent(pay.KindPaid, "r", 0, nil)), http.Header{}); code != http.StatusOK {
		t.Errorf("status = %d, want 200 with the default cap", code)
	}
}

// errReader fails mid-read, which is the transport error the handler must turn
// into a 400 rather than a panic.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestWebhookHandlerUnreadableBody(t *testing.T) {
	h := pay.WebhookHandler(&pay.MemProvider{}, nil)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/pay", errReader{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWebhookHandlerLogsDroppedDeliveries(t *testing.T) {
	var sb strings.Builder
	log := slog.New(slog.NewTextHandler(&sb, nil))
	h := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, nil, pay.WithLogger(log))
	if code := post(t, h, "{}", http.Header{}); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(sb.String(), "webhook delivery dropped") {
		t.Errorf("nothing logged for a dropped delivery: %q", sb.String())
	}
	// Without a logger the same path must stay silent rather than panic.
	quiet := pay.WebhookHandler(&pay.MemProvider{Secret: "shh"}, nil)
	if code := post(t, quiet, "{}", http.Header{}); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

// The whole point of the port: a product's money path runs end to end with no
// processor. Buy, credit, refund, reverse.
func TestOfflineMoneyPath(t *testing.T) {
	m := &pay.MemProvider{Secret: "shh"}
	spread := money.Spread{Bps: 500, FixedMicro: 300_000}
	gross := 20 * money.Dollar
	credited := spread.Credited(gross)

	out, err := m.CreateCheckout(context.Background(), pay.CheckoutParams{
		Email:    "buyer@example.test",
		Amount:   gross,
		Currency: money.USD,
		Meta:     map[string]string{"credited_micro": credited.String(money.USD)},
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if out.URL == "" {
		t.Fatal("no payment page")
	}

	var balance money.Micro
	h := pay.WebhookHandler(m, func(_ context.Context, e pay.Event) error {
		switch e.Kind {
		case pay.KindPaid:
			balance += credited
		case pay.KindRefunded, pay.KindDisputed:
			balance -= credited
		}
		return nil
	})
	if code := post(t, h, string(pay.MemEvent(pay.KindPaid, "pi_1", gross, nil)), signed()); code != http.StatusOK {
		t.Fatalf("paid delivery status = %d", code)
	}
	if balance != 18_700_000 {
		t.Fatalf("balance after purchase = %d, want $18.70", balance)
	}
	if code := post(t, h, string(pay.MemEvent(pay.KindRefunded, "pi_1", gross, nil)), signed()); code != http.StatusOK {
		t.Fatalf("refund delivery status = %d", code)
	}
	if balance != 0 {
		t.Fatalf("balance after refund = %d, want 0", balance)
	}
}

var _ = io.Discard
