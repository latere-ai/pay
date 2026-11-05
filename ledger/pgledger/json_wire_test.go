// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pgledger

import "testing"

// Stored labels encode with sorted keys so equal labels are equal bytes.
func TestMarshalLabelsBytes(t *testing.T) {
	b, err := marshalLabels(map[string]string{"z": "1", "a": "2", "m": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":"2","m":"3","z":"1"}`; string(b) != want {
		t.Fatalf("labels = %s, want %s", b, want)
	}
}

// A label with invalid UTF-8 is stored with U+FFFD rather than failing the entry.
func TestMarshalLabelsInvalidUTF8(t *testing.T) {
	b, err := marshalLabels(map[string]string{"k": "a\xff"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"k\":\"a�\"}"; string(b) != want {
		t.Fatalf("labels = %q, want %q", b, want)
	}
}
