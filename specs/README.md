# pay specs

How this component is built. One spec per package, tightly scoped.

The **cross-repo migration** that created `pay` (retiring unused billing
scaffolding elsewhere, moving the origin product onto the shared ledger, and
building the gateway's credits lane) is tracked outside this repository. It is
not duplicated here: that work changes other codebases, this directory
describes this one.

## Contents

| Spec | Package | Topic | Status |
|---|---|---|---|
| [001-money.md](001-money.md) | `money` | The amount type, currency vocabulary, rounding rule, and purchase-spread formula every money path shares | ✅ implemented |
| [002-payment-port.md](002-payment-port.md) | root | The vendor-neutral port: hosted checkout, off-session charge against a saved method, verified webhooks reduced to a flat event | ✅ implemented |
| [003-stripe-adapter.md](003-stripe-adapter.md) | `stripe` | One proven integration, one design reference | ✅ implemented |
| [004-credit-ledger.md](004-credit-ledger.md) | `ledger` | Append-only micro-USD ledger: balance as a fold, holds, exactly-once settlement, idempotent writes, and the rollup shape a high-rate gateway needs | ✅ implemented |

Build order: 001 first, then 002 and 004 in parallel, then 003.

Operator-facing material lives in [`../docs/`](../docs/), not here. These specs
are the internal design record: why the shape is what it is, and what was tried
and rejected.

## Prior art

A second Stripe integration was written elsewhere and never driven end to end;
it was removed unused in August 2026. Nothing of it is carried here verbatim.
What it worked out that was worth keeping is written into these specs in this
repo's own terms: the test harness and the API-version posture in 003, and the
account settings and async-payment event pair now written up in
[`../docs/stripe-operations.md`](../docs/stripe-operations.md).

The one integration that has actually taken a payment is the origin product's.
Where it and any other source disagree, it wins.

## Decisions of record

Taken 2026-08-22, before implementation.

| Question | Decision |
|---|---|
| Merchant of record? | Stripe direct now, port shaped so an MoR adapter fits later |
| One repo or a port/adapter split? | One repo. `latere.ai/x/pkg` gets nothing |
| Where does the ledger of record live? | Here, embedded per product against the product's own database |
| Budget enforcement? | The product enforces at its own request path; this is the authority its counter reconciles against |
| Credits data model? | One ledger, namespaced holders, one balance per holder |
| Where is margin taken? | A purchase spread at top-up |
| A finance daemon? | Deferred. Nothing would call it today |
