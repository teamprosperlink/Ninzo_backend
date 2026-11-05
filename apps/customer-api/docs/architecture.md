# Architecture: customer-api

Version: 0.1.0 (draft for owner review) · Updated: 2026-11-04

Covers only what is specific to this app. Stack: `docs/engineering/tech_stack.md`. Layout and module rules: `docs/engineering/project_structure.md`. Flow and failure design: `EDD.md`.

## 1. Context

```
Customer app (Flutter)
   | HTTPS /v1, WS /v1/orders/{id}/stream
   v
customer-api ----------> Razorpay (create order, refund)
   ^    |   \----------> Routing engine (live ETA)
   |    |
Razorpay webhook
        | shared modules in internal/
        v
Supabase Postgres (system of record)     Redis (live state, fan-out)
        ^                                   ^
        | outbox polling                    |
      worker --> FCM, SMS gateway           |
                                            |
      rider-sim (tests) / rider-api (later) +-- location, offers, Ready, arrival
```

## 2. Components

| Component | Role | Notes |
|---|---|---|
| `apps/customer-api` | HTTP and WebSocket handlers, request and response types, wiring | No SQL, no business rules |
| `cmd/worker` | Outbox delivery, timers, payment settlement retries, notifications | Shared across apps |
| `cmd/rider-sim` | Plays the rider side in tests and staging | Test only, never deployed to production |
| `apps/rider-api` (core) | Location, offers, Ready, handover | Built alongside this app, because rider location drives the customer's timer ([decision 0007](../../../docs/decisions/0007-zone-timer-order-flow.md)) |
| `cmd/tracker` | Location processing | Later. Until then, `tracking` runs in `rider-api`. **Proposed** |

**Expiries (Proposed).** Offer, packing and reservation expiry run in `cmd/worker`, by polling due rows in Postgres. A restart loses nothing, because they are rows and not memory. Every expiry handler must be safe to run twice.

**Zone events (Proposed).** `tracking` calls `orders` directly when a zone or perimeter is entered. They do not go through the outbox, because polling would delay the customer's timer.

## 3. App layout
From `project_structure.md`. Created when first needed.

```
apps/customer-api/
├─ main.go          wiring only
├─ CLAUDE.md  CHANGELOG.md  Dockerfile
├─ docs/            these documents
└─ internal/
   ├─ auth/  gates/  menu/  availability/
   ├─ orders/  payments/  stream/
   └─ support/  devices/
```

Handlers call each module's public surface. Example: `internal/orders` in this app calls `internal/orders` at the repo root through its public file, and never reaches its `store/`.

## 4. Data owned
Tables belong to shared modules, not to this app. The full list is in `docs/engineering/endpoint_data_map.md`, which wins if this differs. The customer flow needs these, named as in PLAN section 4:

| Module | Tables |
|---|---|
| `identity` | `customers`, `sessions`, `otp_requests` |
| `geo` | `zones`, `gates`, `gate_history`, `sweet_spots` |
| `catalog` | `menu_items`, `price_history`, `availability_rules` |
| `inventory` | `lots`, `scooter_stock`, `stock_ledger`, `reservations` |
| `orders` | `orders`, `order_items`, `order_legs`, `order_events`, `offers` |
| `payments` | `payments`, `refunds`, `webhook_events` |
| `platform` | Idempotency records, outbox |
| `audit` | `audit_events` |
| `notify` | Device tokens. **Proposed,** missing from PLAN |

`offers` may belong better to `dispatch`, because `dispatch` writes them. **Proposed:** move it there.

## 5. API layer
- Contract: `contracts/` (OpenAPI for HTTP, AsyncAPI for the stream). The contract comes first, and the handlers follow it.
- Conventions: `project_spec.md` section 1.
- Customer tokens are short-lived access tokens with a refresh token. No Supabase Auth ([0004](../../../docs/decisions/0004-supabase-postgres-database-only.md)).
- The Razorpay webhook is the only route with no bearer token apart from OTP. It is verified by signature against the raw request body.

## 6. Deployment
One container per service (`deploy/`). Hosting is open (Q1). Keep the service stateless, with state in Postgres and Redis, so it can restart at any time. A WebSocket client reconnects and re-fetches on restart.

## 7. Limits and risks

| Risk | Effect | Response |
|---|---|---|
| Supabase failover not generally available | A database outage stops orders and may break a promise in progress | Decide before real customers ([0004](../../../docs/decisions/0004-supabase-postgres-database-only.md)) |
| Routing engine down | No live ETA | No new offers, per EDGE. Running timers are unaffected, because they use pre-built polygons |
| Razorpay down | No new paid orders | Reject with a retryable error. Webhooks reconcile later |
| Redis down | Live positions and stream fan-out degrade | No new offers, because live position is unknown. Orders and stock stay correct. Zone checks must not depend on Redis alone |
| Pool limits | Too many services exhaust connections | Small pool per service ([0005](../../../docs/decisions/0005-supabase-setup.md)) |

## 8. History

| Date | Change | Decision | Changelog |
|---|---|---|---|
| 2026-11-04 | First draft | [0001](../../../docs/decisions/0001-one-repo-separate-services.md) to [0006](../../../docs/decisions/0006-console-web-separate-repo.md) | Unreleased |
| 2026-11-04 | Zone timer, core of `rider-api` built alongside | [0007](../../../docs/decisions/0007-zone-timer-order-flow.md) | Unreleased |
