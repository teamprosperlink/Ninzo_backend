# Project status: ninzo-backend

LIVE. Summary of the current situation. GitHub Issues are the source of truth; if they disagree, the Issues win.

Last updated: 2026-11-04

## Current milestone
`v0.1.0`: contracts and mock servers. The owner gave the go-ahead on 2026-11-04.

## In progress
| Issue | Feature | Phase | Branch | State |
|---|---|---|---|---|

## Completed (shipped, with evidence)
| Issue | Feature | Version | Test report |
|---|---|---|---|
| #1 | Project setup | Before `v0.1.0` | PR #2, CI passed |
| #5 | Shared API rules | `v0.1.0` | [reports/5.md](../testing/reports/5.md) |

## Blocked
| Issue | Blocker | Since |
|---|---|---|

## Next actions
1. `v0.1.0`, one phase at a time: #6 customer contract, #7 rider contract, #8 mock servers, #9 contract review.
2. Owner supplies what `v0.1.0` needs: see section 1 of `docs/planning/owner_setup_checklist.md`.
3. Owner reviews `docs/engineering/tech_stack.md`, decisions 0001-0007 and `apps/customer-api/docs/`. Each decision moves to Accepted after review.

GitHub: `teamprosperlink/Ninzo_backend`. `main` cannot be protected on the current plan, so the PR rule is followed by discipline, not enforced.

Old folders in `Ninzo/` were deleted on 2026-11-04. Backup, removed only when the owner says so: `Github-prosperlink/ninzo-old-folders-backup-2026-11-04.tar.gz`.

Build order, set by the owner: all customer and rider APIs, then partner, then admin and ops console. Plan: `docs/planning/milestones.md`.

## Recent decisions
Agreed with the owner on 2026-11-04. Written up in `docs/decisions/` 0001-0006, awaiting owner review of the write-ups.
- One repo, one `go.mod`, with each API deployed as its own service.
- Go for all backends. Python only for offline jobs in `tools/`. FastAPI only if a live forecast model is needed later.
- Redis and a Postgres outbox to start, instead of Kafka or NATS.
- Supabase Postgres, used only as a database: no Supabase Auth, Storage, Realtime or Edge Functions. Revisit RDS Multi-AZ before the pilot takes real customers, because Supabase automatic failover was found only as a private alpha.
- Supabase setup: Mumbai region, session pooler, small connection pool per service, outbox by polling, Data API off or granted nothing, PostGIS in the `extensions` schema, schema changes through migrations only, Docker Postgres for local work and tests.
- Console frontend goes in a separate `console-web` repo.

## Order and timer flow (owner decision, 2026-11-04)
Overrides the KT docs and the earlier plan. Full flow: `docs/engineering/data_flow.md`, section 1. Decision 0007.
- Payment is outside the timer. The customer then sees "looking for riders", then "rider has accepted", while the rider is already allotted in the backend.
- A rider already in the zone gets a shorter pre-timer wait, never zero, to allow for packing.
- The rider packs first, while stopped, then rides.
- The timer starts automatically from GPS on entering the zone. It stops at the 100 m perimeter, showing "rider has arrived at your location". The map stays live.
- The promise is reaching the location, not handover or door delivery.
- Timer length and zone radius are console settings. Radius defaults to 350 m and varies by location; the owner will write the location-based algorithm later.
- Locations and polygons are pre-built, then mapped through Google Maps.
- The rider sees a calm "1.5 mins", not a bumper-style 90 s countdown.
- Marketing and branding own the public claim and wording.
- Starting values, proposed by engineering and accepted as defaults when the owner said to start on 2026-11-04: timer starts when packed and inside the zone (packed means Ready pressed or packing limit passed); 5 minute maximum wait before the timer; credit if the timer ends before 100 m; free cancel until "rider has accepted", or once the maximum wait passes.
- Every task starts with a GitHub Issue. See `CLAUDE.md`.
- GPS alone stops the timer. OTP is the proof of delivery. The required QR or code scan on arrival, and `POST /v1/orders/{id}/arrive`, are dropped.
- Claude merges PRs once checks pass, without owner sign-off on each PR.
- Milestones are one combined line, with `app:*` labels to filter by app.
- Other developers build all Flutter apps, including the partner app. Claude builds only the hub APIs for it.
- The Google Doc [Ninzo Backend: Shared Index](https://docs.google.com/document/d/1p5sGHeYSo4cCCLlAErqLLqEMnOPkZvk2-gEA4fKgO6M/edit) is the orchestrator for communicating with the app developers, and the index of all shareables. GitHub stays the source of truth.
- Supabase: Ninzo gets its own new project in Mumbai when `v0.2.0` starts, and everything is built around it. The existing project, `teamprosperlink's Project`, belongs to a different app and must never be touched.
- Mock server: a small, cheap host now. Railway proposed. Production hosting is decided later.

## Open questions for the owner
Proposed defaults are not yet agreed.
1. Backend and Redis hosting. Proposed: AWS Mumbai. Does not block docs.
2. Decided by the owner on 2026-11-04: backend only for Claude, and other developers build the Flutter apps. Still open: who builds the `console-web` frontend?
3. Gates: any customer pin, or only verified gates? Proposed: ops-verified list, because sweet spots are computed offline and nothing sets `verified`.
4. Decided by the owner on 2026-11-04: see "Order and timer flow" above.
5. Payment holds with UPI: confirm with Razorpay. If holds are not possible, charge at order and auto-refund on failure, which changes the app wording.
6. Pilot city (sets map data). Do scooters have temperature sensors? Proposed: scan-only for the pilot.
7. Decided by the owner on 2026-11-04: see "Order and timer flow" above.
8. Official time for a location sent late from a dead zone. Proposed: server receipt time, with late events inside the promise sent to audit review. See EDD 9.3.

## Known gaps in the customer KT doc
To resolve in the customer docs and contract.
- Handover OTP: customer and rider apps both submit it, and no response gives the customer an OTP to show. Proposed: server issues it, customer app displays it, rider submits it.
- Payment wording: the feature table says "nobody is charged", while rule 7 forbids that wording.
- Payment and dispatch order: the architecture doc sets `MATCHING` at creation, before payment. Proposed: pre-select a rider at creation, and contact the rider only after the payment webhook confirms.
- Reservation TTL: stock is reserved before payment, so the planned timer order does not cover the payment window or a late webhook. Proposed: two-stage TTL and automatic void or refund. See EDD section 5.
- Offline handover: the rider doc requires it, but an offline app cannot check the OTP. Proposed: a salted hash per order, re-checked on sync. See EDD 9.4.
- No endpoints for: push token registration, menu list, customer profile, masked call, rough scooter position, longer-ETA consent, "notify me", payment retry, gate change before acceptance, Razorpay webhook.
