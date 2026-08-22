---
title: Billing — user-facing handlers (/me/billing*)
status: complete
depends_on:
  - specs/.archive/025-impl-02-mock-service.md
  - specs/.archive/008-impl-06-handlers.md
affects:
  - internal/handler/billing.go
  - internal/handler/router.go
created: 2026-05-02
updated: 2026-05-08
author: codex
---

# `/me/billing*` handlers

JWT-protected reads and writes the dashboard consumes. Cella's
`BillingAuth.{Checkout,Portal}WithToken` already forwards the
user's session access token; these handlers must accept it and
reject the cella service token.

> **Delta (2026-05-08):** these routes accept an optional
> `?scope=org|principal` query parameter. Active payer is
> resolved per `billing.md` §"GET /me/billing[?scope=...]"; the
> handler passes a `Payer` (not a bare `orgID`) into the service.
> `PaymentView.vue` (already shipped) uses `scope=org` for
> `/orgs/:id/billing` and `scope=principal` for `/me/billing`.

## Routes

Wire in `internal/handler/router.go` next to other `/me/*` JWT
routes:

```go
mux.HandleFunc("GET  /me/billing",          jwtMW(h.handleGetMyBilling))
mux.HandleFunc("POST /me/billing/checkout", jwtMW(h.handleCreateMyCheckout))
mux.HandleFunc("POST /me/billing/portal",   jwtMW(h.handleCreateMyPortal))
```

All three resolve the active **payer** from the JWT plus the
optional `?scope=` query param:

```go
func (h *Handler) resolvePayer(r *http.Request, claims Claims) (billing.Payer, error) {
    scope := r.URL.Query().Get("scope")
    switch scope {
    case "org":
        if claims.OrgID == "" { return billing.Payer{}, errNoActiveOrg }
        id, err := uuid.Parse(claims.OrgID); if err != nil { return billing.Payer{}, err }
        return billing.OrgPayer(id), nil
    case "principal":
        id, err := uuid.Parse(claims.Sub); if err != nil { return billing.Payer{}, err }
        return billing.PrincipalPayer(id), nil
    case "":
        // default: active org if present, else principal
        if claims.OrgID != "" {
            id, err := uuid.Parse(claims.OrgID); if err != nil { return billing.Payer{}, err }
            return billing.OrgPayer(id), nil
        }
        id, err := uuid.Parse(claims.Sub); if err != nil { return billing.Payer{}, err }
        return billing.PrincipalPayer(id), nil
    default:
        return billing.Payer{}, errBadScope
    }
}
```

For `org` payer, also re-check membership (the principal might
have lost membership since the JWT was minted). For `principal`
payer, no extra check needed — the JWT already proves identity.

## Permission gates

Reuse the existing seeded permissions:

| Route | Org payer | Principal payer |
|---|---|---|
| `GET /me/billing` | `(billing, read)` against the org | self (always allowed) |
| `POST /me/billing/checkout` | `(billing, write)` against the org | self |
| `POST /me/billing/portal` | `(billing, write)` against the org | self |

`migrations/000007_seed_data.up.sql:11-14` defines
`(billing, read|write|delete|admin)`. Default grants on **org**
membership:

- `owner` → all four.
- `admin` → `read`, `write`.
- `member` → `read`.
- `viewer` → none on billing.

For **principal** payer, the JWT subject IS the payer — no RBAC
gate (the principal can always read and modify their own billing
state). Superadmin can read any principal's billing via
`/internal/billing/account/principal/{id}` (impl-04), not through
`/me/billing`.

For org payer, reuse `authz.Engine.CheckPermission(ctx,
principalID, orgID, nil, "billing", "read"|"write")`.

## Reject service tokens on `/me/billing/*`

Cella's `cella-billing` service-account JWT carries
`principal_type=service_account` (per the existing service-
account pattern). `/me/billing/*` handlers must:

```go
if claims.PrincipalType == "service_account" {
    writeErr(w, http.StatusForbidden, "forbidden",
        "service tokens cannot create user-scoped billing sessions")
    return
}
```

This closes the failure mode where a misconfigured cella deploy
falls back to using the service token for Checkout/Portal — the
exact bug spec 60 was originally written to prevent.

## Handler shapes

### `handleGetMyBilling`

```go
func (h *Handler) handleGetMyBilling(w http.ResponseWriter, r *http.Request) {
    claims := claimsFrom(r)
    if claims.PrincipalType == "service_account" { /* 403 — see below */ return }
    payer, err := h.resolvePayer(r, claims)
    if err != nil { writeErr(w, 400, "bad_request", err.Error()); return }
    if payer.Kind == billing.PayerOrg {
        if !h.authzAllow(r.Context(), claims.Sub, payer.ID, "billing", "read") {
            writeErr(w, 403, "forbidden", "billing.read required"); return
        }
    }
    out, err := h.billing.GetMyBilling(r.Context(), payer)
    if err != nil { ...; return }
    writeJSON(w, 200, out)
}
```

Response is the **nested** `MyBilling` shape from
`auth/specs/.archive/billing.md`'s "Wire contract — `GET /me/billing`."

### `handleCreateMyCheckout`

Body: `{"plan": "pro"}`. Plan name must be in
`BILLING_PLAN_PRICES_JSON`; otherwise `400 unknown_plan`. The
service maps name → Stripe Price ID and returns `{"url": "..."}`.

### `handleCreateMyPortal`

Body: `{}`. Returns `{"url": "..."}`. `404 no_customer` if the
payer has no `billing_customers` row (no Stripe customer to
portal into).

## Tests

`internal/handler/billing_handler_test.go`:

- `GET /me/billing` 200 with subscription, 200 with `null` when
  payer has no row, 403 when org-payer caller lacks `billing.read`.
  Run for both `?scope=org` (with active org in JWT) and
  `?scope=principal`.
- `POST /me/billing/checkout` 200 with mock URL, 400 on unknown
  plan, 403 when org-payer caller lacks `billing.write`, 403 with
  the service-token rejection message. Principal-payer path needs
  no perm check.
- `POST /me/billing/portal` 200 with mock URL, 404 when no
  customer, 403 service-token reject.
- Default scope (no `?scope=` param): with active org in JWT →
  org-scoped; without → principal-scoped. Test both paths.
- `?scope=org` with empty `org_id` claim → `400 bad_request`.

## Acceptance

- Cella's `dashboard_api.go:apiBillingCheckout` round-trips end-
  to-end against mock-mode auth.
- Service tokens cannot call `/me/billing/*` (regression test).
- All three routes appear under `jwtMW` in `router.go` and the
  router test diff stays tight.
