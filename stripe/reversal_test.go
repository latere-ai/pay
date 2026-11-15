// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package stripe

import (
	"testing"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

func TestParseWebhook_AWonDisputeIsKindDisputeWon(t *testing.T) {
	a := newAdapter(t, newStub(t))
	for _, tc := range []struct {
		name  string
		event string
		obj   map[string]any
		want  pay.Kind
	}{
		{"closed as won", eventDisputeClosed, map[string]any{"id": "dp_1", "object": "dispute", "status": "won", "amount": 2000, "currency": "usd", "payment_intent": "pi_1"}, pay.KindDisputeWon},
		{"funds reinstated", eventDisputeReinstated, map[string]any{"id": "dp_1", "object": "dispute", "status": "won", "amount": 2000, "currency": "usd", "payment_intent": "pi_1"}, pay.KindDisputeWon},
		{"closed as lost", eventDisputeClosed, map[string]any{"id": "dp_1", "object": "dispute", "status": "lost", "amount": 2000, "currency": "usd", "payment_intent": "pi_1"}, pay.KindIgnored},
		{"closed with a warning", eventDisputeClosed, map[string]any{"id": "dp_1", "object": "dispute", "status": "warning_closed", "payment_intent": "pi_1"}, pay.KindIgnored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := eventPayload(t, tc.event, tc.obj)
			ev, err := a.ParseWebhook(payload, signedNow(payload))
			if err != nil {
				t.Fatalf("ParseWebhook: %v", err)
			}
			if ev.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", ev.Kind, tc.want)
			}
			if tc.want != pay.KindDisputeWon {
				return
			}
			if ev.Ref != "pi_1" || ev.ReversalRef != "dp_1" || ev.Gross != 20*money.Dollar {
				t.Errorf("event = ref %q reversal %q gross %v, want pi_1, the dispute id, $20", ev.Ref, ev.ReversalRef, ev.Gross)
			}
		})
	}
}
