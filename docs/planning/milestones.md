# Milestones: ninzo-backend

FIXED scope, agreed with the owner on 2026-11-04. Changing it requires a decision record. Live progress is in `project_status.md`; work items are GitHub Issues.

Versioning: `vMAJOR.MINOR.PATCH`. `v1.0.0` = MVP: a basic, working core of all P1 features.

**One combined line of milestones for the whole backend.** Customer and rider share one order flow and the same modules, so they are not split. To see one app's work, filter Issues by label: `app:customer`, `app:rider`, `app:partner`, `app:console`, `app:shared`.

Each version is a GitHub Milestone, each phase is an Issue, and each sub-phase is a checklist item in that Issue. Sub-phases are written just before a version starts, because later ones will change as we learn.

## Customer and rider

| Version | Goal | Phases | Exit criteria | Status |
|---|---|---|---|---|
| v0.1.0 | App developers can build against mocks | Shared API rules; customer contract; rider contract; mock servers; contract review | Mocks answer every customer and rider endpoint and scenario, at a stable URL. Contracts reviewed with the app developers | planned |
| v0.2.0 | Foundations | Service skeleton; database and migrations; idempotency, audit, outbox; customer and rider sign-in; staging deploy | `task verify` green in CI. Migrations apply. Sign-in works on staging | planned |
| v0.3.0 | Real places and stock | Gates, zones, polygons; settings store; menu; lots and stock ledger; availability; rider profile, shift, node, stock scan | A test customer sees real availability. A test rider starts a shift and scans stock | on staging, 2026-11-09 |
| v0.4.0 | An order holds stock | Orders; reservations; a test payment, refused in production; cancel and refund rules | Concurrency tests pass. Razorpay comes before v1.0.0 (owner, 2026-11-06) | on staging, 2026-11-09 |
| v0.5.0 | One full order works | Location and GPS; offers, accept, Ready; zone timer; live streams; handover OTP | One order runs from payment to handover, with GPS starting and stopping the timer | on staging, 2026-11-09 |
| v0.6.0 | Failures handled | Remedies; no-scooter options; rider problems; offline sync; notifications; history and help | Every failure test in the customer EDD passes | on staging, 2026-11-10 |
| v0.7.0 | Customer and rider APIs complete | Restock suggestions and requests; relocation; earnings | All customer and rider P1 features meet "Done when". Hub actions come from a simulator | on staging (#120) |

**Before `v0.3.0`:** location, availability and matching work waits for the answers in `v0.3.0_open_questions.md` (#34).

## Partner

| Version | Goal | Phases | Exit criteria | Status |
|---|---|---|---|---|
| v0.8.0 | Hub APIs | Contract and mock; staff sign-in; queue, accept, pick, scan; shortfall, quarantine, ready, labels; offline sync; connect to rider restock | A full restock runs through real hub APIs, with no simulator | planned |

## Admin and ops console

| Version | Goal | Phases | Exit criteria | Status |
|---|---|---|---|---|
| v0.9.0 | Admin APIs | Contract and mock; staff sign-in with 2FA, roles, approvals; users, zones, kill switch, gates, menu; rider review, flags, audit log | Role matrix tests pass. Timer length and radius change through the API | planned |
| v0.10.0 | Ops and support APIs | Hub ops; order timeline, refunds, replacements, incidents; live map and alerts | All console P1 endpoints meet "Done when" | planned |

## Pilot

| Version | Goal | Phases | Exit criteria | Status |
|---|---|---|---|---|
| v1.0.0 | Pilot ready | Razorpay, real SMS and Apple sign-in, once credentials arrive; load and failure tests; security scan; monitoring; live keys and go-live steps | All P1 features shipped per Definition of Done | planned |

Status values: planned · in progress · released.

## Parts for v0.7.0 to v0.10.0
Owner, 2026-11-10 (decision 0014). Each part is one Issue and one PR.

**v0.7.0, customer and rider APIs complete.** The rider contract already has these endpoints.

| Part | What |
|---|---|
| A | Hubs and restock data: hubs per zone, restock requests and lines, shortfalls. A hub simulator stands in for hub staff |
| B | Restock suggestions when a scooter runs low; the rider confirms, edits or declines. The low-stock level is a setting |
| C | Manual restock requests, options, substitute answers and status; queued restock events in the offline batch |
| D | Relocation: suggested moves toward demand, accepted with no penalty for refusing. A simple rule first |
| E | Earnings: orders, incentives and paid relocation time per day. Rates are settings, with test values until the console |

**v0.8.0, partner (hub) APIs.** A: contract and mock, shared through the Doc first. B: hub staff sign-in by phone and OTP. C: the restock queue: accept, pick, scan. D: shortfall with a substitute, quarantine, ready, labels. E: hub offline sync. F: the real hub flow replaces the simulator.

**v0.9.0, admin console APIs.** A: contract and mock. B: staff sign-in with an ID, password and authenticator code; roles admin, ops, support and hub manager; approvals for a refund above a set amount and for changes to settings. C: users, rider review and flags. D: zones, every setting and the kill switch. E: menu, prices, gates, hubs and earning rates entered by the admin, replacing test data. F: the audit log, arrival reviews and the queued support events.

**v0.10.0, ops and support console APIs.** A: the order timeline. B: support actions: tickets, refunds, replacements, credits, arrival reviews. C: incidents in one queue. D: hub operations across hubs. E: the live map and alerts.

## What this order means
- Until v0.9.0, timer length, radius, gates and menu are seed data, with no screen or API to change them.
- Until v0.8.0, rider stock and restock run on seeded lots, with a simulator for hub actions.
- Until the apps exist, full orders are tested with a rider simulator and API tests.
- Flutter apps are built by other developers. This repo holds only the APIs.
