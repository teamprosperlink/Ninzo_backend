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
| #6 | Customer contract | `v0.1.0` | [reports/6.md](../testing/reports/6.md) |
| #7 | Rider contract | `v0.1.0` | [reports/7.md](../testing/reports/7.md) |
| #15 | Cancel and refund rule | `v0.1.0` | [reports/15.md](../testing/reports/15.md) |
| #8 | Mock server | `v0.1.0` | [reports/8.md](../testing/reports/8.md) |

## Blocked
| Issue | Blocker | Since |
|---|---|---|

## Next actions
0. Owner, before `v0.3.0`: answer `docs/planning/v0.3.0_open_questions.md` (#34). `docs/context/algorithms/` conflicts with the flow in force, which stays until then.
1. `v0.2.0`: all 8 migrations are live on Supabase (#30). The sign-in contract and mock are live, and the developers are told in the shared Doc. The service skeleton is built (#32). Idempotency, audit and outbox are built (#36). The session system is built (#38). Phone OTP is built, with test codes (#43). Google sign-in and phone verification are built (#47). Each customer has one sign-in method, with no merging (#54, migration 0010 is not on Supabase). Rate limiting is built (#39). Customer and rider profile and devices are built (#58, migration 0011 is not on Supabase). Staging is live with test data only (#60): customer https://customer-api-production-3ba0.up.railway.app/v1 and rider https://rider-api-production-cf92.up.railway.app/v1, in Singapore. Next: the app developers test on staging. The `v0.3.0` questions are answered (decision 0011), except a second order for a busy rider. The pilot city is Bengaluru. `v0.3.0` step 1, the settings store, is built (#64). Step 2, zones, gates and the location check, is built (#66). Step 3, the menu, is built (#68). Steps 4 and 5, stock and the rider shift, are built (#70). Step 6, availability, is built (#72). **v0.3.0 is live on staging (#74):** all 16 migrations are on Supabase, with test zones, menu, fleet and a test rider. `v0.4.0` part A, orders, holds and the test payment, is built (#78, migration 0017 is not on Supabase). Part B, cancels, retries, gate changes, refund status and the background passes, is built (#80). `v0.4.0` is on staging (#82, 2026-11-09): migrations 0017 and 0018 applied, and the order flow passed there. `v0.5.0` part 1, offers, is built (#84, migration 0019 is not on Supabase). Part 2, GPS pings, the zone timer and handover, is built (#86, migration 0020 is not on Supabase). Part 3, the live streams, is built (#88), and streams stay open past the request limit (#90). `v0.5.0` is on staging (#92, 2026-11-09): migrations 0019 and 0020 applied, and one full order passed there from payment to handover with both streams live. `v0.6.0` part A, remedies, late-arrival review and the longer ETA, is built (#94, migration 0021 is not on Supabase). Part B, rider problems and reassignment after no ping, is built (#96, migration 0022 is not on Supabase). Part C, the rider's offline event batch, is built (#98, migration 0023 is not on Supabase). Part D, support tickets, notify me and notifications, is built (#101, migration 0024 is not on Supabase). Push is a stand-in that only logs until FCM. `v0.6.0` is on staging (#112, 2026-11-10): migrations 0021 to 0024 applied, and the failure cases passed there. `v0.7.0` part A, hubs, restock data and the hub simulator, is built (#105, migration 0025 is not on Supabase). Part B, restock suggestions, is built (#106, migration 0026 is not on Supabase). Part C, manual requests, shortfall answers and the pickup, is built (#107, migration 0027 is not on Supabase). Part D, relocation, is built (#108, migration 0028 is not on Supabase). Part E, earnings, is built (#109, migration 0029 is not on Supabase). `v0.7.0` is on staging (#120, 2026-11-10): migrations 0025 to 0029 applied, test hubs added, and restock, the pickup and pay passed there. `v0.8.0` part A, the partner contract and mock, is built (#122). It is shared in the Doc. Part B, hub staff sign-in and the partner API service, is built (#123, migration 0030 is not on Supabase). Part C, the queue, accept, pick list, scan and the hub live channel, is built (#124, migration 0031 is not on Supabase). Part D, shortfall lines, sealing, labels, lots, quarantine and substitutes, is built (#125, migration 0032 is not on Supabase). Part E, hub offline sync, is built (#126, migration 0033 is not on Supabase). Part F, the real hub flow replacing the simulator, is built (#127). `v0.8.0` is complete in code; the owner runs migrations 0030 to 0034 and deploys the partner API (its secrets). Next is `v0.9.0`, the admin console. `v0.9.0` part A, the console contract and mock, is built (#133, decision 0015). Part B, console sign-in, users, permissions, approvals and flags, is built (#134, migration 0035 is not on Supabase). Part C, rider review, is built (#135). Part D, zones, settings and the kill switch, is built (#136). Part E, menu and prices, gates, hubs and hub staff, is built (#137, migration 0036 is not on Supabase). Part F, the audit log, arrival reviews and support alerts, is built (#138, migration 0037 is not on Supabase). `v0.9.0` is complete in code. Next is `v0.10.0`, ops and support.

**Owner, 2026-11-09:** the `ninzo_service` password is rotated, and staging runs on it. MSG91 and DLT are in progress on the owner's side and block nothing. The real menu and service areas will be entered through the partner app and the console, so test data stays until then. What is real and what is a stand-in: `docs/engineering/real_and_stand_ins.md`. `ninzo_service` has no password yet: the owner sets one when the first service connects to Supabase.
2. #9 stays open for developer feedback. The owner shares the shared index with them.
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
- Starting values, proposed by engineering and accepted as defaults when the owner said to start on 2026-11-04: timer starts when packed and inside the zone (packed means Ready pressed or packing limit passed); 5 minute maximum wait before the timer; credit if the timer ends before 100 m.
- Every task starts with a GitHub Issue. See `CLAUDE.md`.
- GPS alone stops the timer. OTP is the proof of delivery. The required QR or code scan on arrival, and `POST /v1/orders/{id}/arrive`, are dropped.
- Claude merges PRs once checks pass, without owner sign-off on each PR.
- Milestones are one combined line, with `app:*` labels to filter by app.
- Other developers build all Flutter apps, including the partner app. Claude builds only the hub APIs for it.
- The Google Doc [Ninzo Backend: Shared Index](https://docs.google.com/document/d/1p5sGHeYSo4cCCLlAErqLLqEMnOPkZvk2-gEA4fKgO6M/edit) is the orchestrator for communicating with the app developers, and the index of all shareables. GitHub stays the source of truth.
- Supabase: Ninzo gets its own new project in Mumbai when `v0.2.0` starts, and everything is built around it. The existing project, `teamprosperlink's Project`, belongs to a different app and must never be touched.
- No gate or address change after payment: owner decision, 2026-11-04. It overrides the edge-case doc.
- Cancel: full refund within 5 seconds of payment being confirmed, and after that no cancel and no refund. Failures on Ninzo's side are always refunded, with the reason and method told to the customer. The customer can leave with a refund once the maximum wait passes. Owner decision, 2026-11-04.
- Tables are built version by version (owner, 2026-11-05).
- GPS pings are kept in full through the testing period, and retention is set after it (owner, 2026-11-05).
- Supabase has PostGIS 3.3.7, and local and CI have 3.5, so check PostGIS functions against 3.3. A separate service role is still to be decided before the first service connects (see `docs/testing/reports/22.md`).
- Sign-in, roles and sessions are decided by the owner: see `docs/engineering/authentication_rules.md`, and Issue #25.
- Ninzo's Supabase project is `Ninzo-backend-mumbai`, ref `chyejfgsipcsdxnginei`, region `ap-south-1`, free plan. The connection details go in the root `.env` (owner, 2026-11-05).
- `v0.2.0` starts as soon as the mock server is published, without waiting for developer feedback.
- Mock server is live at https://mock-server-production-6a71.up.railway.app (Railway trial, $5 credit, up to 30 days). The owner must decide on a paid plan before it ends. Production hosting is decided separately.
- Mock server: a small, cheap host now. Railway proposed. Production hosting is decided later.

## Open questions for the owner
Proposed defaults are not yet agreed.
1. Backend and Redis hosting. Proposed: AWS Mumbai. Does not block docs.
2. Decided by the owner on 2026-11-04: backend only for Claude, and other developers build the Flutter apps. Still open: who builds the `console-web` frontend?
3. Gates: any customer pin, or only verified gates? Proposed: ops-verified list, because sweet spots are computed offline and nothing sets `verified`.
4. Decided by the owner on 2026-11-04: see "Order and timer flow" above.
5. Decided by the owner on 2026-11-04: charge up front and refund automatically, with no holds.
6. Pilot city (sets map data). Do scooters have temperature sensors? Proposed: scan-only for the pilot.
7. Decided by the owner on 2026-11-04: see "Order and timer flow" above.
8. Official time for a location sent late from a dead zone. Proposed: server receipt time, with late events inside the promise sent to audit review. See EDD 9.3.

## Known gaps in the customer KT doc
To resolve in the customer docs and contract.
- Handover OTP: customer and rider apps both submit it, and no response gives the customer an OTP to show. Proposed: server issues it, customer app displays it, rider submits it.
- Payment wording: the feature table says "nobody is charged", while rule 7 forbids that wording.
- Payment and dispatch order: the architecture doc sets `MATCHING` at creation, before payment. Proposed: pre-select a rider at creation, and contact the rider only after the payment webhook confirms.
- Reservation TTL: stock is reserved before payment, so the planned timer order does not cover the payment window or a late webhook. Proposed: two-stage TTL and automatic refund. See EDD section 5.
- Offline handover: the rider doc requires it, but an offline app cannot check the OTP. Proposed: a salted hash per order, re-checked on sync. See EDD 9.4.
- No endpoints for: push token registration, menu list, customer profile, masked call, rough scooter position, longer-ETA consent, "notify me", payment retry, gate change before acceptance, Razorpay webhook.

The plan for `v0.7.0` to `v0.10.0` is agreed (decision 0014, #103).
