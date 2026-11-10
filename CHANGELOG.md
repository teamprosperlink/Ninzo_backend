# Changelog

Format: newest first. One section per version; inside it, one entry per feature or module,
each with its Issue number. Entries are added in the same PR that ships the feature.

## [Unreleased]

### Console contract and mock (#133)
- Added: `contracts/console/openapi.yaml`, 40 operations: two-step sign-in (ID and password, then TOTP), users and roles, approvals, rider review, flags, zones, settings and the kill switch, menu and prices, gates and their history, hubs and hub staff, the audit log, arrival reviews and support alerts.
- Added: the mock serves it at `/console/v1`; CI checks the mock against it. Decision 0015.
- Evidence: docs/testing/reports/133.md

### Hub offline sync (#126)
- Added: `POST /hub/events` applies queued accept, scan, unscan, shortfall, ready and quarantine in order, each once by event_id, with the same rules as their endpoints. One rejected event never blocks the rest; a direct call and its queued copy count once.
- Added: migration 0033, `hub_events`. Not applied to Supabase.
- Evidence: docs/testing/reports/126.md

### Shortfall, sealing and quarantine (#125)
- Added: `recordShortfall` (every short line; partial, substitute or another hub in the zone), `markHubRequestReady` (the lots scanned; partial with a shortfall, even empty; again changes nothing), `getCassetteLabel`, `listHubLots` and `quarantineLot` (hub lead only; the lot leaves any unsealed cassette).
- Changed: a rider accepting a substitute reopens packing with the substitute in place of what was short; the simulator records its short lines.
- Added: migration 0032, `shortfall_lines` (with the simulator's past shortfalls filled in), the shortfall's reason and who recorded it, and who quarantined a lot and why. Not applied to Supabase.
- Evidence: docs/testing/reports/125.md

### The hub queue (#124)
- Added: `listHubRequests`, `getHubRequest`, `acceptHubRequest` (one acceptance only), `getPickList` (soonest expiry first), `scanHubLot`, `unscanHubLot` and the hub live channel `/v1/hub/stream`.
- Added: migration 0031, `hub_state`, and `seq` and `accepted_by` on requests. Every change the rider sees also raises the hub's seq.
- Changed: the hub simulator stands aside at a hub with active staff.
- Evidence: docs/testing/reports/124.md

### Hub staff sign-in (#123)
- Added: `apps/partner-api`, the partner (hub) API. Hub staff and hub leads sign in with phone and OTP (decision 0014); `GET /hub/me` returns the staff member, their role and hub, and whether the hub is open.
- Added: migration 0030, `hub_staff`, and the staff type on codes, sessions and devices. Not applied to Supabase.
- Added: test staff in `cmd/testdata staff`.
- Evidence: docs/testing/reports/123.md

### Partner contract and mock (#122)
- Added: `contracts/partner/openapi.yaml` and the hub live channel, for the partner (hub) app: staff sign-in by phone and OTP, the queue, accept, pick list, scan, unscan, shortfall, ready, label, lots, quarantine (hub lead only) and the offline event batch.
- Added: the mock serves them at `/partner/v1` with 27 scenarios; CI checks the mock against the contract.
- Evidence: docs/testing/reports/122.md

### Earnings (#109)
- Added: a handover records the order's pay; the day's incentive comes once the rider's handed-over orders reach the level; an accepted move is paid by the started minute until it ends, up to a cap. `getEarnings` returns a local day.
- Added: migration 0029, `earnings`, and test rates as settings: `pay_per_order_paise` (4000), `incentive_orders` (10), `incentive_paise` (20000), `relocation_paise_per_minute` (200), `relocation_max_paid_s` (1200). Not applied to Supabase.
- Evidence: docs/testing/reports/109.md

### Relocation (#108)
- Added: a check every 30 seconds that asks a spare rider (from a node with two or more waiting) to move to the nearest node with recent orders and nobody waiting. `listNodes` shows the suggestion; `acceptRelocation` moves the rider's node and scooter and starts paid time; a ping near the node marks arrival. Ignoring or refusing has no penalty.
- Added: migration 0028, `relocations` and five settings: `relocation_window_s` (1800), `relocation_min_orders` (2), `relocation_ttl_s` (300), `relocation_rest_s` (900), `relocation_arrive_m` (100). Not applied to Supabase.
- Evidence: docs/testing/reports/108.md

### Manual restock and the pickup (#107)
- Added: `createRestock` (merges into a confirmed request at the same hub, replaces a suggestion) and `respondToShortfall` (a partial fill or another hub, which opens a follow-up request there).
- Added: scanning cassette lots moves a request to `picked_up`, then `received`, and settles an open shortfall. A cassette nobody collects 30 minutes after `arrive_by` expires and its lots go back on the shelf.
- Added: migration 0027, `restock_requests.follow_of`. Not applied to Supabase.
- Evidence: docs/testing/reports/107.md

### Restock suggestions (#106)
- Added: a stock check that drafts one suggestion when a scooter runs low, from the nearest hub; `listRestockSuggestions`, `confirmRestock` (with changed quantities) and `declineRestock`; suggestions expire.
- Added: settings `restock_low_qty` (3), `restock_target_qty` (10), `restock_suggestion_ttl_s` (600) and `restock_check_s` (300). Migration 0026, not applied to Supabase.
- Changed: queued `restock_confirm` and `restock_decline` events apply in the offline batch.
- Evidence: docs/testing/reports/106.md

### Hubs and restock data (#105)
- Added: hubs, lots waiting at a hub, restock requests, their lines and packed lots, and shortfalls. Migration 0025, not applied to Supabase.
- Added: `getRestockOptions`, `getRestock`, and the rider's `restock_request` in `getRiderState`.
- Added: a hub simulator, standing in for hub staff until v0.8.0, and a test hub with lots in each test zone.
- Evidence: docs/testing/reports/105.md

### Support and notifications (#101)
- Added: `createTicket`, `getTicket` and `notifyWhenAvailable`.
- Added: a push per customer stage change, following `customer_stage` only, and per new offer for the rider; a support alert for a rejected queued handover. Pushes go through the outbox to a pusher that only logs until FCM is connected.
- Migration 0024, not applied to Supabase.
- Evidence: docs/testing/reports/101.md

### The offline event batch (#98)
- Added: `sendEventBatch`. Queued location, handover, problem and stock scan events apply in order, each on its own, with the same rules as the direct requests. A repeated `event_id` returns its first final result; a direct request sent first with the same key counts once.
- Added: `rider_events`, migration 0023, not applied to Supabase.
- Evidence: docs/testing/reports/98.md

### Rider problems and reassignment (#96)
- Added: `reportProblem`. A breakdown or accident takes the scooter out of service; an order the customer has not seen accepted goes to another rider silently, and one they have seen fails with a full refund (`order_failed`). Other problems are recorded for support.
- Added: when a rider's on-time pings stop for `reassign_after_s` (2 minutes) before the timer, the order is reassigned, unless the customer already has the code; once the timer runs, support is alerted instead.
- Changed: a scooter in maintenance gets no offers.
- Migration 0022, not applied to Supabase.
- Evidence: docs/testing/reports/96.md

### Remedies and the longer ETA (#94)
- Added: a credit for a missed timer, owed once the rider's pings show the miss, or 10 minutes after the timer if the signal stays lost. A late ping showing the rider at the gate in time opens a review, which holds the credit for up to 10 minutes.
- Added: a partial handover refunds the missing food (`missing_food`) and shows it as a remedy.
- Added: `acceptLongerEta`, and `eta_offer` on the order at `no_scooter`.
- Changed: `payment_status` is `refunded` only once the whole payment went back.
- Changed: the rider stream sends `order_state` when the order changes, such as Ready, arrival and handover.
- Migration 0021, not applied to Supabase.
- Evidence: docs/testing/reports/94.md

### Fixed (#90)
- Live streams no longer drop after about 20 seconds: a WebSocket upgrade skips the request limit, and the stream clears the server's read and write deadlines.

### Live streams (#88)
- Added: the customer's order stream and the rider's stream, as in the stream contracts: sign-in by frame, `auth_ok`, heartbeats with `seq`, `order_update`, `rider_position`, `offer` and `order_state`, and the close codes 4401, 4403, 4404 and 4410.
- Evidence: docs/testing/reports/88.md

### The zone timer and handover (#86)
- Added: `locationPing`, kept as evidence. Two pings in a row inside the zone start the timer once the order is packed and the minimum wait has passed; two inside the arrival perimeter stop it. A packed rider already inside the perimeter gets no timer.
- Added: `handOver` with the customer's code and quantities. Stock is taken off the scooter for good.
- Added: Ready is refused with `rider_moving` when a fresh ping shows movement. The timer is marked missed when its length passes. The code is released to the customer when the signal is lost after the timer or the wait deadline.
- Migration 0020, not applied to Supabase.
- Evidence: docs/testing/reports/86.md

### Offers (#84)
- Added: offers to one rider at a time once the cancel window closes: `getRiderState`, `listOffers`, `acceptOffer`, `declineOffer`, `markReady` and `getRiderOrder`.
- Added: the rider pass that expires offers, moves an order to the next free scooter, packs at the packing limit, and refunds an order left at no scooter.
- Added: the customer sees `rider_accepted` and the rider's first name once the minimum wait has passed.
- Added: the `offer_timeout_s` setting, 30 seconds. Migration 0019, not applied to Supabase.
- Changed: ending a shift gives back an open offer, and is refused with `order_in_progress` while an order is accepted.
- Evidence: docs/testing/reports/84.md

### Orders, part B (#80)
- Added: `cancelOrder`, `retryPayment`, `changeOrderGate` and `getOrderRefund`, following the cancel and refund table in contracts/README.md.
- Added: a background pass inside the customer API that ends unpaid orders after their hold and raises `seq` when `cancellable` changes. Migration 0018, not applied to Supabase.
- Evidence: docs/testing/reports/80.md

### Orders, part A (#78)
- Added: orders, order lines with their price, an append-only order history, holds on one scooter, payments and refunds. Migration 0017, not applied to Supabase.
- Added: `createOrder`, `getOrder`, `listOrders`, and `confirmTestPayment` on staging, standing in for Razorpay.
- Added: the `payment_hold_s` setting, 10 minutes.
- Evidence: docs/testing/reports/78.md

### Plan for v0.4.0 (#34)
- Added: decision 0013, and `real_and_stand_ins.md`, which lists what is real, a stand-in or mock only.
- Closes #34: every question before v0.3.0 is answered, in decisions 0011 to 0013.

### v0.3.0 on staging (#74)
- Migrations 0012 to 0016 are on Supabase, with test data. Both APIs are deployed, and the flow passed on the real URLs.
- Evidence: docs/testing/reports/74.md

### Test rider and the version plan (#74)
- Added: a test rider through `task testdata`, for trying the rider app on staging.
- Changed: `milestones.md` follows the owner's decision that payment comes later.

### Availability (#72)
- Added: `getAvailability` for a saved gate or a pin, and `availability_rules` for selling hours. Migration 0016, not applied to Supabase.
- Evidence: docs/testing/reports/72.md

### Stock and the rider shift (#70)
- Added: staging nodes, scooters, lots, `scooter_stock`, an append-only `stock_ledger`, shifts and `rider_state`. Migration 0015, not applied to Supabase.
- Added: `startShift`, `endShift`, `listNodes`, `scanStock` and `getStock`. `getRider` now returns `zone_id`.
- Added: `task riders` to approve a rider until the console, and a test fleet through `task testdata`.
- Evidence: docs/testing/reports/70.md

### Menu (#68)
- Added: `menu_items` and `price_history`, `listMenuItems` and `getMenuItem`, and three test items. Migration 0014, not applied to Supabase.
- Evidence: docs/testing/reports/68.md

### Zones, gates and the location check (#66)
- Added: service areas on `zones`, `listGates`, `createGate`, and `checkLocation`, which saves nothing. Migration 0013, not applied to Supabase.
- Added: `task testdata` and `task staging:testdata`, for the Bengaluru test zones.
- Evidence: docs/testing/reports/66.md

### Settings store (#64)
- Added: `zones` and versioned `zone_settings`, seeded with the starting values. Migration 0012, not applied to Supabase.
- Added: `task settings` and `task staging:settings`, audited, until the console exists.
- Evidence: docs/testing/reports/64.md

### Staging (#60)
- Added: the customer and rider APIs on Railway, in Singapore, with test data only. Migrations 0009 to 0011 are applied to Supabase.
- Evidence: docs/testing/reports/60.md

### A far-away database (#60)
- Changed: the database check allows 8 seconds, not 2, because a new connection from another region often took longer, which made starting slow and hid the database's real error. Each API keeps one connection open.

### Profile and devices (#58)
- Added: `getMe`, `updateMe`, `registerDevice`, `removeDevice`, `getRider`, `registerRiderDevice` and `removeRiderDevice`.
- Added: an optional customer email, for contact only. Migration 0011, not applied to Supabase.
- Changed: a change of sign-in method removes every device.
- Evidence: docs/testing/reports/58.md

### Rate limits (#39)
- Added: limits per network address, per phone and address, per refresh token and per session, counted in memory behind one interface. Decision 0010.
- Changed: 20 codes an hour per phone in all, and 5 per phone and address.
- Added: the `CLIENT_IP_HEADER` setting, required outside local.
- Evidence: docs/testing/reports/39.md

### One sign-in method per customer (#54)
- Changed: a customer has one sign-in method, and accounts never merge. `phone_already_linked` is now `phone_already_registered`, and `customer_id` no longer changes.
- Added: `sign_in_method_mismatch` and `switch_to_phone` on `verifyOtp`, and `changeSignInMethod`. Migration 0010, not applied to Supabase.
- Added: sign-in methods as components, with `SIGNIN_METHODS`, and `docs/engineering/known_downsides.md`.
- Evidence: docs/testing/reports/54.md

### When a Google customer verifies a phone (#52)
- Changed: a Google customer is asked for a phone when they go to order, not straight after sign-in. No API change.

### Branch and release setup (#50)
- Added: decision 0008, `main` plus `production`, on the free GitHub plan.

### Google sign-in, phone verification and merging (#47)
- Added: `signInWithGoogle`, `requestPhoneOtp` and `verifyPhone` on `customer-api`. Accounts merge only on a phone proven by OTP.
- Added: the `GOOGLE_CLIENT_IDS` setting, required by `customer-api`.
- Evidence: docs/testing/reports/47.md

### SMS setup steps (#45)
- Added: `docs/planning/sms_setup.md`.

### Phone OTP sign-in (#43)
- Added: `requestOtp` and `verifyOtp` on `customer-api` and `rider-api`, with test codes. Real SMS comes later.
- Evidence: docs/testing/reports/43.md

### Rider sign-in is phone and OTP only (#41)
- Removed: rider ID and rider password endpoints and fields from the rider contract and mock.

### Session system (#38)
- Added: `internal/identity` with access and refresh tokens, renewal, the lost-reply retry and ending a chain. `customer-api` and `rider-api` serve `refreshToken` and `signOut`.
- Added: `signOut` to both contracts and the mock.
- Added: migration 0009, the refresh token generation. Not yet applied to Supabase.
- Changed: on any 401 from a signed-in request, apps refresh once and send it again.
- Evidence: docs/testing/reports/38.md

### Idempotency, audit and outbox (#36)
- Added: `internal/platform/idempotency`, `internal/audit`, `internal/platform/outbox` and `internal/platform/sensitive`. No endpoint uses them yet.
- Evidence: docs/testing/reports/36.md

### Questions before v0.3.0 (#34)
- Added: the owner's algorithm documents in `docs/context/algorithms/`, as background, and `docs/planning/v0.3.0_open_questions.md`.

### Service skeleton (#32)
- Added: `apps/customer-api` and `apps/rider-api`, with config, logging, health checks, the database pool and graceful shutdown. No business endpoints yet.
- Changed: `internal/dbtarget` keeps the migration and service logins apart.
- Evidence: docs/testing/reports/32.md

### Sign-in migrations applied to Supabase (#30)
- Changed: migrations 0007 and 0008 are applied to `Ninzo-backend-mumbai`, and verified there.
- Evidence: docs/testing/reports/30.md

### Sign-in contract, mock and tables (#28)
- Added: customer Google sign-in and phone verification. Rider ID, password sign-in and reset, and the `applicant` status.
- Added: migrations for `people`, `person_identities`, `rider_credentials`, the OTP purpose, and the `ninzo_service` database login.
- Changed: phone moved from `customers` and `riders` to `people`.

### Authentication rules (#26)
- Added: `docs/engineering/authentication_rules.md`, recording sign-in rules, roles, situations, problems and solutions, with their reasons.

### Docs (#1)
- Added: `docs/engineering/tech_stack.md` and decisions 0001-0006, awaiting owner review.
- Added: draft customer backend docs in `apps/customer-api/docs/`, awaiting owner review.
- Added: zone timer flow in `docs/engineering/data_flow.md` and decision 0007, replacing the KT clock.
- Added: `CLAUDE.md` rule that every task starts with a GitHub Issue.
- Added: `docs/planning/owner_setup_checklist.md`.

### Migrations applied to Supabase (#22)
- Changed: the 6 migrations are applied to `Ninzo-backend-mumbai`, and verified there.
- Evidence: docs/testing/reports/22.md

### Database foundation (#21)
- Added: migrations for the 10 v0.2.0 tables, a migration revoking Supabase's default grants, `cmd/migrate`, which refuses the wrong target, and database tests in CI.
- Changed: local Redis moved to port 6380. Local Postgres runs under emulation on ARM machines.
- Evidence: docs/testing/reports/21.md

### Endpoint data map (#13)
- Added: `docs/engineering/endpoint_data_map.md`, and a test that keeps it complete.

### Mock server (#8)
- Added: `cmd/mock-server` and `internal/mockserver`: every customer and rider endpoint, all scenarios and live stream replay.
- Added: `cmd/contract-bundle`, `contracts/dist` and `contracts/bruno`.
- Fixed: the rider handover contract now declares a retryable 503.
- Evidence: docs/testing/reports/8.md

### Cancel and refund rule (#15)
- Changed: 5-second cancel window with a full refund, replacing free cancel. No rider is contacted until it closes.
- Evidence: docs/testing/reports/15.md
- Added: `cancel_until` on the order, and `reason_code` and `method` on the refund.
- Changed: payment is charged up front, with no holds. `PaymentStatus` is now `pending`, `paid`, `failed`, `not_paid`, `refunded`.

### Rider contract (#7)
- Added: `contracts/rider/openapi.yaml` and `contracts/rider/stream.asyncapi.yaml`.
- Added: rule tests that keep countdown fields and an arrive endpoint out of the rider contract.
- Evidence: docs/testing/reports/7.md

### Customer contract (#6)
- Added: `contracts/customer/openapi.yaml` and `contracts/customer/stream.asyncapi.yaml`.
- Added: `tests/contracts`, which enforces the contract rules in `go test`.
- Evidence: docs/testing/reports/6.md

### API rules (#5)
- Added: `contracts/shared/common.yaml` and `contracts/README.md`: errors, `server_time`, idempotency, paging, sign-in, live streams, order states, customer stages, timer fields.
- Added: `task contracts:lint` and a CI job that lints contracts.
- Evidence: docs/testing/reports/5.md

### Plan (#3)
- Added: agreed milestones v0.1.0 to v1.0.0 in `docs/planning/milestones.md`, and `app:*` labels.
- Changed: Claude merges PRs once checks pass, with no per-PR owner sign-off.
- Removed: the required QR or code scan on arrival.
- Added: rule that the shared Google Doc is how we communicate with the app developers.

### Config (#1)
- Added: `env.example`, `.gitattributes`, `.editorconfig`, `.dockerignore`, `deploy/compose.yml`, `deploy/Dockerfile.service`, and `db:up` and `db:down` tasks.
- Changed: `.gitignore` now covers keys, service account files and local settings.

<!--
## [v0.1.0] — YYYY-MM-DD
### <feature or module name> (#issue)
- Added / Changed / Fixed / Removed: …
- Evidence: docs/testing/reports/<issue>.md
-->
