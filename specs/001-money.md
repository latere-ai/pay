---
title: money — the unit every amount in the platform is in
status: implemented
repo: latere-ai/pay
package: latere.ai/x/pay/money
effort: small
created: 2026-08-22
updated: 2026-08-22
author: changkun
trigger: Payment and ledger both need one amount type, one currency vocabulary, and one rounding rule. the origin product carries `credit.Micro`/`credit.Dollar` and `payment.Currency` as separate untyped ideas, and a gateway computes cost as `float64` USD-per-million before storing `cost_usd_micro int64`. Two products, three unit conventions, and a documented `-1` sentinel on the a gateway side that a signed ledger would read as a credit. Fix the unit before building anything on top of it.
---

# money

## Overview

`latere.ai/x/pay/money` is the smallest package in the repo and the one
every other package here imports. It defines the amount type, the
currency vocabulary, the conversions to and from a processor's minor
units, the rounding rule, and the purchase spread formula.

Stdlib only. No I/O. Every function is pure, which is what lets the
ledger call it inside a transaction and the rate card call it on the
hot path.

## Why a defined type and not `int64`

`type Micro int64` costs a conversion at each seam and buys the one
thing raw `int64` cannot: the compiler refuses to add cents to
micro-USD, or a per-million rate to an amount. Those are exactly the
two mistakes that are invisible in review and expensive in production.
a gateway already proves the hazard: `internal/rates/rates.go` returns
`cost_usd_micro = -1` for an unpriced model, a magic value that is safe
only because today's consumer is a counter that filters it. A signed
ledger would read it as a one-micro credit.

So: `Micro` is a defined type, and the ledger's write API takes an
unsigned magnitude and applies the sign itself (spec 004).

## Surface

```go
package money

// Micro is an amount in millionths of one currency unit. int64, never a
// float: a float cannot represent a tenth of a cent exactly, and money
// that does not round-trip through storage is money that drifts.
type Micro int64

const (
    Unit   Micro = 1          // one micro
    Cent   Micro = 10_000
    Dollar Micro = 1_000_000
)

// Currency is a presentment currency, a closed vocabulary.
type Currency string

const (
    USD Currency = "usd"
    EUR Currency = "eur"
)

func (c Currency) Valid() bool
// Exponent is the number of decimal places the currency's minor unit
// carries: 2 for USD and EUR, 0 for JPY when it is added. It exists so
// a processor's "smallest unit" conversion is a table lookup rather
// than a hardcoded 100.
func (c Currency) Exponent() int

// MinorUp converts to the processor's smallest unit, rounding away from
// zero. This is the direction that charges a customer, so a fraction of
// a cent is charged rather than absorbed.
func (m Micro) MinorUp(c Currency) int64
// MinorDown rounds toward zero. This is the direction that quotes a
// purchase back to a person, so the quote is never above what is charged.
func (m Micro) MinorDown(c Currency) int64
// FromMinor is the inverse.
func FromMinor(n int64, c Currency) Micro

// FromUSD converts a float USD amount, rounding away from zero. It is
// the one sanctioned float boundary, for rate cards that are quoted as
// floats (the gateway's `internal/rates`). Negative input is an error rather
// than a negative amount: a cost is a magnitude, and the gateway's -1 sentinel
// must not silently become a credit.
func FromUSD(f float64) (Micro, error)

// String formats for display: "$1.23", "€0.0451". Display only; never
// parsed back.
func (m Micro) String(c Currency) string

// Ceil divides rounding away from zero. Exported because the ledger and
// the rate card both need the same bias and must not each re-derive it.
func Ceil(num, den int64) int64
```

### Rounding rule

One rule, stated once: **every conversion that decides what somebody is
charged rounds away from zero; every conversion that quotes a number
back rounds toward zero.** The bias is at most one micro per operation
and it always favours the platform. The opposite bias, repeated across
millions of gateway calls, is the platform paying for rounding.

Formally, for a cost expressed as a rate per million tokens:

$$
\text{cost}_{\mu} = \left\lceil \frac{\sum_i n_i \cdot r_i}{10^6} \right\rceil
$$

where $n_i$ is the token count of class $i$ (fresh input, cached input,
cache write, output) and $r_i$ is that class's rate in micro-USD per
million tokens. One division at the end, not one per class: dividing
per class rounds each of them away and systematically undercharges.

## The purchase spread

Margin is taken at top-up (decided 2026-08-22). A gross purchase credits
less than it costs, and the cut is one formula both products share:

```go
// Spread is the platform's cut at purchase.
type Spread struct {
    Bps        int64 // percentage cut, in basis points
    FixedMicro Micro // fixed cut per purchase
}

// Credited is what a gross purchase lands in a wallet. Never negative:
// a purchase below the fixed cut credits zero, which is why a minimum
// purchase exists alongside this.
func (s Spread) Credited(gross Micro) Micro
```

$$
\text{credited} = \max\!\left(0,\; \text{gross} - \left\lfloor \frac{\text{gross} \cdot \text{bps} + 5000}{10^4} \right\rfloor - \text{fixed}\right)
$$

The percentage cut rounds half up. It is lifted verbatim from
`the origin product/internal/platform.Settings.Credited`, whose behaviour is
already covered by `TestCreditedAppliesTheSpread`, so the extraction is
provably equivalent.

The *values* (`Bps`, `FixedMicro`, and the minimum purchase) are policy
and stay in each product's settings store. Only the arithmetic moves.

## Tests

- Table tests over `MinorUp`/`MinorDown`/`FromMinor` for both currencies,
  including the boundaries: 0, 1 micro, 9,999 micros, exactly one cent.
- `FromUSD` refuses negatives and NaN/Inf, and rounds `0.0000001` up to
  1 micro rather than down to 0.
- `Spread.Credited` reproduces the origin product's existing assertions exactly,
  plus: zero spread credits the gross, a purchase below the fixed cut
  credits zero and never a negative.
- A fuzz test on `FromUSD` and on `String` (both take unconstrained
  input; `pkg`'s gold standard requires fuzzing those).
- Property: `FromMinor(m.MinorUp(c), c) >= m` for all non-negative `m`.

100% statement coverage is achievable and expected here.

## Out of scope

Foreign exchange. There is exactly one ledger currency (USD) and a
non-USD charge is converted once, by the processor, at the moment of
purchase. `Currency` exists so the *payment port* can present a price in
EUR, not so the ledger can hold euros.

## Dependencies

None. This is the leaf.

## Outcome

**Implemented** 2026-08-22 (`8014a79`), 100% statement coverage, three fuzz
targets.

Built as specced. Two notes for a reader:

- `Ceil` rounds away from zero in **both** directions and panics on a zero
  denominator, rather than returning a plausible number for a programming
  error.
- `String` widens to six decimals only for sub-cent amounts, so a real charge
  never displays as `$0.00`.
