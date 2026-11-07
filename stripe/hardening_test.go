// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pay"
	"latere.ai/x/pay/money"
)

func TestCreateCheckout_RefusesANonPositiveAmount(t *testing.T) {
	for name, amount := range map[string]money.Micro{"zero": 0, "negative": -5 * money.Dollar} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.json(http.MethodPost, sessionsPath, sessionCreated)
			a := newAdapter(t, s)
			p := topUp()
			p.Amount = amount
			_, err := a.CreateCheckout(context.Background(), p)
			if !errors.Is(err, pay.ErrNotPositive) {
				t.Errorf("CreateCheckout = %v, want ErrNotPositive", err)
			}
			if n := len(s.calls); n != 0 {
				t.Errorf("%d requests reached Stripe, want none", n)
			}
		})
	}
}

func TestChargeSaved_RefusesANonPositiveAmount(t *testing.T) {
	for name, amount := range map[string]money.Micro{"zero": 0, "negative": -money.Dollar} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.json(http.MethodPost, intentsPath, map[string]any{"id": "pi_x", "object": "payment_intent", "status": "succeeded"})
			a := newAdapter(t, s)
			_, err := a.ChargeSaved(context.Background(), pay.SavedChargeParams{
				Customer:       pay.CustomerRef{Provider: pay.Stripe, ID: "cus_1"},
				Amount:         amount,
				Currency:       money.USD,
				IdempotencyKey: "k",
			})
			if !errors.Is(err, pay.ErrNotPositive) {
				t.Errorf("ChargeSaved = %v, want ErrNotPositive", err)
			}
			if n := len(s.calls); n != 0 {
				t.Errorf("%d requests reached Stripe, want none", n)
			}
		})
	}
}

// stripe-go rejects a signature that is too old and accepts one from any time
// ahead, so a secret holder could sign a delivery that verifies for ever.
func TestParseWebhook_RefusesAFutureDatedSignature(t *testing.T) {
	a := newAdapter(t, newStub(t))
	payload := eventPayload(t, eventSessionCompleted, paidSession())
	for name, ahead := range map[string]time.Duration{
		"a year ahead":  365 * 24 * time.Hour,
		"an hour ahead": time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := a.ParseWebhook(payload, signedHeader(testWebhookSecret, payload, time.Now().Add(ahead)))
			if !errors.Is(err, pay.ErrBadSignature) {
				t.Errorf("ParseWebhook = %v, want ErrBadSignature", err)
			}
		})
	}
	// A sender whose clock runs a little fast is within the tolerance.
	ev, err := a.ParseWebhook(payload, signedHeader(testWebhookSecret, payload, time.Now().Add(10*time.Second)))
	if err != nil {
		t.Fatalf("a signature 10 s ahead = %v, want it accepted", err)
	}
	if ev.Kind != pay.KindPaid {
		t.Errorf("Kind = %q, want KindPaid", ev.Kind)
	}
}

func TestSignedAt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for header, ok := range map[string]bool{
		"t=1700000000,v1=abc":  true,
		"v1=abc, t=1700000000": true,
		"v1=abc":               false,
		"t=soon,v1=abc":        false,
		"":                     false,
	} {
		_, got := signedAt(header)
		if got != ok {
			t.Errorf("signedAt(%q) ok = %v, want %v", header, got, ok)
		}
		if !ok && !errors.Is(notFromTheFuture(header, now), pay.ErrBadSignature) {
			t.Errorf("notFromTheFuture(%q) did not refuse an unreadable timestamp", header)
		}
	}
}

