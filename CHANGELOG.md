# Changelog

Format: newest first. One section per version; inside it, one entry per feature or module,
each with its Issue number. Entries are added in the same PR that ships the feature.

## [Unreleased]

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
