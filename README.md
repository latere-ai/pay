# pay

[![CI](https://github.com/latere-ai/pay/actions/workflows/ci.yml/badge.svg)](https://github.com/latere-ai/pay/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/latere.ai/x/pay.svg)](https://pkg.go.dev/latere.ai/x/pay)
[![Coverage](https://img.shields.io/badge/coverage-97%25-brightgreen)](CONTRIBUTING.md#coverage)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**Sell credit, hold a balance, and spend it, in Go.**

Two halves that work together or apart: a **payment port** for taking money
without hard-wiring a processor, and a **credit ledger** whose balances are
folded from an append-only history rather than stored.

```bash
go get latere.ai/x/pay
```

## Why

Most products that sell credit end up writing the same four things: a checkout
call, a webhook whose status codes are subtly wrong, a balance column that
drifts from its history, and a refund path nobody tested. Each is easy to get
almost right.

This library is those four, already gotten wrong once and fixed.

- A **webhook handler** with the status codes pinned, because returning the
  wrong one either loses a purchase or credits it twice.
- A **balance that cannot drift**, because it is never stored.
- **Holds**, so a burst of concurrent requests cannot each spend the same money.
- **Exactly-once settlement**, enforced by the database rather than by
  remembering.
- **Idempotency on every write**, so a replayed delivery moves a balance once.

## Quick start

Sell $20 of credit and put it in a wallet:

```go
processor := stripe.New(stripe.Config{
    SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
    WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
})
book := pgledger.New(pool) // after pgledger.Migrate(ctx, pool)

// Quote the cut before the redirect, and carry what it credits on the session.
spread := money.Spread{Bps: 500, FixedMicro: 30 * money.Cent}
gross := 20 * money.Dollar
credited := spread.Credited(gross) // $18.70

out, err := processor.CreateCheckout(ctx, pay.CheckoutParams{
    Email:      user.Email,
    Amount:     gross,
    Currency:   money.USD,
    SuccessURL: "https://example.com/wallet?ok=1",
    CancelURL:  "https://example.com/wallet",
    Meta:       map[string]string{"credited": strconv.FormatInt(int64(credited), 10)},
})
// redirect the browser to out.URL
```

Then mount the webhook and credit on a verified delivery:

```go
mux.Handle("POST /webhooks/stripe", pay.WebhookHandler(processor,
    func(ctx context.Context, e pay.Event) error {
        if e.Kind != pay.KindPaid {
            return nil
        }
        amount, _ := strconv.ParseInt(e.Meta["credited"], 10, 64)
        return book.Credit(ctx, ledger.Posting{
            Holder: ledger.NewHolder("user", e.Email),
            Amount: money.Micro(amount),
            Reason: "topup",
            Ref:    e.Ref, // idempotent: a replay credits once
            Actor:  "stripe:" + e.Ref,
        })
    }))
```

That is the whole money-in path. [Getting started](docs/getting-started.md)
walks through spending it.

## What is in the box

| Package | What it gives you |
|---|---|
| [`pay`](https://pkg.go.dev/latere.ai/x/pay) | The processor-agnostic port, the webhook handler, and an in-memory fake |
| [`pay/money`](https://pkg.go.dev/latere.ai/x/pay/money) | An integer amount type, a currency vocabulary, and one rounding rule |
| [`pay/ledger`](https://pkg.go.dev/latere.ai/x/pay/ledger) | Balances, holds, transfers, settlement, reversals |
| [`pay/ledger/pgledger`](https://pkg.go.dev/latere.ai/x/pay/ledger/pgledger) | The Postgres store, and enlistment in *your* transaction |
| [`pay/stripe`](https://pkg.go.dev/latere.ai/x/pay/stripe) | The Stripe adapter |
| [`pay/paytest`](https://pkg.go.dev/latere.ai/x/pay/paytest), [`pay/ledger/ledgertest`](https://pkg.go.dev/latere.ai/x/pay/ledger/ledgertest) | Conformance suites, so your own adapter or store is provably correct |

## Documentation

| Guide | For |
|---|---|
| [Getting started](docs/getting-started.md) | Selling your first credit, end to end |
| [The money model](docs/money-model.md) | Why a balance is a fold, and what a hold is for |
| [Webhooks](docs/webhooks.md) | The delivery contract, and the status codes that matter |
| [Writing an adapter](docs/adapters.md) | Supporting a processor this library does not |
| [Running Stripe](docs/stripe-operations.md) | The account settings that change what a customer is charged |

API reference lives on [pkg.go.dev](https://pkg.go.dev/latere.ai/x/pay).

## Design in one paragraph

Amounts are `int64` micro-USD, never floats. A write API takes an unsigned
magnitude and applies the sign itself, so a caller cannot turn a debit into a
credit by passing a negative. A balance is `SUM(amount)` over an append-only
table, so it cannot disagree with its own history; a mistake is corrected by
another entry, never by an update. Holds are excluded from a balance and
subtracted from what is *available*, which is what stops two concurrent
requests spending the same money. Every write that an outside system can replay
is idempotent on that system's reference.

## Testing your integration

Your money path is testable with no processor and no database.

- `ledger.NewMemStore()` is a complete ledger in memory, with the same
  guarantees as the Postgres store.
- `pay.MemProvider` is a complete processor fake. It records every checkout
  and charge and accepts synthetic webhook deliveries built with
  `pay.MemEvent`, so a test drives purchase, credit, and refund end to end.
  [Getting started](docs/getting-started.md#6-test-it-with-no-processor-at-all)
  shows the shape.

If you write your own processor adapter or ledger store, run the exported
conformance suite against it: `paytest.RunProviderContract` for an adapter,
`ledgertest.RunStoreContract` for a store. Both stores and the Stripe adapter
in this repository pass the same suites, and the store suite found four real
bugs in the Postgres store before any caller existed.

## Status

Used in production by [Latere](https://latere.ai). The API is `v0.x` and will
change where the design turns out wrong. A breaking change gets a minor
version bump and an entry in [`CHANGELOG.md`](CHANGELOG.md).

## Contributing

Issues and pull requests are welcome. [`CONTRIBUTING.md`](CONTRIBUTING.md)
covers building and testing a change, including the Postgres half of the
suite, the coverage floor, and how a release is cut. The design record, with
the approaches that were tried and rejected, is in [`specs/`](specs/README.md).

## License

[MIT](LICENSE)
