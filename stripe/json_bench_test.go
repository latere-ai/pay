// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package stripe

import (
	"fmt"
	"testing"

	"latere.ai/x/pay"
)

// benchSession is a paid checkout session padded with the line items, address
// and consent members a real delivery carries, which the adapter ignores.
func benchSession() map[string]any {
	s := paidSession()
	items := make([]map[string]any, 0, 3)
	for i := range 3 {
		items = append(items, map[string]any{
			"id": fmt.Sprintf("li_test_%d", i), "object": "item", "amount_total": 2000,
			"currency": "usd", "description": "Latere credits, top-up of 20 USD",
			"price": map[string]any{"id": "price_test", "unit_amount": 2000, "type": "one_time"},
		})
	}
	s["line_items"] = map[string]any{"object": "list", "data": items, "has_more": false}
	s["customer_details"] = map[string]any{
		"email": "buyer@example.test", "name": "A. Buyer",
		"address": map[string]any{"country": "DE", "postal_code": "10115", "city": "Berlin", "line1": "Example Str. 1"},
	}
	s["success_url"] = "https://platform.latere.ai/billing?session={CHECKOUT_SESSION_ID}"
	s["cancel_url"] = "https://platform.latere.ai/billing"
	s["livemode"] = false
	s["created"] = 1793750400
	return s
}

// BenchmarkJSONParseWebhook stands for the Stripe webhook endpoint: every
// delivery is signature-verified, its envelope decoded by the SDK, and its
// object decoded again into the adapter's own shape. Each sub-benchmark is one
// delivery kind.
func BenchmarkJSONParseWebhook(b *testing.B) {
	a := New(Config{SecretKey: testSecretKey, WebhookSecret: testWebhookSecret})
	for _, tc := range []struct {
		name string
		typ  string
		obj  any
		kind pay.Kind
	}{
		{"checkout_session_paid", eventSessionCompleted, benchSession(), pay.KindPaid},
		{"charge_refunded", eventChargeRefunded, map[string]any{
			"id": "ch_test_1", "object": "charge", "currency": "usd", "amount_refunded": 2000,
			"payment_intent": "pi_test_1", "metadata": map[string]string{"email": "buyer@example.test"},
			"refunds": map[string]any{"data": []map[string]any{
				{"id": "re_test_2", "amount": 500, "created": 1793750500, "currency": "usd"},
				{"id": "re_test_1", "amount": 1500, "created": 1793750400, "currency": "usd"},
			}},
		}, pay.KindRefunded},
		{"dispute_created", eventDisputeCreated, map[string]any{
			"id": "dp_test_1", "object": "dispute", "amount": 2000, "currency": "usd", "payment_intent": "pi_test_1",
		}, pay.KindDisputed},
		{"payment_failed", eventPaymentFailed, map[string]any{
			"id": "pi_test_9", "object": "payment_intent", "metadata": map[string]string{"origin": "auto_recharge"},
		}, pay.KindPaymentFailed},
	} {
		payload := eventPayload(b, tc.typ, tc.obj)
		hdr := signedNow(payload)
		ev, err := a.ParseWebhook(payload, hdr)
		if err != nil || ev.Kind != tc.kind {
			b.Fatalf("%s: setup: kind %q, err %v; want %q", tc.name, ev.Kind, err, tc.kind)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				if _, err := a.ParseWebhook(payload, hdr); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
