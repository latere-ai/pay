---
title: Billing — meter push worker
status: complete
depends_on:
  - specs/.archive/027-impl-04-internal-handlers.md
  - specs/.archive/028-impl-05-stripe-checkout-portal.md
affects:
  - internal/billing/stripe/meters.go
  - internal/billing/worker.go
  - cmd/latere-auth/main.go
created: 2026-05-02
author: codex
---

# Meter push worker

Drains `billing_meter_pushes WHERE pushed_at IS NULL` to Stripe
Meter Events. Cella's reporter posts deltas (sandbox spec 60);
the worker forwards each delta to Stripe.

## Lifecycle

Boot from `cmd/latere-auth/main.go`:

```go
if cfg.Billing.Enabled {
    w := billing.NewWorker(billing.WorkerConfig{
        Service:  billingSvc,
        Interval: cfg.Billing.WorkerInterval, // default 30s
        Batch:    100,
        Log:      log,
    })
    go w.Run(ctx)
}
```

Single-process worker. v1 auth runs as a single replica; if/
when auth scales horizontally, gate this with a leader lease
the same way cella does. **Out of scope for v1** — note in
impl-09 rollout doc.

## Run loop

```go
func (w *Worker) Run(ctx context.Context) {
    t := time.NewTicker(w.cfg.Interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-t.C:
            n, err := w.svc.DrainMeterPushes(ctx, w.cfg.Batch)
            if err != nil { w.log.Warn("drain failed", "err", err) }
            else if n > 0 { w.log.Info("drained meter pushes", "n", n) }
        }
    }
}
```

## `DrainMeterPushes`

```go
func (s *stripeService) DrainMeterPushes(ctx context.Context, limit int) (int, error) {
    rows, err := s.store.UnsentMeterPushes(ctx, limit)
    if err != nil { return 0, err }
    drained := 0
    for _, row := range rows {
        if err := s.pushOne(ctx, row); err != nil {
            _ = s.store.MarkMeterPushed(ctx, row.IdempotencyKey, err)
            continue
        }
        if err := s.store.MarkMeterPushed(ctx, row.IdempotencyKey, nil); err != nil {
            return drained, err
        }
        drained++
    }
    return drained, nil
}
```

`pushOne` calls Stripe's `meter_event.New` (or the v76 SDK
equivalent) with:

```go
&stripe.MeterEventParams{
    EventName: row.Meter,
    Identifier: stripe.String(row.IdempotencyKey),
    Timestamp:  stripe.Int64(row.PeriodStart.Unix()),
    Payload: map[string]string{
        "stripe_customer_id": cust.StripeCustomerID,
        "value":              strconv.FormatInt(row.Quantity, 10),
    },
}
```

`Identifier` is the cella-supplied idempotency key. Stripe
dedupes by event identifier within ~24h; combined with our PK
on `billing_meter_pushes.idempotency_key`, double-billing is
not possible.

`Timestamp = PeriodStart` is intentional: Stripe assigns the
event to the period containing the timestamp. v1 always uses
the period start; if Stripe's billing period doesn't align with
calendar months for some plan, a future iteration can pass the
event time. v1 documents this constraint.

## Error handling

- Stripe transient (5xx, network) → leave `pushed_at NULL`,
  set `error="transient: ..."` and retry next tick. Add a
  bounded retry (after 24h of failures, surface in admin UI).
- Stripe 4xx (invalid meter, missing customer) → set
  `pushed_at = now()` AND `error="permanent: ..."` so the row
  doesn't loop forever. Surface in admin UI; ops investigates.
- Cella sent a meter cella thinks exists but Stripe doesn't →
  `permanent`; ops fixes Stripe-side meter config.

## Tests

`internal/billing/worker_test.go`:

- Drains N rows successfully against a mock Stripe transport.
- Transient Stripe error leaves `pushed_at` null and sets
  `error`.
- Permanent Stripe error sets `pushed_at` and `error`
  (idempotent next-tick).
- Empty queue → 0 drained, no Stripe calls.
- Mock-mode `DrainMeterPushes` (impl-02) marks all rows pushed
  with no Stripe calls.

## Acceptance

- Worker drains the staging table at the configured interval.
- Stripe sees one meter event per cella-reported delta.
- Permanent failures don't loop; transient ones retry; admin UI
  can see both.
