# PRD: customer-api

Version: 0.1.0 (draft for owner review) · Updated: 2026-11-04

Sources are in `docs/context/`:
- **PRD:** `KT/PRD.pdf`
- **CUST:** `KT/Master Doc of ninzo_customer_app.pdf`
- **ARCH:** `KT/Master Doc of ninzo_architecture.pdf`
- **EDGE:** `KT/EDge_cases.pdf`
- **PLAN:** `BACKEND_PLAN.md`

Open questions (Q1-Q6) are in `docs/planning/project_status.md`. Anything marked **Proposed** is not agreed.

## 1. Problem
Quick commerce is fast, but food loses its condition on the way: hot food cools and cold items melt. Urgent needs are not met by current delivery times. (PRD, Problem Statement)

## 2. Product goal
Keep sealed, temperature-tracked food inside scooters that wait near demand. (PRD, Product Info)

Once a packed rider is inside the zone around the customer's location, a visible timer runs, and the rider reaches that location within the time set in the console, 90 seconds by default. The timer covers reaching the location, not handover or door delivery. ([Decision 0007](../../../docs/decisions/0007-zone-timer-order-flow.md), overriding the PRD's wording.) Marketing and branding own the public claim.

**First proof:** one micro-market, 4-8 scooters, 1-3 menu items. (PRD)

## 3. What customer-api is for
`customer-api` is the server behind the Flutter customer app. It lets a customer pick a gate, see what is truly in reach, pay, and follow the order to handover, with honest timing at every step. (CUST, Purpose)

It owns the customer's HTTP and live API only. Business rules live in the shared modules, and `customer-api` holds no SQL (`docs/engineering/project_structure.md`).

## 4. Users
| User | Need |
|---|---|
| Customer | Get an item at an exact gate, with a promise that can be trusted |
| Customer app developer (Developer 2) | A stable contract and a mock server, before the real backend exists (CUST, Backend needed) |

## 5. Scope

### In scope (P1)
Sign-in, gates, availability, orders and payment, the wait for a scooter, the accepted countdown, arrival and handover, no-scooter options, cancel, history and reorder, help and refund status, push and SMS at each state. (CUST, feature table)

### Later (P2)
- Orders split across several scooters. The pilot starts single-leg because it has only 1-3 items (PLAN section 6).
- Demand forecasting and rider nudges (future suggestions doc).

### Out of scope
- The Flutter app, design and visual styling (CUST).
- Rider, partner and console APIs. The customer flow is proven with a test-only rider simulator first (project status, build order).
- DigiLocker, DLT SMS registration and FSSAI. Test OTP is used until then (CUST). These are still required before a real pilot (PLAN R6).

## 6. Promises the product must keep
From EDGE, "Rules that hold in every scenario", and CUST, "Real-time and payment rules":
1. **No promise without checks.** An offer appears only when stock, route, rider and gate checks pass.
2. **Safety beats the clock.** No rule may push a rider to rush.
3. **One clock, owned by the server.** No device can change the timer or arrival times.
4. **Every miss has a remedy.** A missed or failed order triggers a refund, credit or replacement automatically.
5. **Only the server marks an order paid,** through the Razorpay webhook.
6. **One tap, one order.**

## 7. Success measures
The sources set the 90 s promise and list what to watch, but no numeric targets except the promise itself. **The targets below are for the owner to set before the pilot.**

| Measure | Source | Target |
|---|---|---|
| Timers stopped at 100 m before they end | Decision 0007, EDGE (missed-promise rate) | Owner to set |
| Time from payment to timer start | Decision 0007 | Owner to set |
| Duplicate orders from repeated taps | CUST (test checklist) | 0, proposed |
| Oversold units | PLAN section 1 | 0, proposed |
| Misses with an automatic remedy | EDGE rule 4 | 100%, proposed |
| Orders lost to no coverage; empty-zone minutes | EDGE, scenario catalog | Owner to set |

## 8. Decisions needed from the owner
These change the customer's experience. Details are in the EDD, section 9.

| Topic | Proposed default | Reference |
|---|---|---|
| Minimum waits, packing limit and credit amount | Not set. They are console settings | `docs/engineering/data_flow.md` |
| Which gates can be used | Ops-verified list | Q3 |
| Pilot city | Not proposed | Q6 |
