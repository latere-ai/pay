# pay

Latere's finance component. One repository for money: the vendor-neutral
payment port, its processor adapters, and the credit ledger.

Every product that takes a payment or holds a balance imports this. No
product implements any of it twice.

```go
import (
    "latere.ai/x/pay"                    // Provider, Event, CheckoutParams
    "latere.ai/x/pay/money"              // Micro, Currency, Spread
    "latere.ai/x/pay/ledger"             // Store, Ops, Holder
    "latere.ai/x/pay/ledger/pgledger"    // the Postgres store
    "latere.ai/x/pay/stripe"             // an adapter, at a composition root only
)
```

## What it owns

| Concern | Package |
|---|---|
| The amount type, currency vocabulary, rounding rule, purchase spread | `money` |
| The payment port: checkout, off-session charge, verified webhooks | root |
| Processor adapters. Stripe today | `stripe` |
| The append-only credit ledger: balances as folds, holds, exactly-once settlement | `ledger`, `ledger/pgledger` |

## What it does not own

**Pricing.** What a token, a pod-second or a page costs is the product's
business. This repo holds amounts, never rate cards.

**Metering.** Products count what they used and report an amount.

**Authorization.** It takes a holder key and an amount and has no opinion
about who may spend. Auth remains the identity provider.

**Notification and policy.** `Reverse` returns before-and-after balances;
what a zero crossing *means* is the product's decision.

## Shape

A library that products embed. A product holding balances runs
`pgledger` against **its own database**, so a request never crosses a
service boundary to ask whether it may spend. There is no finance daemon
today; see the specs for why, and for where one would land if it is ever
needed.

## Rules

- Import direction is one way: `stripe` imports the root and `money`;
  `ledger` imports `money`; `money` imports nothing.
- Nothing imports `stripe` except a product's `main`.
- `money` and `ledger` are stdlib-only. `pgx` enters only through
  `pgledger`; `stripe-go` only through `stripe`.

## Development

```
make test        # go test
make race        # go test -race
make cover       # coverage, 95% floor enforced
make fuzz        # fuzz targets, 30s
```

The Postgres half of the ledger contract needs a database:

```
TEST_DATABASE_URL=postgres://... make test
```

A database-free run silently skips it, which is how a ledger can look
far less covered than it is.

## Specs

[`specs/`](specs/). The cross-repo migration that created this component
lives in the private `latere-ai/specs` repo under `infrastructure/pay`.

## License

[MIT](LICENSE)
