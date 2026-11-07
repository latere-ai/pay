# Security

## Reporting a vulnerability

Report one privately, through
[GitHub's private vulnerability reporting](https://github.com/latere-ai/pay/security/advisories/new)
or by email to security@latere.ai. Do not open a public issue for it. The
[organization's security policy](https://github.com/latere-ai/.github/blob/main/SECURITY.md)
sets the response times, how disclosure works, and the safe harbor for
research done in good faith.

pay is before v1.0.0, so there is one release line: a fix ships in the
next release, and the [changelog](CHANGELOG.md) says which release has it.

## What is protected

pay runs inside your server and with its permissions. What it handles
comes from people you may not trust: anyone on the internet can reach
your webhook endpoint, the buyer chooses what to type on the processor's
page, and a delivery is retried, duplicated and replayed. Against them,
pay commits to the following, and the named tests check each property.

Each test is named as `package.Test`. A name in quotes is a case of the
store contract in `ledger/ledgertest`, which runs against both stores,
as `ledger.TestMemStoreSatisfiesTheContract` and
`pgledger.TestPGStoreSatisfiesTheContract`.

| Property | Tests |
|---|---|
| A delivery that does not verify posts nothing: `WebhookHandler` answers 400 and never calls your handler. The Stripe adapter refuses a missing, tampered or foreign signature, and with a blank webhook secret it verifies nothing. | `pay.TestWebhookHandlerStatusCodes`, `stripe.TestParseWebhook_VerifiesTheSignature`, `stripe.TestParseWebhook_RefusesATamperedPayload`, `stripe.TestParseWebhook_RefusesASignatureFromAnotherSecret`, `stripe.TestParseWebhook_RefusesAnUnsignedDelivery`, `stripe.TestParseWebhook_ABlankSecretVerifiesNothing` |
| A Stripe signature dated more than 5 minutes in the past or in the future is refused, so a captured delivery cannot be replayed later. | `stripe.TestParseWebhook_RefusesASignatureOutsideTheToleranceWindow`, `stripe.TestParseWebhook_RefusesAFutureDatedSignature` |
| `WebhookHandler` reads at most 1 MiB of a body (`pay.WithMaxBody` changes it) before anything is verified. | `pay.TestWebhookHandlerBoundsTheBody` |
| Only a handler error asks the processor for a retry; every other outcome has a fixed status code. | `pay.TestWebhookHandlerStatusCodes`, `pay.TestWebhookHandlerNamesWhyItRefused` |
| Only a paid purchase credits. A bank payment that is confirmed later credits once, and a payment intent credits only when `ChargeSaved` created it; metadata you pass cannot claim that origin. | `stripe.TestParseWebhook_PaidSessionCarriesWhatTheLedgerNeeds`, `stripe.TestRegression_AnAsyncPurchasePaysTwiceAndMustCreditOnce`, `stripe.TestChargeSaved_MarksTheIntentItCreates`, `stripe.TestParseWebhook_ACheckoutsIntentLeavesTheCreditToItsSession` |
| The Stripe adapter charges and credits USD only, and refuses another currency before calling Stripe. An amount in a delivery that does not fit an `int64` of micro-USD is refused, never wrapped into a credit. | `stripe.TestCreateCheckout_RefusesACurrencyTheWebhookCannotCredit`, `stripe.TestParseWebhook_RefusesToCreditANonUSDSession`, `stripe.TestChargeSaved_RefusesWhatItCannotChargeSafely`, `stripe.TestParseWebhook_RefusesAnAmountThatDoesNotFit` |
| A checkout or a charge for zero or a negative amount is refused before any call to the processor. | `stripe.TestCreateCheckout_RefusesANonPositiveAmount`, `stripe.TestChargeSaved_RefusesANonPositiveAmount`, `pay.TestMemProviderRefusesANonPositiveAmount` |
| A declined card is `pay.ErrDeclined` and is not retried. An email is escaped in a customer search, and another processor's customer is refused. | `stripe.TestChargeSaved_DeclineIsErrDeclinedAndIsNotRetried`, `stripe.TestEnsureCustomer_EscapesTheSearchQuery`, `stripe.TestCreateCheckout_RefusesAnotherProcessorsCustomer` |
| Printing a `stripe.Config` does not print its API key or its webhook secret. | `stripe.TestConfigDoesNotPrintItsSecrets` |
| An amount is never a float. `money.FromUSD` refuses NaN, infinities, negatives and values out of range, and is exact for whole micros. A charge rounds up, so it never loses money. | `money.TestFromUSDRefusesNonFiniteAndOutOfRange`, `money.TestFromUSDRefusesTwoToTheSixtyThree`, `money.TestFromUSDIsExactOnWholeMicros`, `money.FuzzFromUSD`, `money.TestMinorUpNeverLosesMoney` |
| `money.Spread.Credited` never credits more than the gross or less than zero, for any input. | `money.TestCreditedNeverExceedsGross`, `money.TestCreditedIgnoresANegativeSpread`, `money.FuzzSpreadCreditedNeverExceedsGross` |
| A currency code cannot forge a sign in displayed text. | `money.TestAnUnknownCurrencyCannotForgeASign`, `money.FuzzString` |
| A write takes a magnitude and applies the sign itself, so a negative cannot turn a debit into a credit. | "a write takes a magnitude and refuses a negative" |
| A write with a reference moves a balance once, however often it is replayed, also after the funds it used are spent. The same reference on a different write is refused, never dropped in silence. | "a credit is idempotent on its reference", "a replayed hold or transfer is idempotent after the funds are spent", "a reference reused with another payload is refused", "a reference with surrounding space is refused" |
| A hold cannot take more than is available, also when many arrive at once, and a transfer cannot spend what a hold has taken. | "a hold cannot exceed what is available", "concurrent holds cannot all be admitted against one balance", "a transfer may not spend what a hold has committed" |
| A group settles exactly once, against the holder of its hold. A hold on a settled group, and a debit that names a group, are refused. | "concurrent settlements of one group produce one debit", "a hold on a settled group is refused", "a settlement names the holder of its hold", "a debit cannot carry a group" |
| An entry is reversed at most once, by its exact amount, whatever the reference of the refund or dispute. A transfer, a hold or a release cannot be reversed. | "a reversal undoes the exact amount and dedupes on its own reference", "a reversal applies once per entry, whatever its reference", "concurrent reversals of one entry apply once", "a transfer, a hold or a release cannot be reversed", "reversing something unknown is refused, not guessed at" |
| A transfer moves both sides or neither, and a unit of several writes that fails moves nothing, without undoing other writers' entries. | "a transfer moves both sides or neither", "a unit that fails moves nothing", "a failed unit keeps what other writers wrote", `ledger.TestATransferInterruptedBetweenItsSidesMovesNothing`, `ledger.TestASettlementInterruptedBetweenItsEntriesMovesNothing` |
| A balance that does not fit an `int64` is an error, never a wrapped number. | "a balance that does not fit in int64 is an error" |
| The Postgres store fails closed: every operation on a dead pool, and a fault at any statement, is an error. A hold made with `Bind(tx)` commits and rolls back with your transaction. Migrating from several processes at once is safe. | `pgledger.TestEveryOperationFailsClosedOnADeadPool`, `pgledger.TestOperationsPropagateAFaultAtEveryStep`, `pgledger.TestBindCommitsWithTheCallersTransaction`, `pgledger.TestConcurrentMigrateOnAFreshDatabase` |
| The adapter conformance suite fails an adapter that accepts a delivery it did not verify. | `paytest.TestContractRejectsAnAdapterThatAcceptsForgeries` |

## What is not defended

- **The webhook signing secret.** Whoever holds it can make any
  delivery verify: its amount, its metadata, its email and its
  reference are taken from the delivery as they are. Keep it in a secret
  store, one per product.
- **Events of another product.** One processor account often serves
  several products, and each endpoint receives the account's events. pay
  does not tell yours from theirs. Your handler must return `nil` for a
  purchase it never recorded; [Webhooks](docs/webhooks.md#idempotency-is-not-optional)
  shows how.
- **A delivery that verifies and cannot be used,** such as a paid
  session in another currency. It is answered 400, the processor retries
  it, and it is refused each time. The customer was charged and nothing
  is credited until someone acts on the log.
- **Partial refunds and won disputes.** A refund or a dispute reverses
  the whole credit. A dispute you win is not modeled, so its credit stays
  reversed.
- **Authorization.** Any caller of a `ledger.Store` can read and write
  any holder. Who may spend from which wallet is your product's decision.
- **The database operator.** The ledger is append-only by this code's
  convention. Postgres does not enforce it: anyone with write access to
  `ledger_entries` can change history.
- **Isolation above READ COMMITTED.** A hold made with `Bind(tx)` is
  exclusive only at READ COMMITTED, the Postgres default. Under
  REPEATABLE READ or SERIALIZABLE, two holds can both be admitted against
  the same money.
- **The case of a holder id.** `ledger.NewHolder("user", "Ada@Example.com")`
  and `ledger.NewHolder("user", "ada@example.com")` are two wallets.
  When your product does not fill in the buyer's email, the Stripe
  adapter takes the one the buyer typed, as typed. Normalize an id before
  you make a holder of it.
- **Paging across equal timestamps.** `ledger.Page.Before` skips entries
  that share a timestamp with the last entry of the page before.
- **Advisory locks.** The Postgres store locks a holder with
  `pg_advisory_xact_lock(hashtext(holder))`, in the same key space as
  every other user of advisory locks in the database. A collision makes
  two writers wait for each other; it cannot admit a spend twice.
- **The webhook endpoint's limits.** `WebhookHandler` has no rate limit,
  no timeout and no method check. Put it behind your server's.
- **`pay.MemProvider` without a `Secret`.** It accepts any delivery, as
  its documentation says. It is for tests.
- **Telemetry to Stripe.** The Stripe SDK sends the timing of earlier
  requests to Stripe with each request, as it does by default.

## Audit

pay has had no external security audit.

## Supply chain

pay is a Go module and ships as source: there are no binaries and no
images. A release is a version tag on `main` with release notes. The Go
tools check each module download against the
[checksum database](https://sum.golang.org), so a tag cannot change after
it is published. `money`, `pay` and `ledger` import the standard library
only; `pgledger` adds `github.com/jackc/pgx/v5`, and `stripe` adds
`github.com/stripe/stripe-go/v87`. To check what you have:

```sh
go mod verify
govulncheck ./...
```

A vulnerability check over pay and its dependencies is in the quality bar
that runs on every push. The module is built and tested with Go 1.27.3,
which fixes vulnerabilities in the standard library of earlier 1.27
releases; build your program with 1.27.3 or later.
