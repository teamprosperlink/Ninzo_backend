# Milestones and stages: customer-api

Version: 0.2.0 (draft for owner review) · Updated: 2026-11-04

This is the customer view of the plan. The fixed plan, agreed on 2026-11-04, is in `docs/planning/milestones.md`, and that file wins if the two differ.

Feature IDs are from `project_spec.md`. The KT milestones doc is deliberately not time-based, so no dates are given here.

## Versions
Rider location drives the customer's timer ([decision 0007](../../../docs/decisions/0007-zone-timer-order-flow.md)), so these versions include the core of `rider-api`.

| Version | Goal | Stages | Exit criteria |
|---|---|---|---|
| v0.1.0 | App developers can build against mocks | S1 | Mocks answer every customer and core rider endpoint and scenario. Contracts reviewed with the app developers |
| v0.2.0 | Foundations | S2 | `task verify` green in CI. Migrations apply. Sign-in works on staging |
| v0.3.0 | Browsing on real data | S3 | A test customer sees real availability. Zones and settings are stored and can be changed |
| v0.4.0 | A paid order holds stock correctly | S4 | Concurrency and payment tests pass, using Razorpay test keys |
| v0.5.0 | **One full order works end to end** | S5, S6 | A real order runs from payment to handover, with GPS starting and stopping the timer. Tested with the core of `rider-api` and a simulator |
| v0.6.0 | Every failure ends properly, and the customer feature set is complete | S7, S8 | Every test in EDD section 10 passes. All P1 CA features meet "Done when" |

`v1.0.0` is system-wide, covering all P1 features, so it also needs the rest of `rider-api` (v0.7.0), `partner-api` (v0.8.0) and `console-api` (v0.9.0 and v0.10.0).

## Stages

| Stage | Name | Features | Depends on | Must be settled first |
|---|---|---|---|---|
| S1 | OpenAPI, AsyncAPI, mocks, fixtures | All (contract only), plus core rider endpoints | GitHub access | Handover OTP (EDD 9.2), missing endpoints |
| S2 | Platform: config, migrations, idempotency, audit, outbox, CI, sign-in | CA-01 | S1 | Migration tool, Supabase project |
| S3 | Gates, zones, polygons, menu, availability, settings | CA-02, 03, 16 | S2 | Gate policy (Q3), pilot city (Q6) |
| S4 | Orders, reservations, Razorpay, cancel | CA-04, 05, 11 | S3 | None for payment: charged up front (owner decision) |
| S5 | Core of `rider-api`, location, offers, packing, simulator | CA-06, 08, 18 | S4 | Minimum wait and packing limit values |
| S6 | Zone timer, arrival, handover | CA-07, 09 | S5 | Late events (Q8) |
| S7 | Remedies and no-scooter paths | CA-10, 15 | S6 | Credit amount, no-show policy |
| S8 | Notifications, history, help, masked call | CA-12, 13, 14, 17 | S4 | SMS gateway, telephony provider |

Gate rule (`CONTRIBUTING.md`): the next stage starts only after the current one is tested and validated.

## What each stage's evidence must show
- S1: Schemathesis passes against the mock. Every `X-Mock-Scenario` returns schema-valid data.
- S2: `task verify` green in CI. A migration applies cleanly to Docker Postgres with PostGIS.
- S3 to S8: the matching EDD section 10 tests, with their real output in `docs/testing/reports/<issue>.md`.

## Mock scenarios required for S1
From CUST, "Backend needed before you start": no scooter available; offer timeout; offer accepted; payment declined; payment captured; order handed over; socket drop and reconnect; last unit taken by another order; gate not verified; zone with the 90 s offer switched off.

Added by decision 0007: rider already in the zone; rider already inside 100 m; timer running; timer ended before 100 m; rider arrived at 100 m; maximum wait passed.
