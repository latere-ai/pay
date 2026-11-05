// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pay

import (
	"strings"
	"testing"
)

// MemEvent writes map keys in sorted order so equal events encode to equal bytes.
func TestMemEventBytes(t *testing.T) {
	got := string(MemEvent("k", "r", 5, map[string]string{"b": "2", "a": "1"}))
	want := `"Meta":{"a":"1","b":"2"}`
	if !strings.Contains(got, want) {
		t.Fatalf("MemEvent = %s, want it to contain %s", got, want)
	}
}

// A hand-written payload may repeat a key and use any letter case.
func TestMemProviderParseLenient(t *testing.T) {
	var m MemProvider
	e, err := m.ParseWebhook([]byte(`{"ref":"a","Ref":"b","gross":7}`), nil)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if e.Ref != "b" || e.Gross != 7 {
		t.Fatalf("event = %+v, want Ref b, Gross 7", e)
	}
}
