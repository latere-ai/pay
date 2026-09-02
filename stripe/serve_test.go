// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package stripe

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"latere.ai/x/pay"
)

// The endpoint helpers. A product mounts pay.WebhookHandler over this adapter,
// so the status codes are part of the contract: 400 refuses a delivery Stripe
// must not retry, 200 acknowledges one nothing can be done about.

func serveHandler(t *testing.T, h http.Handler, a *Adapter, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
	if a.configured {
		r.Header = signedHeader(a.webhookSecret, payload, time.Now())
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func serveUnsigned(t *testing.T, h http.Handler, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func serveWebhook(t *testing.T, a *Adapter, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	return serveHandler(t, pay.WebhookHandler(a, nil), a, payload)
}
