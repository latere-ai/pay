# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

### Added

- **`pay.MemEvent` builds a refund.** The options `pay.WithReversalRef` and
  `pay.WithEmail` set the refund's own reference and the payer's email, so a
  test drives a purchase, its credit and its refund through the handler.
  Before, a refund built with `MemEvent` had no `ReversalRef`, and the
  documented handler refused it.
- `paytest.WithPaidDelivery` gives the conformance suite a signed, paid
  delivery. With it, the suite checks that your adapter turns it into
  `pay.KindPaid` with its reference, and refuses the same body with one byte
  changed with `pay.ErrBadSignature`.

### Changed

- **`pgledger.Migrate` adds a migration.** Version 2 adds the column
  `reverses` to `ledger_entries` and a unique index on it, so the database
  allows one reversal per entry. Run `Migrate` before the new release
  serves traffic, as at every start. A reversal written before the
  migration does not record its entry, so an entry reversed then can be
  reversed once more.
- **A reference used again for a different write is refused** with the new
  `ledger.ErrRefConflict`. A replay of the same write, with the same holder,
  kind, amount and group, still writes nothing and returns `nil`. Before,
  any write whose reference was already recorded returned `nil` and wrote
  nothing, so a credit or a hold could vanish behind another write's
  reference. A reversal whose `Ref` equals its `Of` is refused the same way.
  A reference that is blank or starts or ends with a space is refused with
  `ledger.ErrBadRef`.
- **An entry is reversed at most once.** A second reversal of the same
  entry under another reference, such as a dispute after a refund, writes
  nothing and returns `Effect{Applied: false}`. A transfer, a hold or a
  release cannot be reversed: `ledger.ErrNotReversible`.
- **A settled group stays closed.** A hold on a group that is already
  settled is refused with `ledger.ErrSettled`, a settlement that names
  another holder than the hold's with `ledger.ErrWrongHolder`, and a
  `Debit` that carries a group with `ledger.ErrDebitGroup`. Before, each of
  them was accepted and left credit held that nothing could release, or
  released it onto the wrong holder.
- `ledger.MemStore.Within` holds the store for the whole unit. Other writers
  wait until it ends, and see none of its entries if it fails. A unit must
  write through the `Ops` it is given: a call on the store itself from inside
  it waits forever.
- `ledger.CheckReversal` and `ledger.CheckReplay` are exported, so a store
  you write refuses the same reversals and treats the same writes as
  replays as the two stores here.
- **A checkout or a charge for zero or a negative amount is refused** with
  the new `pay.ErrNotPositive`, by the Stripe adapter before it calls Stripe,
  and by `pay.MemProvider`. `MemProvider` also refuses a currency `money` does
  not know, with `money.ErrCurrency`. Before, Stripe refused such a charge
  with its own error, and the fake accepted it.
- **The conformance suite is stricter.** `paytest.RunProviderContract` fails
  an adapter that accepts a delivery with no signature, so the factory you
  pass must configure a webhook secret: a `pay.MemProvider` without a
  `Secret` no longer passes. It also fails an adapter that accepts a
  checkout or a charge for zero or a negative amount.
- `pay.WebhookHandler` still answers 400 for every delivery the adapter
  refuses, but the answer says `bad signature` only when the signature
  failed, and `unusable delivery` for a delivery that verified and cannot be
  used, such as a paid session in another currency. The `at` field of the
  log line tells the two apart. With `pay.WithLogger`, a delivery answered
  200 without reaching your handler, because no processor or no handler is
  configured, is now logged as a warning.
- `pgledger` is built on pgx v5.12.0, up from v5.10.0.
- The Stripe adapter is built on stripe-go v87.1.0, up from v85.2.0. Its calls
  to Stripe now carry API version `2026-10-28.endive` instead of
  `2026-05-27.dahlia`; no parameter it sends and no field it reads changed
  between the two. Webhook deliveries are still read at the version the
  endpoint is registered on. A 409 or 5xx answer from Stripe is now retried
  under the same idempotency key, so a retried create returns the original
  checkout, customer or charge.

### Fixed

- **A replayed hold or transfer that carries a reference returns `nil`,**
  in both stores, also after the first attempt spent the funds. Before, it
  returned `ledger.ErrInsufficient`, and a caller could stop work that was
  paid for.
- **Reversing a transfer no longer creates money.** It reversed only the
  sending side, so the receiver kept what the sender got back.
- `pgledger.Migrate` from several processes against a database that has
  never had the ledger now succeeds. It created its version table before
  taking its lock, and all but one process failed.
