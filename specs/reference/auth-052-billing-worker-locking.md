---
title: Billing worker — claim rows with SKIP LOCKED + leader lock
status: complete
track: auth-rework
depends_on:
  - specs/.archive/051-auth-rework.md
affects:
  - internal/billing/worker.go
  - internal/billing/store.go
  - internal/billing/stripe.go
effort: medium
created: 2026-07-05
author: changkun
trigger: P8 from the audit
---

# Billing worker — safe concurrent draining

## Problem

The meter-push worker (`worker.go`) polls every 30s and drains via
`UnsentMeterPushes` (LIMIT, ordered) but with no row locking or leader
lock, so with >1 replica every worker selects the same rows and
double-processes (Stripe idempotency + the idempotency_key PK prevent
duplicate charges, but not the wasted work and `MarkMeterPushed` races).
`pushOne` also does a per-row `GetCustomer` query plus a blocking Stripe
call, serially, capping throughput at ~batch/tick.

## Approach

1. Claim rows with `SELECT ... FOR UPDATE SKIP LOCKED` inside the draining
   transaction so concurrent workers take disjoint batches; or gate the
   whole tick behind a `pg_advisory_lock` (single-leader) if simpler.
2. Batch the customer lookups (one query for the batch's org→customer map)
   instead of one per row.

## Acceptance

- A test runs two drain passes concurrently against the same staged rows
  and asserts each row is pushed exactly once (no double MarkMeterPushed).
- Customer lookups for a batch are a single query.

## Risks

- Long-held row locks if the Stripe call blocks inside the tx. Mitigate by
  claiming+marking in a short tx and doing the Stripe call outside it, or
  keep the advisory-lock single-leader model where lock duration is bounded.

## Current state (2026-07-11)

**~60% done; REMAINING slice only.** `FOR UPDATE SKIP LOCKED` drain is shipped (`billing/store.go`, migration `000044`, concurrency test) — the correctness-critical double-processing risk is closed. **Remaining:** batch the per-`Payer` `GetCustomer` lookups (throughput only). Advisory leader lock is an explicit "or" the SKIP-LOCKED design already satisfies; not needed.

## Outcome (2026-07-11)

COMPLETE. `FOR UPDATE SKIP LOCKED` drain was already shipped; the remaining batch-lookup landed: `Store.GetOrgCustomers` resolves all orgs in one query and the drain pushes from the in-memory map, preserving the missing=permanent / transient=retry classification. Commit `4d69f9f`.
