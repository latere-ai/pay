// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package money

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func TestCurrencyValidAndExponent(t *testing.T) {
	for _, c := range []Currency{USD, EUR} {
		if !c.Valid() {
			t.Errorf("%s.Valid() = false, want true", c)
		}
		if got := c.Exponent(); got != 2 {
			t.Errorf("%s.Exponent() = %d, want 2", c, got)
		}
		if got := c.String(); got != string(c) {
			t.Errorf("%s.String() = %q", c, got)
		}
	}
	var unknown Currency = "xyz"
	if unknown.Valid() {
		t.Error("unknown currency reports Valid")
	}
	if got := unknown.Exponent(); got != 0 {
		t.Errorf("unknown Exponent = %d, want 0", got)
	}
	// Zero-decimal fallback: one minor unit is one whole unit.
	if got := unknown.microsPerMinor(); got != microsPerUnit {
		t.Errorf("unknown microsPerMinor = %d, want %d", got, microsPerUnit)
	}
}

// The boundaries are where a rounding rule earns its keep: exactly one cent,
// one micro under it, and one micro over.
func TestMinorConversionBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		m        Micro
		wantUp   int64
		wantDown int64
	}{
		{"zero", 0, 0, 0},
		{"one micro", 1, 1, 0},
		{"just under a cent", Cent - 1, 1, 0},
		{"exactly a cent", Cent, 1, 1},
		{"just over a cent", Cent + 1, 2, 1},
		{"a dollar", Dollar, 100, 100},
		{"a dollar and a fraction", Dollar + 1, 101, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.MinorUp(USD); got != tc.wantUp {
				t.Errorf("MinorUp = %d, want %d", got, tc.wantUp)
			}
			if got := tc.m.MinorDown(USD); got != tc.wantDown {
				t.Errorf("MinorDown = %d, want %d", got, tc.wantDown)
			}
		})
	}
}

func TestFromMinorRoundTrip(t *testing.T) {
	for _, c := range []Currency{USD, EUR} {
		for _, n := range []int64{0, 1, 99, 100, 12_345} {
			if got := FromMinor(n, c); got.MinorDown(c) != n {
				t.Errorf("FromMinor(%d, %s) did not round-trip: %d", n, c, got.MinorDown(c))
			}
		}
	}
}

// The property that makes the two directions safe to mix: converting up and
// back never loses money.
func TestMinorUpNeverLosesMoney(t *testing.T) {
	for _, m := range []Micro{0, 1, Cent - 1, Cent, Cent + 1, Dollar, Dollar + 4_321} {
		if back := FromMinor(m.MinorUp(USD), USD); back < m {
			t.Errorf("FromMinor(MinorUp(%d)) = %d, want >= %d", m, back, m)
		}
		if back := FromMinor(m.MinorDown(USD), USD); back > m {
			t.Errorf("FromMinor(MinorDown(%d)) = %d, want <= %d", m, back, m)
		}
	}
}