- `ledger.MemStore.Within` no longer discards other writers' entries when a
  unit fails, and a `Transfer` or a `Settle` in `MemStore` that fails
  partway writes nothing.
- In Postgres, `Reverse` reports `Applied: true` to exactly one of several
  concurrent callers. Each of them was told it applied.
- A balance that does not fit an `int64` is an error that wraps
  `money.ErrRange`, in both stores. `MemStore` wrapped it around to a
  negative number, and Postgres failed with a database error.
- `NegativeHolders` in Postgres matches the namespace as written. It read it
  as a `LIKE` pattern, so `_` and `%` matched other namespaces.
- `money.Spread.Credited` no longer overflows. Above about $18 billion of
  gross at a 5% cut, the cut wrapped around to a negative number and the
  credit came out larger than the gross. A negative `Bps` or `FixedMicro`
  now counts as zero, where it credited more than was paid.
- `money.FromUSD` is exact for an amount that is a whole number of micros:
  `FromUSD(0.000123)` was 124 micros and is now 123. About one such value in
  a hundred came out one micro high. It also refuses 2^63 micros, which it
  let through and converted to a different number on each architecture.

### Security

- **The Stripe adapter refuses a signature dated more than 5 minutes in the
  future,** with `pay.ErrBadSignature`. The Stripe SDK refuses only old
  ones, so a delivery signed with a timestamp far ahead verified until that
  time.
- **The Stripe adapter refuses an amount in a delivery that does not fit an
  `int64` of micro-USD,** with an error that wraps `money.ErrRange`. Before,
  the conversion wrapped around without an error, and a large amount became
  a negative one.
- Printing a `stripe.Config` with `%v`, `%+v` or `%#v` no longer prints its
  API key or its webhook secret: each prints as `[redacted]` when set.
- A webhook signing secret made only of whitespace now refuses every
  delivery with `pay.ErrBadSignature`. Before, it was used as the signing
  key, so a delivery signed with the same blank key verified.
- **Built and tested with Go 1.27.3,** which fixes GO-2026-6611,
  GO-2026-6612, GO-2026-6613, GO-2026-6617 and GO-2026-6809, a flaw in the
  standard library's certificate verification that this code can reach. A
  program that imports this module builds with its own Go, so build it
  with 1.27.3 or later.

## v0.2.0 - 2026-09-24

### Changed

- A signature error from the Stripe adapter keeps the SDK's own error in its
  chain beside `pay.ErrBadSignature`, so `errors.Is` can tell a stale
  delivery (`webhook.ErrTooOld`) from a forged one.
- The identity gate now also reads the frontend for the retired admin flag
  and refuses a second copy of the authorizer envelope, the question and
  the decision that `latere.ai/x/pkg` declares once (ci-gate v0.42.0).
  Nothing changes for a caller of pay.
- The guides match the code. Webhooks lists every delivery the handler refuses
  with 400, not only a bad signature. Running Stripe says that automatic tax
  needs both `stripe.Config.Tax` and `pay.TaxAutomatic`. Getting started
  carries a complete offline test of the money path, and the package
  documentation links to the guides.

### Fixed

- The Stripe adapter refuses a checkout in any currency other than USD with
  `stripe.ErrNotUSD`, before it calls Stripe. A session created in EUR used to
  take the customer's payment and then be refused when its webhook arrived, so
  nothing was credited. `ChargeSaved` refuses the same way. A customer abroad
  still pays in their own currency through Adaptive Pricing on a USD checkout,
  which is unchanged.
- An off-session charge that needed 3-D Secure is now credited. `ChargeSaved`
  reports such a charge as `pay.ChargePending` and says to wait for the
  webhook, but the Stripe adapter ignored `payment_intent.succeeded`, so the
  charge completed and nothing was credited. That event is now `pay.KindPaid`,
  with the `Ref` that `ChargeSaved` returned as `Charge.Ref` and the `Meta` it
  was given, so a charge credited on its synchronous success and again from
  its delivery posts once. Subscribe the webhook endpoint to
  `payment_intent.succeeded`. Only intents `ChargeSaved` created are credited:
  it marks them with the metadata `pay_origin: saved_method`, and the intent
  behind a checkout is left to its session.

## v0.1.0 - 2026-08-22

### Added

- **The first release.** `money`, an integer amount type in micro-USD with
  a currency vocabulary and one rounding rule; the processor-agnostic
  payment port with its webhook handler and the in-memory `MemProvider`;
  the Stripe adapter; the credit ledger, with balances folded from an
  append-only history, holds, transfers, settlement and reversals, in an
  in-memory store and a Postgres store; and the conformance suites
  `paytest` and `ledgertest`.
