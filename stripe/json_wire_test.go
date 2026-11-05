// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package stripe

import (
	"encoding/json/v2"
	"testing"
)

// Stripe payloads are read leniently: a repeated key, another letter case and
// invalid UTF-8 are accepted.
func TestWebhookJSONLenient(t *testing.T) {
	var got struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	}
	in := []byte("{\"id\":\"a\",\"ID\":\"b\",\"metadata\":{\"k\":\"v\xff\"}}")
	if err := json.Unmarshal(in, &got, webhookJSON); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != "b" || got.Metadata["k"] == "" {
		t.Fatalf("got %+v", got)
	}
}