func TestFromUSD(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want Micro
	}{
		{"zero", 0, 0},
		{"one dollar", 1, Dollar},
		{"one cent", 0.01, Cent},
		// The reason the rule is "away from zero": a tenth of a micro is a real
		// charge and must not round to free.
		{"a tenth of a micro rounds up", 0.0000001, 1},
		{"a rate card figure", 3.5, 3_500_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromUSD(tc.in)
			if err != nil {
				t.Fatalf("FromUSD(%v) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("FromUSD(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// -1 is the platform's "unpriceable model" sentinel. A signed ledger would
// read it as a one-micro credit, so the conversion refuses it by name.
func TestFromUSDRefusesTheUnpriceableSentinel(t *testing.T) {
	if _, err := FromUSD(-1); !errors.Is(err, ErrNegative) {
		t.Errorf("FromUSD(-1) error = %v, want ErrNegative", err)
	}
	if _, err := FromUSD(-0.000001); !errors.Is(err, ErrNegative) {
		t.Errorf("FromUSD(tiny negative) error = %v, want ErrNegative", err)
	}
}

func TestFromUSDRefusesNonFiniteAndOutOfRange(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := FromUSD(f); !errors.Is(err, ErrNotFinite) {
			t.Errorf("FromUSD(%v) error = %v, want ErrNotFinite", f, err)
		}
	}
	if _, err := FromUSD(math.MaxFloat64); !errors.Is(err, ErrRange) {
		t.Errorf("FromUSD(max) error = %v, want ErrRange", err)
	}
}

func TestString(t *testing.T) {
	cases := []struct {
		m    Micro
		c    Currency
		want string
	}{
		{0, USD, "$0.00"},
		{Dollar, USD, "$1.00"},
		{Dollar + 23*Cent, USD, "$1.23"},
		{-Dollar - 23*Cent, USD, "-$1.23"},
		{45_100, EUR, "€0.0451"},
		{1, USD, "$0.000001"},
		{Dollar, "xyz", "XYZ 1.00"},
	}
	for _, tc := range cases {
		if got := tc.m.String(tc.c); got != tc.want {
			t.Errorf("Micro(%d).String(%s) = %q, want %q", tc.m, tc.c, got, tc.want)
		}
	}
}

func TestCeilRoundsAwayFromZero(t *testing.T) {
	cases := []struct{ num, den, want int64 }{
		{0, 3, 0},
		{1, 3, 1},
		{3, 3, 1},
		{4, 3, 2},
		{-1, 3, -1},
		{-4, 3, -2},
		{1, -3, -1},
		{-4, -3, 2},
	}
	for _, tc := range cases {
		if got := Ceil(tc.num, tc.den); got != tc.want {
			t.Errorf("Ceil(%d, %d) = %d, want %d", tc.num, tc.den, got, tc.want)
		}
	}
}

func TestCeilPanicsOnZeroDenominator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Ceil(1, 0) did not panic")
		}
	}()
	_ = Ceil(1, 0)
}

func TestSpreadCredited(t *testing.T) {
	// 5% + $0.30 on $20 credits $18.70.
	s := Spread{Bps: 500, FixedMicro: 300_000}
	if got, want := s.Credited(20*Dollar), 20*Dollar-1*Dollar-300_000; got != want {
		t.Errorf("Credited($20) = %d, want %d", got, want)
	}
	// No spread credits the gross.
	if got := (Spread{}).Credited(20 * Dollar); got != 20*Dollar {
		t.Errorf("Credited with no spread = %d, want the gross", got)
	}
	// A purchase below the fixed cut credits zero, never a negative.
	if got := (Spread{FixedMicro: Dollar}).Credited(100_000); got != 0 {
		t.Errorf("Credited below the fixed cut = %d, want 0", got)
	}
	// A non-positive gross credits nothing rather than inverting the cut.
	if got := s.Credited(0); got != 0 {
		t.Errorf("Credited(0) = %d, want 0", got)
	}
	if got := s.Credited(-Dollar); got != 0 {
		t.Errorf("Credited(negative) = %d, want 0", got)
	}
}

// The percentage cut rounds half up, so the platform is not shorted by a
// fraction on every purchase.
func TestSpreadRoundsHalfUp(t *testing.T) {
	s := Spread{Bps: 1} // 0.01%, so the cut is gross/10000 before rounding
	// The half-up boundary sits at gross = 5000, where the exact cut is 0.5.
	if got := s.Credited(5_000); got != 5_000-1 {
		t.Errorf("Credited(5000) = %d, want a cut of 1 (0.5 rounds up)", got)
	}
	// One micro below the boundary the exact cut is 0.4999, which rounds to 0.
	if got := s.Credited(4_999); got != 4_999 {
		t.Errorf("Credited(4999) = %d, want a cut of 0 (0.4999 rounds down)", got)
	}
	// And a whole unit above it the cut is a clean 1.5 → 2.
	if got := s.Credited(15_000); got != 15_000-2 {
		t.Errorf("Credited(15000) = %d, want a cut of 2 (1.5 rounds up)", got)
	}
}

func FuzzFromUSD(f *testing.F) {
	f.Add(0.0)
	f.Add(1.0)
	f.Add(0.0000001)
	f.Add(-1.0)
	f.Fuzz(func(t *testing.T, v float64) {
		got, err := FromUSD(v)
		if err != nil {
			return
		}
		if got < 0 {
			t.Fatalf("FromUSD(%v) = %d, a negative amount from a successful conversion", v, got)
		}
		// The result is within one micro plus one float step of the exact value
		// of the float: the step is the slack the shortest decimal may take.
		if !withinAFloatStep(v, got) {
			t.Fatalf("FromUSD(%v) = %d, more than a micro plus a float step from the exact value", v, got)
		}
	})
}

func FuzzString(f *testing.F) {
	f.Add(int64(0), "usd")
	f.Add(int64(-1), "eur")
	f.Add(int64(1_234_567), "xyz")
	f.Fuzz(func(t *testing.T, v int64, cur string) {
		s := Micro(v).String(Currency(cur))
		if s == "" {
			t.Fatal("String returned empty")
		}
		if (v < 0) != (s[0] == '-') {
			t.Fatalf("Micro(%d).String(%q) = %q: sign mismatch", v, cur, s)
		}
	})
}

func FuzzSpreadCreditedNeverExceedsGross(f *testing.F) {
	f.Add(int64(20_000_000), int64(500), int64(300_000))
	f.Add(int64(math.MaxInt64), int64(10_000), int64(0))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MinInt64))
	f.Add(int64(2e16), int64(500), int64(300_000))
	f.Fuzz(func(t *testing.T, gross, bps, fixed int64) {
		if gross < 0 {
			return
		}
		got := Spread{Bps: bps, FixedMicro: Micro(fixed)}.Credited(Micro(gross))
		if got < 0 {
			t.Fatalf("Credited(%d) = %d, negative", gross, got)
		}
		if got > Micro(gross) {
			t.Fatalf("Credited(%d) = %d, more than the gross", gross, got)
		}
	})
}

