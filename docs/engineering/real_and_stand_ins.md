# What is real, and what stands in for it

The owner's plan (2026-11-09): build every API and the whole pipeline first, with simple stand-ins where something is only an estimate or needs an outside account. Then make each part real. **Money, stock, order states and sign-in rules are real from the start**, because they are hard to fix later.

Update this page in the same PR as any change to a row. The shared Doc points app developers here.

**Status:** Real means production behaviour. Stand-in means it works end to end, with a simple version behind one swappable part. Mock only means only the mock server has it. Not built means nothing answers yet.

## On the real backend (staging)
| Part | Status | What it is today | What makes it real | When |
|---|---|---|---|---|
| Phone sign-in, sessions, refresh, sign-out | Real | As in production | | Done |
| SMS for codes | Stand-in | Test code `123456`, no SMS sent. Production refuses to start in test mode | MSG91, after DLT approval | When credentials arrive |
| Google sign-in | Real | Checked against Google's keys. Tested with a real login on the web, not yet from the Android app | The Android app test on staging | Developer testing now |
| Apple sign-in | Not built | | Apple Developer account and a Mac | Before v1.0.0 |
| One sign-in method, switching, settings change | Real | | | Done |
| Rate limits | Real, in memory | Counted inside each API | Redis, before two replicas of an API | When scaling |
| Profile and devices | Real | Push tokens are stored | Push sending, which needs the worker and FCM | v0.6.0 |
| Settings store | Real | Changed by `task settings` | The admin console | v0.9.0 |
| Zones and service areas | Real, test data | Two rough test boxes in Koramangala and Indiranagar | The owner's real areas | When the owner gives them |
| Gates, location check | Real | Verified by the area they are in (decision 0012) | | Done |
| Menu | Real, test data | Three test items | The owner's real menu, then the console | When the owner gives it |
| Stock, lots, ledger | Real, test data | Lots are made by `task testdata` | Hub APIs make lots | v0.8.0 |
| Rider approval | Stand-in | `task riders`, audited, documents marked checked by hand | Rider application, DigiLocker and the console | v0.3.0 application later, console v0.9.0 |
| Rider shift, nodes, stock scan | Real | A free scooter at the node is the rider's for the shift | | Done |
| Scooter battery (`battery_pct` on the rider's shift) | Stand-in | Always null. There is no scooter data yet | Scooter telemetry, when the pilot scooters report it | When the scooters report |
| Restock `ready_in_s` | Stand-in | Counts down to the hub's queue slot, which the hub simulator sets. Null without a slot, 0 once the slot has passed | The real hub's packing time, from the hub app (v0.8.0) | With the hub app |
| **Reach in availability** | Stand-in | Straight-line distance from the scooter's staging node, at a placeholder speed | Rider GPS, then real road times from a routing engine | GPS v0.5.0, roads after the road test |
| H3 map grid | Not built | Not needed at pilot size, where PostGIS is exact | H3 cells, if demand data needs them | After real traffic |
| Matching riders to orders | Not built | | Nearest rider on shift with the whole basket, then ranking | v0.5.0 |
| Orders and holds on one scooter | Real | Built (#78), on staging (#82) | | v0.4.0 |
| Cancel window, retry, gate change and refund rules | Real | Built (#80), on staging (#82). Refunds of the test payment are marked processed at once, as no money moved | Razorpay refunds stay pending until Razorpay says processed | v0.4.0 |
| Background passes: holds end after 10 minutes, seq moves when cancellable changes | Real, in a simple form | A loop every 5 seconds inside the customer API, safe with several copies | A separate worker with scheduled tasks, when load needs it | v0.4.0 |
| Offers: one rider at a time, accept, decline, expiry, Ready, the packing limit, no scooter | Real | Built (#84), on staging (#92). The order's own scooter goes first, then the nearest free one holding the whole basket. One order per rider | | v0.5.0 |
| GPS pings, the zone timer, arrival, the missed timer, `rider_moving` | Real | Built (#86), on staging (#92). Straight-line distance to the gate, two pings in a row, jumps over 40 m/s and pings over 30 s late dropped | Road distance and location polygons later; the in-zone shorter wait is not used yet | v0.5.0 |
| Live streams for the customer's order and the rider | Real, in a simple form | Built (#88, #90), on staging (#92). Each stream reads the saved `seq` about once a second, sends a heartbeat every 15 seconds and the rider's position every 3 seconds | A notification channel instead of polling, when there are many streams | v0.5.0 |
| Remedies: a credit for a missed timer, a refund for missing food, and late-arrival review | Real | Built (#94), on staging (#112). The credit is owed with no amount until the console sets `remedy_credit_paise`. Reviews time out after 10 minutes; nobody decides them until the console | Applying credits to a later order, in the console era | v0.6.0 |
| A longer ETA at no scooter | Real to accept, nothing offers one yet | Built (#94), on staging (#112). The contract offers a longer ETA only when the time is measured, and times are straight lines for now | Offers made once road times exist | v0.6.0 |
| Rider problems and reassignment after no ping | Real | Built (#96), on staging (#112). Breakdown or accident: the scooter leaves service; a hidden order rematches, a shown one fails with a refund. When on-time pings stop for 2 minutes before the timer, the order is reassigned; after it, support is alerted. Reports are stored; nobody reads them until the console | Support tools in the console | v0.6.0 |
| The rider's offline event batch | Real | Built (#98), on staging (#112). Location, handover, problem and stock scan events run the same rules as the direct requests; queued restock confirms and declines apply (#106). A rejected handover queues a support alert (#101) | | v0.6.0 |
| Support tickets and notify me | Real | Built (#101), on staging (#112). Tickets and support alerts are stored; their outbox events wait for the console, as the push worker takes only push topics | Support tools in the console | v0.6.0 |
| **Push notifications** | Stand-in | Built (#101), on staging (#112): one push per customer stage change and per new offer, queued in the outbox and handed to a pusher that only logs | FCM, with the owner's Firebase project | v0.6.0 |
| Handover with the customer's code, partial quantities, stock taken off for good, signal-lost code release | Real | Built (#86). Audit flags are stored on the order; nobody reviews them until the console | Support alerts and remedies in v0.6.0 | v0.5.0 |
| **Payment** | Stand-in | Test payment, marked paid or failed by `POST /test/payments/{order_id}` on staging. Production refuses to start with it | Razorpay and its webhook | Before v1.0.0 |
| Refund money | Being built as a stand-in | The refund is recorded, and no money moves | Razorpay refunds | Before v1.0.0 |
| Background jobs | Being built | Run inside the customer API, one replica | A separate worker | With two replicas, or after the pilot |
| Live streams, timer, handover, offers | Not built | Mock only | | v0.5.0 |
| Failures, remedies, notifications, support tickets | Not built | Mock only | | v0.6.0 |
| Partner and hub APIs | Not built | | | v0.8.0 |
| Admin, ops and support APIs | Not built | | | v0.9.0 to v0.10.0 |

## On the mock server
Every customer and rider endpoint in the contracts answers on the mock, with scenarios. Mock replies are samples: they do not check tokens, stock or rules.

## Related
- Accepted downsides: `known_downsides.md`
- Version plan: `docs/planning/milestones.md`
| Hubs, earning rates, menu, prices, zones and gates | Test data | Entered by the admin in the console from v0.9.0 (decision 0014) | The console | v0.9.0 |
| Restock suggestions: the stock check, confirm with edits, decline, expiry | Real | Built (#106). Low level, target, lifetime and check interval are settings | | v0.7.0 |
| Manual restock, shortfall answers, the cassette pickup and expiry of uncollected cassettes | Real | Built (#107). A substitute shortfall is answered, but only real hub staff (v0.8.0) offer one | | v0.7.0 |
| Relocation: suggested moves toward demand, accept, arrival from pings | Real, simple rule | Built (#108). Demand is recent paid orders counted at the node nearest each gate; distance is straight-line. The demand forecast and road times come in v1.0.0 | | v0.7.0 |
| Earnings: pay per order, the day's incentive, paid moving time; `getEarnings` | Real; rates are test values | Built (#109). Test rates (40 rupees an order, 200 at 10 orders, 2 rupees a minute up to 20 minutes) until the admin sets them in the console (v0.9.0). No payouts | | v0.7.0 |
| Partner (hub) API | Mock only | Contract and mock (#122). The real hub APIs come in parts B to F | | v0.8.0 |
| Hub staff sign-in and `GET /hub/me` | Real | Built (#123): phone and OTP, sessions as for riders; test staff +919800000091 (hub lead) and +919800000092 (hub staff) at the first test hub. Staff are added by the admin from v0.9.0 | | v0.8.0 |
| The hub queue, accept, pick list, scan, unscan and the hub live channel | Real | Built (#124). The slot and rider arrival are estimates (5 and 10 minutes) until a hub sets its own. At a hub with active staff, the simulator stands aside | | v0.8.0 |
| Shortfall lines, sealing the cassette, the label, the hub's lots, quarantine (hub lead), and substitutes | Real | Built (#125). A substitute accepted by the rider reopens packing with the substitute in place of what was short | | v0.8.0 |
| Hub offline sync (`POST /hub/events`) | Real | Built (#126): queued accept, scan, unscan, shortfall, ready and quarantine, applied in order, each once by event_id | | v0.8.0 |
| Hub staff | Real | Built (#127): hub actions come only from staff through the partner API. The simulator runs in tests only. A suggestion prefers a hub with staff; a confirmed request no hub accepts within `restock_accept_s` (15 minutes) expires and the rider is told. Only the first test zone's hub has test staff; the admin adds staff from v0.9.0 | | v0.8.0 |
| Console API | Mock only | Contract and mock (#133). The real console APIs come in parts B to F | | v0.9.0 |
| Console sign-in (password, then TOTP), console users and roles, permissions on every call, approvals, feature flags | Real | Built (#134). The first admin is made with `cmd/console`; settings, kill switch and refund approvals join in parts D and v0.10.0 | | v0.9.0 |
| Rider review in the console: the queue, approve, suspend, reinstate | Real | Built (#135). Replaces `cmd/riders` for daily use; suspending ends the rider's sessions and shift | | v0.9.0 |
| Console zones, settings and the kill switch, each change through an approval | Real | Built (#136). Settings changes go through `geo.ChangeSettings`, so every zone stays valid; the kill switch pauses or resumes the zone | | v0.9.0 |
| Console menu and price history, gate edits with versions, hubs and hub staff | Real | Built (#137). A hub_manager sees and changes only their own hub's staff; migration 0036 records who made each gate version | | v0.9.0 |
| Console audit log, arrival reviews and support alerts | Real | Built (#138). Secrets and full phone numbers never leave the audit log; a lost signal now raises a support alert; migration 0037 records who settled a review | | v0.9.0 |
| Masked calls (customer to rider, rider to customer) | Stand-in | Every call answers 409 `call_not_available` (#151). Needs a telephony provider (`TELEPHONY_*`) | | v1.0.0 |
| Rider document verification (getVerification, startVerification) | Stand-in | Start records a pending request; ops check the documents by hand and mark them with `cmd/riders` (#156) | DigiLocker | v1.0.0 |
| The rider's route to the gate (getRoute) | Stand-in | A straight line from the last position to the gate, eta_source placeholder (#156) | OSRM road routes | v1.0.0 |
