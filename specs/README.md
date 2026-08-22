# pay specs

How this component is built. One spec per package, tightly scoped.

The **cross-repo migration** that created `pay` (deleting the dead
billing scaffolding from auth and agents, migrating replichai onto the
shared ledger, and building Lux's credits lane) lives in the private
`latere-ai/specs` repo under `infrastructure/pay/`. It is not duplicated
here: that work changes other repos, this directory describes this one.

## Contents

| Spec | Package | Topic | Status |
|---|---|---|---|
| [001-money.md](001-money.md) | `money` | The amount type, currency vocabulary, rounding rule, and purchase-spread formula every money path shares | ✅ implemented |
| [002-payment-port.md](002-payment-port.md) | root | The vendor-neutral port: hosted checkout, off-session charge against a saved method, verified webhooks reduced to a flat event | ✅ implemented |
| [003-stripe-adapter.md](003-stripe-adapter.md) | `stripe` | One proven integration, one design reference | ✅ implemented |
| [004-credit-ledger.md](004-credit-ledger.md) | `ledger` | Append-only micro-USD ledger: balance as a fold, holds, exactly-once settlement, idempotent writes, and the rollup shape a high-rate gateway needs | ✅ implemented |
| [005-stripe-operations.md](005-stripe-operations.md) | — | Running the account: webhook events, the settings that change what a customer is charged, the local loop, rollout | drafted |

Build order: 001 first, then 002 and 004 in parallel, then 003. 005 is
operational and can be followed as soon as 003 exists.

## Prior art

`latere-ai/auth` built a complete Stripe integration and never drove it
end to end; it was removed unused in August 2026. Nothing of it is
carried here verbatim. What it worked out that was worth keeping is
written into these specs in this repo's own terms: the test harness and
the API-version posture in 003, the account settings and the async-payment
event pair in 005.

The one integration in the family that has taken a payment is
replichai's. Where it and any other source disagree, it wins.

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