// A currency code is caller-supplied text. Interpolated raw it can forge a
// sign: Currency("-") rendered a positive amount as "- 0.000069", which reads
// as negative. Found by FuzzString.
func TestAnUnknownCurrencyCannotForgeASign(t *testing.T) {
	cases := []struct {
		cur  Currency
		m    Micro
		want string
	}{
		{"-", 69, "0.000069"},
		{"-usd", Dollar, "USD 1.00"},
		{"!!!", Dollar, "1.00"},
		{"gbp", Dollar, "GBP 1.00"},
		{"-", -69, "-0.000069"},
	}
	for _, tc := range cases {
		got := tc.m.String(tc.cur)
		if got != tc.want {
			t.Errorf("Micro(%d).String(%q) = %q, want %q", tc.m, tc.cur, got, tc.want)
		}
		if (tc.m < 0) != (got[0] == '-') {
			t.Errorf("Micro(%d).String(%q) = %q: the leading sign does not match the amount", tc.m, tc.cur, got)
		}
	}
}

// creditedOracle is the documented formula in exact big-integer arithmetic.
func creditedOracle(gross, bps, fixed int64) int64 {
	if gross <= 0 {
		return 0
	}
	if bps < 0 {
		bps = 0
	}
	if fixed < 0 {
		fixed = 0
	}
	cut := new(big.Int).Mul(big.NewInt(gross), big.NewInt(bps))
	cut.Add(cut, big.NewInt(5_000)).Quo(cut, big.NewInt(10_000))
	r := new(big.Int).Sub(big.NewInt(gross), cut)
	r.Sub(r, big.NewInt(fixed))
	if r.Sign() < 0 {
		return 0
	}
	return r.Int64()
}

// gross*bps used to overflow int64, turning the cut negative and crediting
// more than was paid.
func TestCreditedNeverExceedsGross(t *testing.T) {
	grosses := []int64{math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64/500 + 1, 2e16, 1 << 62, 1 << 60}
	spreads := []Spread{
		{Bps: 500}, {Bps: 10_000}, {Bps: 500, FixedMicro: 30 * Cent},
		{Bps: math.MaxInt64}, {Bps: 10_001}, {Bps: 0, FixedMicro: math.MaxInt64},
	}
	for _, g := range grosses {
		for _, s := range spreads {
			got := s.Credited(Micro(g))
			if got < 0 || got > Micro(g) {
				t.Errorf("%+v.Credited(%d) = %d, outside [0, gross]", s, g, got)
			}
			if want := creditedOracle(g, s.Bps, int64(s.FixedMicro)); int64(got) != want {
				t.Errorf("%+v.Credited(%d) = %d, want %d", s, g, got, want)
			}
		}
	}
	if got := (Spread{Bps: 10_000}).Credited(math.MaxInt64); got != 0 {
		t.Errorf("a 100%% cut of MaxInt64 credits %d, want 0", got)
	}
	if got := (Spread{Bps: 500}).Credited(2e16); got != 2e16-1e15 {
		t.Errorf("5%% of 2e16 credits %d, want %d", got, int64(2e16-1e15))
	}
}

// The new arithmetic keeps the old half-up rounding exactly.
func TestCreditedMatchesTheExactFormula(t *testing.T) {
	vals := []int64{0, 1, 2, 4_999, 5_000, 5_001, 9_999, 10_000, 15_000, 20_000_000, 1<<62 + 12345, math.MaxInt64}
	bpss := []int64{0, 1, 7, 500, 9_999, 10_000, 10_001, 1 << 40, math.MaxInt64}
	fixeds := []int64{0, 1, 300_000, 1 << 50, math.MaxInt64}
	for _, g := range vals {
		for _, b := range bpss {
			for _, f := range fixeds {
				got := Spread{Bps: b, FixedMicro: Micro(f)}.Credited(Micro(g))
				if want := creditedOracle(g, b, f); int64(got) != want {
					t.Fatalf("Credited(%d) bps=%d fixed=%d = %d, want %d", g, b, f, got, want)
				}
			}
		}
	}
}