// The signature proves who posted the bytes, not that an amount is sane, and
// money.FromMinor wraps around int64 silently.
func TestParseWebhook_RefusesAnAmountThatDoesNotFit(t *testing.T) {
	const largest = 922337203685477 // math.MaxInt64 / 10000
	taxed := func(c *Config) { c.Tax = true }
	withAmount := func(mutate func(map[string]any)) map[string]any {
		o := paidSession()
		mutate(o)
		return o
	}
	cases := []struct {
		name    string
		typ     string
		object  map[string]any
		options []func(*Config)
	}{
		{"amount_total too large", eventSessionCompleted, withAmount(func(o map[string]any) { o["amount_total"] = int64(largest + 1) }), nil},
		{"amount_total negative", eventSessionCompleted, withAmount(func(o map[string]any) { o["amount_total"] = int64(-1) }), nil},
		{"converted amount_total too large", eventSessionCompleted, withAmount(func(o map[string]any) {
			o["currency_conversion"] = map[string]any{"source_currency": "usd", "amount_total": int64(largest + 1)}
		}), nil},
		{"amount_tax too large", eventSessionCompleted, withAmount(func(o map[string]any) {
			o["total_details"] = map[string]any{"amount_tax": int64(largest + 1)}
		}), []func(*Config){taxed}},
		{"amount_tax negative", eventSessionCompleted, withAmount(func(o map[string]any) {
			o["total_details"] = map[string]any{"amount_tax": int64(-300)}
		}), []func(*Config){taxed}},
		{"amount_received too large", eventIntentSucceeded, map[string]any{
			"id": "pi_1", "currency": "usd", "amount_received": int64(largest + 1),
			"metadata": map[string]string{metaOrigin: originSavedMethod},
		}, nil},
		{"amount_received negative", eventIntentSucceeded, map[string]any{
			"id": "pi_1", "currency": "usd", "amount_received": int64(-1),
			"metadata": map[string]string{metaOrigin: originSavedMethod},
		}, nil},
		{"amount_refunded too large", eventChargeRefunded, map[string]any{
			"id": "ch_1", "currency": "usd", "amount_refunded": int64(largest + 1), "payment_intent": "pi_1",
		}, nil},
		{"dispute amount negative", eventDisputeCreated, map[string]any{
			"id": "dp_1", "currency": "usd", "amount": int64(-1), "payment_intent": "pi_1",
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAdapter(t, newStub(t), c.options...)
			payload := eventPayload(t, c.typ, c.object)
			ev, err := a.ParseWebhook(payload, signedNow(payload))
			if !errors.Is(err, money.ErrRange) {
				t.Fatalf("ParseWebhook = %+v, %v; want money.ErrRange", ev, err)
			}
			if errors.Is(err, pay.ErrBadSignature) {
				t.Errorf("a verified delivery with a bad amount is reported as a bad signature: %v", err)
			}
			if ev.Kind != pay.KindIgnored || ev.Gross != 0 {
				t.Errorf("a refused delivery returned %+v", ev)
			}
		})
	}

	// The largest amount that converts exactly is still credited.
	a := newAdapter(t, newStub(t))
	sess := paidSession()
	sess["amount_total"] = int64(largest)
	payload := eventPayload(t, eventSessionCompleted, sess)
	ev, err := a.ParseWebhook(payload, signedNow(payload))
	if err != nil {
		t.Fatalf("the largest convertible amount = %v, want it accepted", err)
	}
	if want := money.FromMinor(largest, money.USD); ev.Gross != want || ev.Gross <= 0 {
		t.Errorf("Gross = %d, want %d", ev.Gross, want)
	}
}

// A refusal of a verified delivery has to name its real cause. A paid EUR
// session or one with nothing to dedupe on is not a signature problem, and the
// handler reads the difference to tell an operator where to look.
func TestParseWebhook_AVerifiedButUnusableDeliveryIsNotABadSignature(t *testing.T) {
	a := newAdapter(t, newStub(t))
	eur := paidSession()
	eur["currency"] = "eur"
	noRef := paidSession()
	delete(noRef, "payment_intent")
	delete(noRef, "id")
	cases := []struct {
		name   string
		object map[string]any
		want   error
	}{
		{"a paid EUR session", eur, ErrNotUSD},
		{"a paid session with no reference", noRef, ErrNoReference},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := eventPayload(t, eventSessionCompleted, c.object)
			_, err := a.ParseWebhook(payload, signedNow(payload))
			if !errors.Is(err, c.want) {
				t.Fatalf("ParseWebhook = %v, want %v", err, c.want)
			}
			if errors.Is(err, pay.ErrBadSignature) {
				t.Errorf("the signature verified, yet the error is a bad signature: %v", err)
			}
		})
	}
	// And a delivery that really does not verify still is one.
	payload := eventPayload(t, eventSessionCompleted, paidSession())
	if _, err := a.ParseWebhook(payload, signedHeader("whsec_other", payload, time.Now())); !errors.Is(err, pay.ErrBadSignature) {
		t.Errorf("a delivery signed with another secret = %v, want ErrBadSignature", err)
	}
}

func TestConfigDoesNotPrintItsSecrets(t *testing.T) {
	const key, hook = "sk_live_supersecretkey", "whsec_supersecrethook"
	full := Config{SecretKey: key, WebhookSecret: hook, Tax: true, ProductName: "Credits"}
	type holder struct {
		Name   string
		Config Config
	}
	forms := []string{"%v", "%+v", "%#v", "%s"}
	values := map[string]any{
		"value":   full,
		"pointer": &full,
		"struct":  holder{Name: "x", Config: full},
		"slice":   []Config{full},
	}
	for name, v := range values {
		for _, f := range forms {
			got := fmt.Sprintf(f, v)
			if strings.Contains(got, key) || strings.Contains(got, hook) {
				t.Errorf("%s with %s printed a secret: %s", name, f, got)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Errorf("%s with %s shows no redaction marker: %s", name, f, got)
			}
		}
	}
	// What is not secret still prints, so a log line stays useful.
	for _, f := range forms {
		got := fmt.Sprintf(f, full)
		if !strings.Contains(got, "Credits") || !strings.Contains(got, "true") {
			t.Errorf("%s dropped the fields that are not secret: %s", f, got)
		}
	}
	// An unset secret prints as empty, which says "not configured" in a log.
	empty := Config{}
	for _, f := range forms {
		got := fmt.Sprintf(f, empty)
		if strings.Contains(got, "redacted") {
			t.Errorf("%s of an empty Config claims a secret is set: %s", f, got)
		}
	}
	if got, want := empty.String(), `stripe.Config{SecretKey: WebhookSecret: Tax:false ProductName:""}`; got != want {
		t.Errorf("String = %s, want %s", got, want)
	}
}
