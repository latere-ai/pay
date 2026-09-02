// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package money is the unit every amount in the platform is in.
//
// One amount type, one currency vocabulary, one rounding rule. Everything
// that touches money imports this, so a cost computed by a rate card and a
// balance folded from a ledger cannot disagree about what a number means.
//
// See docs/money-model.md.
package money

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Micro is an amount in millionths of one currency unit.
//
// It is int64 and never a float: a float cannot represent a tenth of a cent
// exactly, and money that does not round-trip through storage is money that
// drifts. It is a defined type rather than a bare int64 because the compiler
// then refuses to add cents to micros, or a per-million rate to an amount,
// which are the two mistakes that are invisible in review and expensive in
// production.
type Micro int64

// The units an amount is built from.
const (
	// Unit is one micro, the smallest amount this package represents.
	Unit Micro = 1
	// Cent is one hundredth of a currency unit, for currencies with two
	// decimal places.
	Cent Micro = 10_000
	// Dollar is one whole currency unit, for readable constants and tests.
	Dollar Micro = 1_000_000
)

// microsPerUnit is how many Micro make one whole currency unit.
const microsPerUnit = int64(Dollar)

// Errors this package returns.
var (
	// ErrNegative reports a conversion of a negative value where only a
	// magnitude makes sense. It exists because a cost is a magnitude and at
	// least one producer in the platform signals "unpriceable" with -1; a
	// signed ledger would read that as a credit.
	ErrNegative = errors.New("money: amount is negative")
	// ErrNotFinite reports a NaN or infinite float.
	ErrNotFinite = errors.New("money: amount is not finite")
	// ErrRange reports a value too large to hold in Micro.
	ErrRange = errors.New("money: amount is out of range")
	// ErrCurrency reports a currency this package does not know.
	ErrCurrency = errors.New("money: unknown currency")
)

// Currency is a presentment currency: a closed vocabulary, because it is
// recorded alongside amounts and a typo is a reconciliation that never
// balances.
type Currency string

// The currencies an amount may be presented in. The ledger itself is USD end
// to end; a non-USD charge is converted once, by the processor, at the moment
// of purchase.
const (
	USD Currency = "usd"
	EUR Currency = "eur"
)

// exponents is the number of decimal places each currency's minor unit
// carries. A table rather than a hardcoded 100, so a zero-decimal currency
// (JPY) can be added without hunting for divisions.
var exponents = map[Currency]int{
	USD: 2,
	EUR: 2,
}

// Valid reports whether c is a currency this package knows.
func (c Currency) Valid() bool {
	_, ok := exponents[c]
	return ok
}

// Exponent is the number of decimal places c's minor unit carries: 2 for USD
// and EUR. An unknown currency reports 0, which is not a licence to use it;
// callers that care ask Valid first.
func (c Currency) Exponent() int { return exponents[c] }

// String makes Currency printable.
func (c Currency) String() string { return string(c) }

// microsPerMinor is how many Micro make one of c's smallest units: 10,000 for
// a two-decimal currency. An unknown currency yields microsPerUnit, treating
// it as zero-decimal, which is the fail-expensive direction.
func (c Currency) microsPerMinor() int64 {
	d := microsPerUnit
	for i := 0; i < c.Exponent(); i++ {
		d /= 10
	}
	return d
}

// MinorUp converts to the processor's smallest unit, rounding away from zero.
//
// This is the direction that decides what somebody is charged, so a fraction
// of a cent is charged rather than absorbed. The bias is at most one minor
// unit per operation and it always favours the platform; the opposite bias,
// repeated, is the platform paying for rounding.
func (m Micro) MinorUp(c Currency) int64 {
	return Ceil(int64(m), c.microsPerMinor())
}

// MinorDown converts to the processor's smallest unit, rounding toward zero.
//
// This is the direction that quotes a number back to a person, so a quote is
// never above what is charged.
func (m Micro) MinorDown(c Currency) int64 {
	return int64(m) / c.microsPerMinor()
}

// FromMinor converts a processor's smallest unit back to Micro.
func FromMinor(n int64, c Currency) Micro {
	return Micro(n * c.microsPerMinor())
}

// FromUSD converts a float USD amount, rounding away from zero.
//
// It is the one sanctioned float boundary, for rate cards quoted as floats. A
// negative input is an error rather than a negative amount: a cost is a
// magnitude, and the platform's "unpriceable" sentinel is -1, which must never
// silently become a credit.
func FromUSD(f float64) (Micro, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, ErrNotFinite
	}
	if f < 0 {
		return 0, fmt.Errorf("%w: %v", ErrNegative, f)
	}
	scaled := math.Ceil(f * float64(microsPerUnit))
	if scaled > float64(math.MaxInt64) {
		return 0, fmt.Errorf("%w: %v", ErrRange, f)
	}
	return Micro(scaled), nil
}

// String formats an amount for display: "$1.23", "€0.045100".
//
// Display only, never parsed back. Sub-cent amounts widen to six decimals
// rather than rounding to zero, because "$0.00" for a real charge reads as a
// bug to whoever is looking at it.
func (m Micro) String(c Currency) string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	whole := v / microsPerUnit
	frac := v % microsPerUnit
	var body string
	switch {
	case frac == 0:
		body = fmt.Sprintf("%d.00", whole)
	case frac%10_000 == 0: // a whole number of cents
		body = fmt.Sprintf("%d.%02d", whole, frac/10_000)
	default:
		body = strings.TrimRight(fmt.Sprintf("%d.%06d", whole, frac), "0")
	}
	return sign + symbol(c) + body
}

// symbol is the display symbol for c.
//
// An unknown currency falls back to its code so the amount is not silently
// unmarked, but the code is reduced to letters first. Interpolating it raw lets
// a code containing punctuation forge a sign: Currency("-") rendered
// Micro(69) as "- 0.000069", a positive amount that reads as a negative one.
// Found by FuzzString.
func symbol(c Currency) string {
	switch c {
	case USD:
		return "$"
	case EUR:
		return "€"
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(string(c)) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + " "
}

// Ceil divides num by den rounding away from zero.
//
// Exported because the ledger and every rate card need the same bias and must
// not each re-derive it. A zero denominator is a programming error, not a
// runtime condition, so it panics rather than returning a plausible number.
func Ceil(num, den int64) int64 {
	if den == 0 {
		panic("money: divide by zero")
	}
	q, r := num/den, num%den
	if r == 0 {
		return q
	}
	// r and the true quotient share a sign here, so stepping away from zero is
	// +1 when the operands agree in sign and -1 when they do not.
	if (num < 0) == (den < 0) {
		return q + 1
	}
	return q - 1
}

// Spread is the platform's cut at purchase: a percentage in basis points plus
// a fixed amount. Margin is taken here, once, visibly, rather than as a markup
// buried in per-unit pricing.
type Spread struct {
	// Bps is the percentage cut in basis points: 500 is 5%.
	Bps int64
	// FixedMicro is the flat cut taken from every purchase.
	FixedMicro Micro
}

// Credited is what a gross purchase lands in a wallet:
//
//	credited = max(0, gross − round_half_up(gross×bps/10⁴) − fixed)
//
// Never negative: a purchase smaller than the fixed cut credits zero, which is
// why a minimum purchase amount exists alongside this rather than instead of
// it. The percentage cut rounds half up.
func (s Spread) Credited(gross Micro) Micro {
	if gross <= 0 {
		return 0
	}
	cut := (int64(gross)*s.Bps + 5_000) / 10_000
	credited := Micro(int64(gross) - cut - int64(s.FixedMicro))
	if credited < 0 {
		return 0
	}
	return credited
}
