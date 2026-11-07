# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

### Changed

- The Stripe adapter is built on stripe-go v87.1.0, up from v85.2.0. Its calls
  to Stripe now carry API version `2026-10-28.endive` instead of
  `2026-05-27.dahlia`; no parameter it sends and no field it reads changed
  between the two. Webhook deliveries are still read at the version the
  endpoint is registered on. A 409 or 5xx answer from Stripe is now retried
  under the same idempotency key, so a retried create returns the original
  checkout, customer or charge.

### Security

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
