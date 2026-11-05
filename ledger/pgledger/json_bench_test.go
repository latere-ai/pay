// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pgledger

import (
	"encoding/json/v2"
	"testing"

	"latere.ai/x/pay/ledger"
)

func benchLabels() map[string]string {
	return map[string]string{
		"email":          "buyer@example.test",
		"credited_micro": "18700000",
		"origin":         "checkout",
		"session":        "cs_test_a1b2c3d4e5f6a1b2c3d4e5f6",
		"intent":         "pi_test_3PqRsT0aBcDeFgHi1jKlMnOp",
		"locale":         "de-DE",
	}
}

// BenchmarkJSONLabelsMarshal stands for every ledger write that carries labels:
// the labels column is encoded by marshalLabels before the INSERT.
func BenchmarkJSONLabelsMarshal(b *testing.B) {
	labels := benchLabels()
	enc, err := marshalLabels(labels)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	for b.Loop() {
		if _, err := marshalLabels(labels); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONLabelsUnmarshal stands for every ledger read: the labels column
// is decoded into Entry.Labels as scanEntry does for each row.
func BenchmarkJSONLabelsUnmarshal(b *testing.B) {
	enc, err := marshalLabels(benchLabels())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	for b.Loop() {
		var e ledger.Entry
		if err := json.Unmarshal(enc, &e.Labels); err != nil {
			b.Fatal(err)
		}
	}
}