// A negative Bps made the cut negative and credited more than the purchase.
func TestCreditedIgnoresANegativeSpread(t *testing.T) {
	gross := 20 * Dollar
	cases := []struct {
		s    Spread
		want Micro
	}{
		{Spread{Bps: -500}, gross},
		{Spread{FixedMicro: -Dollar}, gross},
		{Spread{Bps: math.MinInt64, FixedMicro: math.MinInt64}, gross},
		{Spread{Bps: -500, FixedMicro: 30 * Cent}, gross - 30*Cent},
	}
	for _, tc := range cases {
		if got := tc.s.Credited(gross); got != tc.want {
			t.Errorf("%+v.Credited(%d) = %d, want %d", tc.s, gross, got, tc.want)
		}
	}
}

// float64(i)/1e6 times 1e6 is not always i in floating point, so the old
// multiply-then-ceil rounded some whole-micro amounts up by one micro.
func TestFromUSDIsExactOnWholeMicros(t *testing.T) {
	bad := 0
	check := func(i int64) {
		f := float64(i) / 1e6
		got, err := FromUSD(f)
		if err != nil || int64(got) != i {
			bad++
			if bad <= 3 {
				t.Errorf("FromUSD(%v) = %d, %v, want %d", f, got, err, i)
			}
		}
	}
	for i := int64(1); i <= 1_000_000; i++ {
		check(i)
	}
	for _, i := range []int64{123, 1_000_000_001, 123_456_789_012, 1 << 40, 1_000_000_000_000_000} {
		check(i)
	}
	if bad > 0 {
		t.Fatalf("%d whole-micro amounts were not exact", bad)
	}
	if got, err := FromUSD(0.000123); err != nil || got != 123 {
		t.Fatalf("FromUSD(0.000123) = %d, %v, want 123", got, err)
	}
}

func TestFromUSDRoundsARealFractionUp(t *testing.T) {
	cases := []struct {
		f    float64
		want Micro
	}{
		{0.0000001, 1}, {0.0000011, 2}, {1.0000001, 1_000_001}, {0.5, 500_000},
		{0, 0}, {math.Copysign(0, -1), 0}, {1e-300, 1}, {12.3456789, 12_345_679},
	}
	for _, tc := range cases {
		if got, err := FromUSD(tc.f); err != nil || got != tc.want {
			t.Errorf("FromUSD(%v) = %d, %v, want %d", tc.f, got, err, tc.want)
		}
	}
}

// 2^63 micros is one past MaxInt64. The old check let it through and the
// float-to-int conversion then differed by architecture.
func TestFromUSDRefusesTwoToTheSixtyThree(t *testing.T) {
	for _, f := range []float64{
		9223372036854775808.0 / 1e6, 9.3e12, 1e13, 1e18, 1e19, 1e300, math.MaxFloat64,
	} {
		got, err := FromUSD(f)
		if !errors.Is(err, ErrRange) {
			t.Errorf("FromUSD(%v) = %d, %v, want ErrRange", f, got, err)
		}
	}
	// The largest float below 2^63 micros converts exactly.
	f := math.Nextafter(9223372036854775808.0/1e6, 0)
	got, err := FromUSD(f)
	if err != nil || got <= 0 {
		t.Fatalf("FromUSD(%v) = %d, %v, want an exact amount", f, got, err)
	}
	if !withinAFloatStep(f, got) {
		t.Errorf("FromUSD(%v) = %d, too far from the exact value", f, got)
	}
}

// withinAFloatStep reports whether got is within one micro plus one float
// step (scaled to micros) of the exact value of v.
func withinAFloatStep(v float64, got Micro) bool {
	scale := big.NewRat(microsPerUnit, 1)
	exact := new(big.Rat).SetFloat64(v)
	exact.Mul(exact, scale)
	step := new(big.Rat).SetFloat64(math.Nextafter(v, math.Inf(1)) - v)
	slack := step.Mul(step, scale)
	slack.Add(slack, big.NewRat(1, 1))
	diff := new(big.Rat).Sub(new(big.Rat).SetInt64(int64(got)), exact)
	return diff.Abs(diff).Cmp(slack) < 0
}

// decimalMicros guards its own range, since the float pre-check in FromUSD is
// only an approximation of it near 2^63.
func TestDecimalMicrosRange(t *testing.T) {
	cases := []struct {
		in   string
		want Micro
		ok   bool
	}{
		{"9223372036854.775807", math.MaxInt64, true},
		{"9223372036854.775808", 0, false},
		{"9223372036854.7758071", 0, false}, // a real fraction past the sixth place rounds up out of range
		{"9223372036854.775807000", math.MaxInt64, true},
		{"9223372036855", 0, false},
		{"18446744073710", 0, false},       // times 10^6 overflows 64 bits
		{"10000000000000000000", 0, false}, // 20 digits
		{"1", Dollar, true},
	}
	for _, tc := range cases {
		got, ok := decimalMicros(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("decimalMicros(%q) = %d, %v, want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
