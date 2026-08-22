package money

import (
	"errors"
	"math"
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
		// Rounding is away from zero, so the result never understates the input.
		if float64(got) < v*float64(microsPerUnit)-1 {
			t.Fatalf("FromUSD(%v) = %d understates the input", v, got)
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
	f.Fuzz(func(t *testing.T, gross, bps, fixed int64) {
		if gross < 0 || bps < 0 || fixed < 0 || gross > 1<<60 || bps > 1<<20 || fixed > 1<<60 {
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
