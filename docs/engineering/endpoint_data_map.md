# Endpoint data map: ninzo-backend

Version: 0.3.0 (draft for owner review) · Updated: 2026-11-05

What each customer and rider endpoint, and each background task, reads and writes behind its contract. Migrations are written from this. `tests/contracts` fails if an endpoint is missing here, or if a row names a table not listed in section 1.

Apps never touch the database directly: **app → HTTP → API (Go) → Postgres**. Postgres is the system of record. Redis holds only what can be rebuilt.

**Proposed:** this design has not been built or tested.

## 1. Tables by module

Each table belongs to one module, and only that module has SQL for it (`project_structure.md`). "Built in" is the version whose migration creates it. A column can be added by a later migration when the table it points to does not exist yet.

| Module | Table | Holds | Built in |
|---|---|---|---|
| `platform` | `idempotency_keys` | Stored `2xx` replies, by key | v0.2.0 |
| `platform` | `outbox_events` | Events saved with the change that caused them | v0.2.0 |
| `platform` | `scheduled_tasks` | Due work, with its retry count. See section 5 | v0.2.0 |
| `audit` | `audit_events` | Append-only record of writes | v0.2.0 |
| `identity` | `people` | One row per person, holding the phone and overall status. See `authentication_rules.md` | v0.2.0 |
| `identity` | `person_identities` | Google and Apple accounts linked to a person | v0.2.0 |
| `identity` | `customers` | Customer profiles | v0.2.0 |
| `identity` | `riders` | Rider profiles, including applicants. `zone_id` is added in v0.3.0, when `zones` exists | v0.2.0 |
| `identity` | `rider_credentials` | Not used: riders sign in by OTP only. Kept for the phone-lost options | v0.2.0 |
| `identity` | `rider_verifications` | Document status | v0.2.0 |
| `identity` | `verification_documents` | The status of each rider document, with the reason for a rejection | v0.10.0 |
| `identity` | `otp_requests` | Hashed codes, their purpose, and attempts | v0.2.0 |
| `identity` | `refresh_tokens` | Hashed refresh tokens and their chains | v0.2.0 |
| `notify` | `devices` | Push tokens | v0.2.0 |
| `notify` | `rider_notifications` | What was pushed to a rider, kept 30 days | v0.10.0 |
| `notify` | `customer_notification_settings` | Which optional pushes a customer turned off (offers and news, support replies) | v0.10.0 |
| `geo` | `zones` | Service areas | v0.3.0 |
| `geo` | `zone_settings` | Timer length, perimeter, waits and limits, by version. One global row, overridden per zone | v0.3.0 |
| `geo` | `gates` | Delivery gates | v0.3.0 |
| `geo` | `gate_history` | Earlier versions of each gate | v0.3.0 |
| `geo` | `location_zones` | The polygon and radius where a timer can start, per location, by version | v0.3.0 |
| `geo` | `staging_nodes` | Where scooters wait | v0.3.0 |
| `geo` | `hubs` | Restock sites | v0.3.0 |
| `geo` | `road_data_versions` | One row per OSRM road data build: the OpenStreetMap source and its sha256, the clip box, the extract's timestamp, the profile and its version, the OSRM image and algorithm. Written by `cmd/roaddata` from `build.json`; stored road times point to it (W2) | v1.0.0 |
| `catalog` | `menu_items` | Items and current prices | v0.3.0 |
| `catalog` | `price_history` | Earlier prices | v0.3.0 |
| `catalog` | `availability_rules` | When items can be sold | v0.3.0 |
| `inventory` | `scooters` | Each scooter, and its current rider | v0.3.0 |
| `inventory` | `lots` | Labelled batches, with expiry and status | v0.3.0 |
| `inventory` | `scooter_stock` | `on_hand` and `reserved`, per scooter and lot. See section 7 | v0.3.0 |
| `inventory` | `stock_ledger` | Append-only record of every stock change | v0.3.0 |
| `inventory` | `stock_counts` | A rider's hand count against what the server expected, with the reason the scanner was not used | v0.10.0 |
| `inventory` | `stock_count_lines` | The lines of a hand count: each item, what the server expected and what the rider counted | v0.10.0 |
| `inventory` | `reservations` | Short holds on units | v0.4.0 |
| `tracking` | `shifts` | Rider shifts | v0.3.0 |
| `tracking` | `rider_state` | Each rider's `seq`, written by every change the rider can see | v0.3.0 |
| `tracking` | `rider_events` | The final result of a rider's queued or direct action, by `event_id` | v0.3.0 |
| `tracking` | `location_pings` | Pings, kept as evidence. Partitioned by day. Kept in full through testing | v0.5.0 |
| `tracking` | `shift_breaks` | Breaks inside a shift: start, end and who ended it | v0.10.0 |
| `orders` | `orders` | See section 1.1 | v0.4.0 |
| `orders` | `order_items` | Items, with the price fixed at creation | v0.4.0 |
| `orders` | `order_events` | Append-only true history of each order | v0.4.0 |
| `orders` | `availability_notifications` | "Notify me" requests | v0.6.0 |
| `payments` | `payments` | One row per Razorpay attempt, so a retry is a new row | v0.4.0 |
| `payments` | `refunds` | Refunds, with reason, method and status | v0.4.0 |
| `payments` | `webhook_events` | Razorpay events already handled | v0.4.0 |
| `payments` | `remedies` | Credits owed for a missed timer, and their status | v0.6.0 |
| `dispatch` | `offers` | See section 1.1 | v0.5.0 |
| `dispatch` | `eta_offers` | Longer ETAs offered at `no_scooter` | v0.6.0 |
| `dispatch` | `relocations` | Suggested moves | v0.7.0 |
| `support` | `support_tickets` | Customer reports | v0.6.0 |
| `support` | `ticket_messages` | Messages on a ticket | v0.6.0 |
| `support` | `problem_reports` | Rider problem buttons | v0.6.0 |
| `support` | `arrival_reviews` | Late arrivals awaiting review, with their 10-minute limit | v0.6.0 |
| `support` | `incidents` | One queue of what needs a person: a support alert, problem report, lost signal, open arrival review or open ticket, each with its assignee and status. Opened by database triggers in the same transaction as the source row (migration 0044; a lost signal's report and alert make one `signal_lost` incident); closed by `closeIncident`, or by itself when its ticket or review closes | v0.10.0 |
| `support` | `incident_notes` | Notes staff add to an incident | v0.10.0 |
| `support` | `earnings` | Per-order pay, incentives and paid repositioning time | v0.7.0 |
| `restock` | `restock_requests` | Requests and their states | v0.7.0 |
| `restock` | `restock_lines` | Items and filled quantities | v0.7.0 |
| `restock` | `restock_lots` | Lot numbers in the cassette | v0.7.0 |
| `restock` | `shortfalls` | What a hub could not fill | v0.7.0 |
| `hub` | `hub_staff` | Hub staff and hub leads, each at one hub | v0.8.0 |
| `hub` | `hub_state` | Each hub's seq, raised by every change to its requests, for the live channel | v0.8.0 |
| `hub` | `shortfall_lines` | The short lines of a shortfall, with the missing quantity | v0.8.0 |
| `hub` | `hub_events` | Queued hub actions, each applied once by event_id | v0.8.0 |
| `console` | `console_users` | Console staff: ID, password hash, TOTP secret, role, hub for a hub_manager | v0.9.0 |
| `console` | `console_challenges` | The step between password and authenticator code | v0.9.0 |
| `console` | `approvals` | Changes waiting for a second person, and what was decided | v0.9.0 |
| `console` | `feature_flags` | Flags changed through approvals | v0.9.0 |

### 1.1 Columns that are easy to miss
- **`orders`:** state, customer stage and `customer_seq`. `cancel_until`, `min_wait_until`, `wait_deadline`. `timer_started_at`, `timer_stopped_at`, `timer_missed`. **Fixed when the rider accepts, so a console change cannot affect it:** `timer_duration_s`, `perimeter_m`, `location_zone_id` and its version. `arrival_source` (`gps` or `handover`). `handover_otp`, `otp_attempts`, `ended_reason`. The `Idempotency-Key` that created it.
- **`offers`:** order, rider, state, `fencing_token`, `expires_at`, `rider_accepted_at`, `ended_reason`. This is one rider's view, so a rider who was reassigned sees their own ending, never another rider's progress.
- **`handover_otp`:** unlike sign-in codes, this is kept readable, because the customer must be able to see it. It is cleared once the order is handed over or has ended.
- **Added for the rider app (#157, built in v0.10.0 by #159):** `riders.preferred_area`, `shifts.device_id`, `orders.rider_waiting_since`, and the tables `rider_notifications`, `shift_breaks`, `stock_counts` and `verification_documents`. `max_break_s` is a `zone_settings` value. Until v0.10.0 the endpoints exist only in the contract and the mock.
- **Added for the customer app (#178, built in part B by migration 0043):** `menu_items.is_veg` (default false), `gates.landmark` and `gates.gate_type` (both nullable, also kept in `gate_history`; erasing a customer clears the landmark), and a `customer_notification_settings` table (`offers_and_news`, `support_replies`; no row means off and on; `order_updates` is always true and is not stored). The order's `rider.scooter_plate` is not a new column: it is `scooters.code` of the shift (`shifts.scooter_id`) that took the order, the same code the rider's shift reply calls `plate`.

**Changes from the earlier lists:**
- Added `scooters`, `location_zones`, `zone_settings`, `hubs`, `scheduled_tasks`, `rider_state`, `rider_events`, `devices`, `relocations`, `earnings`, `problem_reports`, `ticket_messages`, `remedies`, `arrival_reviews`, `eta_offers` and `restock_lots`, which the contracts need.
- `offers` belongs to `dispatch`, which writes it. Console settings belong to `geo`.
- `order_legs` waits for multi-scooter orders, which are P2. The pilot is single-leg.
- `hub_staff` is built in v0.8.0 (#123). `hub_queue_slots` waits until the hub queue needs time slots. Console staff, roles and approvals wait for v0.9.0.

## 2. What Redis holds
Redis is never the source of truth. If it is lost, positions refill from the next pings, and everything else comes from Postgres.

| Use | Key idea | Written by |
|---|---|---|
| Latest rider position | `rider:{id}` | `locationPing` |
| Riders by map cell | `cell:{h3}` | `locationPing` |
| Live stream fan-out | Pub/sub per order and per rider | Any change a stream reader can see |
| Rate limits | Requests per IP, per session and ping counters. Limits per phone and per code are in Postgres | `requestOtp`, `verifyOtp`, `refreshToken`, `locationPing` |
| Availability cache | Short-lived, per gate | `getAvailability` |

`locationPing` writes every ping to `location_pings`. The zone check uses that table, `location_zones` and `orders`, so a Redis failure cannot change a timer. If Redis is down, no new offers go out, because rider positions are unknown.

## 3. Customer API

| Endpoint | Reads | Writes | Redis | Outside calls |
|---|---|---|---|---|
| `requestOtp` | `otp_requests` | `otp_requests` | Rate limit | SMS gateway |
| `verifyOtp` | `otp_requests`, `people`, `customers` | `otp_requests`, `people`, `customers`, `refresh_tokens`, `audit_events` | Rate limit | |
| `signInWithGoogle` | `person_identities`, `people`, `customers` | `people`, `person_identities`, `customers`, `refresh_tokens`, `audit_events` | Rate limit | Google's public keys |
| `requestPhoneOtp` | `customers`, `people` | `otp_requests` | Rate limit | SMS gateway |
| `verifyPhone` | `otp_requests`, `customers`, `people`, `person_identities` | `otp_requests`, `people`, `refresh_tokens`, `outbox_events`, `audit_events` | Rate limit | |
| `changeSignInMethod` | `otp_requests`, `customers`, `people`, `person_identities`, `riders` | `otp_requests`, `customers`, `person_identities`, `people`, `devices`, `refresh_tokens`, `outbox_events`, `audit_events` | Rate limit | Google's public keys |
| `refreshToken` | `refresh_tokens`, `people`, `customers` | `refresh_tokens`, `audit_events` | Rate limit | |
| `signOut` | `refresh_tokens` | `refresh_tokens` | | |
| `getMe` | `customers`, `people` | | | |
| `updateMe` | `customers`, `people`, `refresh_tokens` | `customers`, `audit_events`, `idempotency_keys` | | |
| `getNotificationSettings` | `customer_notification_settings`, `customers`, `people`, `refresh_tokens` | | | Built in #178 part B |
| `updateNotificationSettings` | `customer_notification_settings`, `customers`, `people`, `refresh_tokens` | `customer_notification_settings`, `idempotency_keys` | | Built in #178 part B (`customer.notification_settings_updated` is audited). The push worker reads it for `ticket_reply` |
| `registerDevice` | `devices`, `customers`, `people`, `refresh_tokens` | `devices`, `idempotency_keys` | | |
| `removeDevice` | `devices` | `devices`, `idempotency_keys` | | |
| `listGates` | `gates`, `customers`, `people`, `refresh_tokens` | | | |
| `createGate` | `customers`, `people`, `zones`, `gates`, `refresh_tokens` | `gates`, `gate_history`, `audit_events`, `idempotency_keys` | | |
| `checkLocation` | `zones` | | | |
| `listMenuItems` | `menu_items` | | | |
| `getMenuItem` | `menu_items` | | | |
| `getAvailability` | `gates`, `zones`, `zone_settings`, `menu_items`, `availability_rules`, `scooter_stock`, `lots`, `scooters`, `shifts`, `riders`, `people`, `staging_nodes`, `refresh_tokens` | | Positions from v0.5.0 | Routing engine, after the road test |
| `notifyWhenAvailable` | `customers`, `people`, `gates` | `availability_notifications`, `idempotency_keys` | | |
| `listOrders` | `orders`, `order_items`, `payments`, `gates`, `riders`, `remedies` | | | |
| `createOrder` | `people`, `customers`, `orders`, `gates`, `zones`, `zone_settings`, `menu_items`, `availability_rules`, `scooters`, `scooter_stock`, `lots` | `orders`, `order_items`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `payments`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Positions | Razorpay, routing engine |
| `confirmTestPayment` | `orders`, `payments`, `zone_settings`, `refresh_tokens` | `orders`, `payments`, `order_events`, `refunds` | | Staging only. Stands in for the Razorpay webhook |
| `getOrder` | `orders`, `order_items`, `payments`, `gates`, `riders`, `shifts`, `scooters`, `eta_offers`, `remedies` | | | |
| `cancelOrder` | `orders`, `payments`, `reservations`, `offers` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `offers`, `rider_state`, `refunds`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | None: the refund runs from a task |
| `changeOrderGate` | `orders`, `gates`, `zones`, `zone_settings`, `scooters`, `scooter_stock`, `lots`, `reservations`, `payments` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `payments`, `audit_events`, `idempotency_keys` | Positions | Razorpay, routing engine |
| `retryPayment` | `orders`, `payments`, `reservations`, `scooter_stock` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `payments`, `idempotency_keys` | Fan-out | Razorpay |
| `acceptLongerEta` | `orders`, `eta_offers`, `scooters`, `scooter_stock`, `zone_settings` | `orders`, `order_events`, `eta_offers`, `reservations`, `scooter_stock`, `stock_ledger`, `scheduled_tasks`, `outbox_events`, `idempotency_keys` | Fan-out | |
| `startMaskedCall` | `orders`, `riders`, `customers` | `audit_events`, `idempotency_keys` | | Telephony |
| `getOrderRefund` | `orders`, `refunds` | | | |
| `createTicket` | `customers`, `people`, `orders` | `support_tickets`, `ticket_messages`, `outbox_events`, `idempotency_keys` | | |
| `getTicket` | `support_tickets`, `ticket_messages` | | | |
| `razorpayWebhook` | `webhook_events`, `payments`, `orders`, `reservations`, `scooter_stock`, `scooters`, `zone_settings`, `location_zones` | `webhook_events`, `payments`, `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `refunds`, `scheduled_tasks`, `outbox_events`, `audit_events` | Positions, fan-out | None: any refund runs from a task |

## 4. Rider API

Actions a rider can queue offline (`handOver`, `scanStock`, `confirmRestock`, `declineRestock`, `reportProblem`) write `rider_events` on the direct path too, so a direct call and its queued copy count as one. This table is built in v0.3.0 for that reason.

Until a later table exists, an endpoint simply does not use it: `listNodes` has no relocations before v0.7.0, and `scanStock` has no restock check before then.

| Endpoint | Reads | Writes | Redis | Outside calls |
|---|---|---|---|---|
| `requestOtp` | `otp_requests`, `riders`, `people` | `otp_requests` | Rate limit | SMS gateway, only if the account exists |
| `verifyOtp` | `otp_requests`, `riders`, `people` | `otp_requests`, `refresh_tokens`, `audit_events` | Rate limit | |
| `refreshToken` | `refresh_tokens`, `people`, `riders` | `refresh_tokens`, `audit_events` | Rate limit | |
| `signOut` | `refresh_tokens` | `refresh_tokens` | | |
| `getRider` | `riders`, `people`, `audit_events` | | | |
| `getRiderState` | `rider_state`, `riders`, `shifts`, `offers`, `orders`, `order_items`, `gates`, `restock_requests`, `restock_lines`, `restock_lots`, `shortfalls`, `hubs`, `scooters`, `relocations`, `staging_nodes`, `menu_items` | | | |
| `getVerification` | `rider_verifications`, `verification_documents` | | | |
| `startVerification` | `riders` | `rider_verifications`, `audit_events`, `idempotency_keys` | | DigiLocker, a mock for now |
| `registerRiderDevice` | `devices`, `riders`, `people`, `refresh_tokens` | `devices`, `idempotency_keys` | | |
| `removeRiderDevice` | `devices` | `devices`, `idempotency_keys` | | |
| `startShift` | `riders`, `rider_verifications`, `devices`, `shifts`, `staging_nodes`, `scooters`, `relocations` | `shifts`, `scooters`, `rider_state`, `audit_events`, `idempotency_keys` | | |
| `endShift` | `shifts`, `offers`, `scooters`, `relocations` | `shifts`, `scooters`, `rider_state`, `audit_events`, `idempotency_keys` | | |
| `listNodes` | `zones`, `staging_nodes`, `scooters`, `relocations`, `hubs` | | Positions | |
| `acceptRelocation` | `relocations` | `relocations`, `rider_state`, `audit_events`, `idempotency_keys` | | |
| `scanStock` | `rider_events`, `lots`, `scooters`, `scooter_stock`, `restock_requests`, `restock_lots` | `rider_events`, `scooter_stock`, `stock_ledger`, `rider_state`, `audit_events`, `idempotency_keys` | | |
| `getStock` | `scooters`, `scooter_stock`, `lots`, `menu_items` | | | |
| `listOffers` | `offers`, `orders`, `order_items`, `gates` | | | |
| `acceptOffer` | `offers`, `orders`, `zone_settings`, `location_zones`, `reservations` | `offers`, `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `rider_state`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | |
| `declineOffer` | `offers`, `orders` | `offers`, `order_events`, `rider_state`, `scheduled_tasks`, `audit_events`, `idempotency_keys` | Fan-out | |
| `markReady` | `offers`, `orders`, `zone_settings`, `location_pings` | `offers`, `orders`, `order_events`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | Position, fan-out | |
| `getRiderOrder` | `offers`, `orders`, `order_items`, `gates` | | | |
| `getRoute` | `offers`, `orders`, `gates` | | Position | Routing engine |
| `handOver` | `rider_events`, `offers`, `orders`, `order_items`, `gates`, `payments`, `location_pings`, `reservations` | `rider_events`, `offers`, `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `rider_state`, `refunds`, `arrival_reviews`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Position, fan-out | None: a partial refund runs from a task |
| `startRiderMaskedCall` | `offers`, `orders`, `customers` | `audit_events`, `idempotency_keys` | | Telephony |
| `locationPing` | `offers`, `orders`, `location_zones`, `location_pings`, `arrival_reviews` | `location_pings`, `orders`, `order_events`, `rider_state`, `remedies`, `arrival_reviews`, `scheduled_tasks`, `outbox_events` | Position, cells, rate limit, fan-out | |
| `sendEventBatch` | `rider_events`, `offers`, `orders`, `order_items`, `gates`, `payments`, `location_pings`, `lots`, `scooters`, `scooter_stock`, `reservations`, `restock_requests`, `restock_lots` | `rider_events`, `location_pings`, `offers`, `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `rider_state`, `refunds`, `arrival_reviews`, `problem_reports`, `restock_requests`, `restock_lines`, `scheduled_tasks`, `outbox_events`, `audit_events` | Fan-out | None |
| `listRestockSuggestions` | `restock_requests`, `restock_lines`, `hubs`, `shifts`, `staging_nodes` | | | |
| `confirmRestock` | `rider_events`, `restock_requests` | `rider_events`, `restock_requests`, `restock_lines`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | |
| `declineRestock` | `rider_events`, `restock_requests` | `rider_events`, `restock_requests`, `rider_state`, `audit_events`, `idempotency_keys` | | |
| `createRestock` | `hubs`, `restock_requests` | `restock_requests`, `restock_lines`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `getRestockOptions` | `riders`, `hubs`, `menu_items` | | | |
| `respondToShortfall` | `shortfalls`, `restock_requests` | `shortfalls`, `restock_requests`, `rider_state`, `audit_events`, `idempotency_keys` | Fan-out | |
| `getRestock` | `restock_requests`, `restock_lines`, `restock_lots`, `shortfalls`, `hubs`, `shifts`, `staging_nodes` | | | |
| `reportProblem` | `rider_events`, `orders` | `rider_events`, `problem_reports`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `getEarnings` | `earnings` | | | |
| `updateRider` | `riders` | `riders`, `audit_events`, `idempotency_keys` | | |
| `listRiderNotifications` | `rider_notifications` | | | |
| `listRiderShifts` | `shifts`, `shift_breaks`, `offers`, `orders` | | | |
| `getShiftSummary` | `shifts`, `shift_breaks`, `offers`, `orders`, `order_items`, `menu_items`, `scooter_stock`, `stock_ledger`, `earnings` | | | |
| `startBreak` | `shifts`, `shift_breaks`, `offers`, `zone_settings` | `shifts`, `shift_breaks`, `rider_state`, `audit_events`, `idempotency_keys` | Fan-out | The break ends on its own in a background pass (`shifts.EndDueBreaks`) that reads `shifts.break_ends_at`, so no `scheduled_tasks` row is written |
| `endBreak` | `shifts`, `shift_breaks` | `shifts`, `shift_breaks`, `rider_state`, `audit_events`, `idempotency_keys` | Fan-out | |
| `cancelRestock` | `restock_requests` | `restock_requests`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | |
| `markWaitingAtGate` | `offers`, `orders`, `location_pings` | `orders`, `order_events`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | Push, to the customer |
| `submitStockCount` | `shifts`, `scooters`, `scooter_stock`, `lots`, `menu_items` | `stock_counts`, `stock_count_lines`, `audit_events`, `idempotency_keys` | | |

## 4.1 Partner API

The partner (hub) app's endpoints (#122). Every hub path acts on the signed-in staff member's hub. Parts C to E of v0.8.0 build the rows after `getStaff`; until then only the contract and mock exist.

| Endpoint | Reads | Writes | Redis | Outside calls |
|---|---|---|---|---|
| `requestOtp` | `otp_requests`, `hub_staff`, `people` | `otp_requests` | Rate limit | SMS gateway, only if the account exists |
| `verifyOtp` | `otp_requests`, `hub_staff`, `people` | `otp_requests`, `refresh_tokens`, `audit_events` | Rate limit | |
| `refreshToken` | `refresh_tokens`, `people`, `hub_staff` | `refresh_tokens`, `audit_events` | Rate limit | |
| `signOut` | `refresh_tokens` | `refresh_tokens` | | |
| `getStaff` | `hub_staff`, `people`, `hubs` | | | |
| `listHubRequests` | `hub_staff`, `hub_state`, `restock_requests`, `restock_lines`, `restock_lots`, `lots`, `menu_items`, `scooters`, `shortfalls`, `shortfall_lines` | | | |
| `getHubRequest` | `hub_staff`, `restock_requests`, `restock_lines`, `restock_lots`, `lots`, `menu_items`, `scooters`, `shortfalls`, `shortfall_lines` | | | |
| `acceptHubRequest` | `hub_staff`, `hubs`, `restock_requests` | `restock_requests`, `hub_state`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `getPickList` | `hub_staff`, `restock_requests`, `restock_lines`, `restock_lots`, `lots`, `menu_items`, `scooter_stock` | | | |
| `scanHubLot` | `hub_staff`, `restock_requests`, `restock_lines`, `lots`, `restock_lots`, `scooter_stock` | `restock_lots`, `restock_requests`, `hub_state`, `rider_state`, `outbox_events`, `idempotency_keys` | | |
| `unscanHubLot` | `hub_staff`, `restock_requests`, `lots`, `restock_lots` | `restock_lots`, `restock_requests`, `hub_state`, `rider_state`, `idempotency_keys` | | |
| `recordShortfall` | `hub_staff`, `restock_requests`, `restock_lines`, `hubs`, `menu_items` | `shortfalls`, `shortfall_lines`, `restock_requests`, `hub_state`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `markHubRequestReady` | `hub_staff`, `restock_requests`, `restock_lines`, `restock_lots`, `lots`, `shortfalls` | `restock_requests`, `restock_lines`, `hub_state`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `getCassetteLabel` | `hub_staff`, `restock_requests`, `restock_lines`, `restock_lots`, `lots`, `menu_items`, `scooters`, `hubs` | | | |
| `listHubLots` | `hub_staff`, `lots`, `restock_lots` | | | |
| `quarantineLot` | `hub_staff`, `lots`, `restock_lots`, `restock_requests`, `scooter_stock` | `lots`, `restock_lots`, `restock_requests`, `hub_state`, `audit_events`, `idempotency_keys` | | |
| `sendEventBatch` | `hub_events`, `hub_staff`, `restock_requests`, `restock_lines`, `restock_lots`, `lots`, `shortfalls` | `hub_events`, `restock_lots`, `restock_requests`, `restock_lines`, `shortfalls`, `shortfall_lines`, `lots`, `hub_state`, `rider_state`, `outbox_events`, `audit_events` | | |

## 4.2 Console API

The console's endpoints (#133, decision 0015). v0.9.0 parts B to F build them; until then only the contract and mock exist. The last 18 rows (orders, support actions, incidents, hub operations and the live map) are v0.10.0 (#146 to #150), and read `incidents` and `incident_notes`, which arrive in v0.10.0.

| Endpoint | Reads | Writes | Redis | Outside calls |
|---|---|---|---|---|
| `consoleLogin` | `console_users` | `console_users`, `console_challenges`, `audit_events` | Rate limit | |
| `enrollTotp` | `console_challenges`, `console_users` | `console_users` | Rate limit | |
| `verifyTotp` | `console_challenges`, `console_users` | `console_challenges`, `refresh_tokens`, `audit_events` | Rate limit | |
| `refreshToken` | `refresh_tokens`, `console_users` | `refresh_tokens`, `audit_events` | Rate limit | |
| `signOut` | `refresh_tokens` | `refresh_tokens` |  | |
| `changePassword` | `console_users` | `console_users`, `refresh_tokens`, `audit_events`, `idempotency_keys` |  | |
| `getConsoleUser` | `console_users` |  |  | |
| `listConsoleUsers` | `console_users` |  |  | |
| `createConsoleUser` | `console_users`, `hubs` | `console_users`, `audit_events`, `idempotency_keys` |  | |
| `updateConsoleUser` | `console_users`, `hubs` | `console_users`, `refresh_tokens`, `audit_events`, `idempotency_keys` |  | |
| `resetConsoleUser` | `console_users` | `console_users`, `refresh_tokens`, `audit_events`, `idempotency_keys` |  | |
| `listApprovals` | `approvals`, `console_users` |  |  | |
| `approveChange` | `approvals`, `console_users`, `zone_settings`, `zones`, `feature_flags` | `approvals`, `zone_settings`, `zones`, `feature_flags`, `audit_events`, `idempotency_keys` |  | |
| `cancelChange` | `approvals` | `approvals`, `audit_events`, `idempotency_keys` | | |
| `rejectChange` | `approvals` | `approvals`, `audit_events`, `idempotency_keys` |  | |
| `listRiderReviews` | `riders`, `people`, `rider_verifications` |  |  | |
| `decideRider` | `riders`, `rider_verifications`, `zones`, `shifts` | `riders`, `refresh_tokens`, `audit_events`, `idempotency_keys` |  | |
| `listFlags` | `feature_flags` |  |  | |
| `requestFlagChange` | `feature_flags`, `approvals` | `approvals`, `audit_events`, `idempotency_keys` |  | |
| `listZones` | `zones` |  |  | |
| `getGlobalSettings` | `zone_settings` |  |  | |
| `requestGlobalSettingsChange` | `zone_settings`, `approvals` | `approvals`, `audit_events`, `idempotency_keys` |  | |
| `getZoneSettings` | `zones`, `zone_settings` |  |  | |
| `requestZoneSettingsChange` | `zones`, `zone_settings`, `approvals` | `approvals`, `audit_events`, `idempotency_keys` |  | |
| `requestKillSwitch` | `zones`, `approvals` | `approvals`, `audit_events`, `idempotency_keys` |  | |
| `listAdminMenu` | `menu_items` |  |  | |
| `createMenuItem` | `menu_items` | `menu_items`, `price_history`, `audit_events`, `idempotency_keys` |  | |
| `updateMenuItem` | `menu_items` | `menu_items`, `price_history`, `audit_events`, `idempotency_keys` |  | |
| `getPriceHistory` | `menu_items`, `price_history` | | | |
| `listAdminGates` | `gates` |  |  | |
| `updateGate` | `gates`, `gate_history`, `location_zones` | `gates`, `gate_history`, `audit_events`, `idempotency_keys` |  | |
| `getGateHistory` | `gates`, `gate_history` |  |  | |
| `listAdminHubs` | `hubs`, `hub_staff` |  |  | |
| `createHub` | `zones`, `hubs` | `hubs`, `audit_events`, `idempotency_keys` |  | |
| `updateHub` | `hubs` | `hubs`, `audit_events`, `idempotency_keys` |  | |
| `listHubStaff` | `hub_staff`, `people` |  |  | |
| `addHubStaff` | `hubs`, `people`, `hub_staff` | `people`, `hub_staff`, `audit_events`, `idempotency_keys` |  | |
| `updateHubStaff` | `hub_staff`, `hubs` | `hub_staff`, `refresh_tokens`, `audit_events`, `idempotency_keys` |  | |
| `listAuditEvents` | `audit_events` |  |  | |
| `listArrivalReviews` | `arrival_reviews` |  |  | |
| `decideArrivalReview` | `arrival_reviews`, `orders`, `remedies`, `console_users` | `arrival_reviews`, `orders`, `remedies`, `order_events`, `audit_events`, `idempotency_keys` |  | |
| `listSupportAlerts` | `outbox_events` |  |  | |
| `listAdminOrders` | `orders`, `people`, `customers`, `riders` | | | Built (#146). Only `right(phone, 4)` is read from `people`; the full number never reaches the service. Filters use `orders_created_idx` and `orders_zone_created_idx`; a phone search finds people by `people_phone_last4_idx`, then their orders by `orders_customer_idx` and `orders_rider_created_idx`; an order number is looked up by the unique `order_no` index |
| `getOrderTimeline` | `orders`, `order_items`, `order_events`, `offers`, `payments`, `refunds`, `remedies`, `problem_reports`, `support_tickets`, `arrival_reviews`, `audit_events`, `location_pings`, `gates`, `riders`, `people` |  |  | Built (#146). One read-only repeatable-read snapshot. `audit_events` rows of the order and its refunds show only action, entity and reason (before and after stay out); problem notes and ticket text stay out; pings are counted (`location_pings_order_idx`), never listed. Refunds and remedies now name who asked (`refunds.requested_by`, `remedies.created_by`, migration 0042), and a remedy's `items`; the reason staff typed shows on the order card, and stays out of the events (#147) |
| `listTickets` | `support_tickets`, `ticket_messages` |  |  | Built (#147). Newest first by `support_tickets_created_idx`; `subject` is the first 80 characters of the customer's first message |
| `getTicket` | `support_tickets`, `ticket_messages`, `console_users` |  |  | Built (#147). The customer's own words are shown to support here, and nowhere else in the console |
| `replyTicket` | `support_tickets` | `ticket_messages`, `support_tickets`, `outbox_events`, `audit_events`, `idempotency_keys` |  | Built (#147). A `sender = 'support'` row the customer's `getTicket` already reads; one `push.customer` event `{customer_id, ticket_id, stage: ticket_reply}`, no words. FCM stays the log stand-in until v1.0.0 |
| `closeTicket` | `support_tickets` | `support_tickets`, `audit_events`, `idempotency_keys` |  | Built (#147). Its incident closes by itself (trigger, #148) |
| `requestRefund` | `orders`, `payments`, `refunds`, `zone_settings`, `approvals`, `console_users` | `refunds`, `orders`, `approvals`, `order_events`, `audit_events`, `idempotency_keys` |  | Built (#147). Locks the order, then its paid payment, so two requests on an order run one after the other. Up to `refund_approval_paise` (console refunds on the order plus any waiting approval plus this one) the refund is written at once (`orders.RecordSupportRefund`, status `processed` for the test provider); above it only an approval is written, and `approveChange` makes the refund (`console.ApplyRefund`). Razorpay, through `send_refund`, in v1.0.0 |
| `issueReplacement` | `orders`, `order_items`, `payments` | `remedies`, `orders`, `order_events`, `audit_events`, `idempotency_keys` |  | Built (#147). No outbox event: the customer's order view refreshes through `orders.seq` |
| `issueCredit` | `orders`, `payments` | `remedies`, `orders`, `order_events`, `audit_events`, `idempotency_keys` |  | Built (#147) |
| `listIncidents` | `incidents`, `incident_notes`, `console_users` |  |  | Built (#148). Newest first by `opened_at`; `status` and `kind` filters; no source text |
| `getIncident` | `incidents`, `incident_notes`, `console_users` |  |  | Built (#148) |
| `assignIncident` | `incidents`, `console_users` | `incidents`, `audit_events`, `idempotency_keys` |  | Built (#148). Row lock on the incident; the user is locked while assigned |
| `addIncidentNote` | `incidents`, `console_users` | `incident_notes`, `audit_events`, `idempotency_keys` |  | Built (#148) |
| `closeIncident` | `incidents`, `support_tickets`, `arrival_reviews` | `incidents`, `outbox_events`, `problem_reports`, `audit_events`, `idempotency_keys` |  | Built (#148). A support alert is marked delivered and a problem report handled; a ticket or review is never written here |
| `listHubOps` | `hubs`, `hub_staff`, `restock_requests`, `shortfalls`, `lots`, `zone_settings`, `console_users` |  |  | |
| `listStuckRestocks` | `restock_requests`, `hubs`, `riders`, `people`, `zone_settings`, `console_users` |  |  | |
| `expireRestock` | `restock_requests`, `restock_lots`, `zone_settings`, `console_users` | `restock_requests`, `restock_lots`, `hub_state`, `rider_state`, `outbox_events`, `audit_events`, `idempotency_keys` |  | FCM, through `deliver_outbox`. Only before `ready`; packed lots go back to the hub's stock |
| `getLiveZone` | `zones`, `riders`, `people`, `shifts`, `rider_state`, `location_pings`, `scooters`, `scooter_stock`, `staging_nodes`, `orders`, `gates`, `incidents` |  | Latest rider position | |

**New columns (v0.10.0). Migration 0042 (#147) added the first four:**
- `refunds`: `requested_by` (the console user, null for an automatic refund) and `note` (the reason staff typed). A console refund has its own `reason_code`, `support_refund_<n>`, because of `UNIQUE (payment_id, reason_code)`, so partial refunds on one payment do not collide.
- `remedies`: `items` (the replacement's lines) and `created_by` (null when the system made it). An order has at most one remedy of each type (`UNIQUE (order_id, type)`), so `issueCredit` and `issueReplacement` answer `remedy_exists` for a second.
- `support_tickets`: `closed_at`, `closed_by` and `close_reason`; `ticket_messages.author_id`. Statuses stay `open` and `resolved`; closing sets `resolved`.
- `zone_settings`: `refund_approval_paise` (20000 by default, 0 to 10,000,000).
- `incidents` and `incident_notes` (#148, migration 0044): one row per source (`kind`, `source_id` unique), and one open `signal_lost` per order (a lost signal's problem report and support alert make one). `problem_reports.handled_at` is set when a problem report's incident is closed. A handled support alert gets `outbox_events.delivered_at`, which takes it out of the due index.

## 5. Background work
`cmd/worker` runs these from `scheduled_tasks`, or from `outbox_events`. A restart loses nothing, because they are rows and not memory. Each one must be safe to run twice.

| Task | Reads | Writes | Runs when | Outside calls |
|---|---|---|---|---|
| `start_rider_search` | `orders`, `reservations`, `scooters`, `scooter_stock`, `zone_settings` | `offers`, `orders`, `order_events`, `eta_offers`, `reservations`, `scooter_stock`, `stock_ledger`, `rider_state`, `scheduled_tasks`, `outbox_events` | `cancel_until` passes, or a longer ETA is accepted. If the pre-selected rider is gone, it moves stock to another scooter, or moves to `NO_SCOOTER` | Routing engine |
| `offer_expires` | `offers`, `orders`, `reservations`, `scooters`, `scooter_stock` | `offers`, `orders`, `order_events`, `eta_offers`, `reservations`, `scooter_stock`, `stock_ledger`, `rider_state`, `scheduled_tasks`, `outbox_events` | An offer's `expires_at` passes: try the next rider, or move to `NO_SCOOTER` | Routing engine |
| `min_wait_passes` | `orders`, `reservations` | `orders`, `order_events`, `reservations`, `stock_ledger`, `outbox_events` | `min_wait_until` passes: show `rider_accepted`, commit the reservation, raise `customer_seq` | |
| `cancel_window_closes` | `orders` | `orders`, `outbox_events` | `cancel_until` passes: `cancellable` changes, and `customer_seq` rises | |
| `wait_deadline_passes` | `orders` | `orders`, `outbox_events` | `wait_deadline` passes: the same, with cancelling allowed again | |
| `no_scooter_timeout` | `orders`, `reservations`, `payments` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `refunds`, `scheduled_tasks`, `outbox_events` | The time to choose passes: `TIMED_OUT`, with a refund | |
| `packing_limit` | `orders`, `offers`, `zone_settings` | `orders`, `offers`, `order_events`, `rider_state`, `outbox_events` | The packing limit passes: `PACKED`, with no rider action | |
| `reservation_expires` | `reservations`, `orders`, `payments` | `reservations`, `scooter_stock`, `stock_ledger`, `orders`, `order_events`, `refunds`, `scheduled_tasks`, `outbox_events` | A reservation's expiry passes. A paid order is refunded, so no customer pays with no stock held | |
| `send_refund` | `refunds`, `payments` | `refunds`, `payments`, `orders`, `outbox_events`, `audit_events` | A refund is created. Retried, then raises an ops alert | Razorpay |
| `timer_ends` | `orders`, `location_pings` | `orders`, `order_events`, `remedies`, `arrival_reviews`, `scheduled_tasks`, `outbox_events` | The timer's length passes: `timer_missed`. A remedy follows if pings are fresh. If the signal is lost, the remedy waits until it returns or the limit passes | |
| `signal_lost_check` | `orders`, `location_pings` | `orders`, `order_events`, `rider_state`, `scheduled_tasks`, `outbox_events` | No ping for 30 seconds, after the timer or deadline: release the OTP. Otherwise it reschedules itself until the order moves on | |
| `arrival_review_limit` | `arrival_reviews`, `orders` | `arrival_reviews`, `remedies`, `outbox_events`, `audit_events` | 10 minutes pass with no decision | |
| `problem_followup` | `problem_reports`, `orders`, `offers`, `reservations`, `payments` | `problem_reports`, `orders`, `order_events`, `offers`, `reservations`, `scooter_stock`, `stock_ledger`, `rider_state`, `refunds`, `scheduled_tasks`, `outbox_events` | A problem affects an order: rematch silently during the wait, or fail and refund | Routing engine |
| `deliver_outbox` | `outbox_events`, `devices` | `outbox_events`, `earnings`, `availability_notifications` | Polling. Sends pushes, and records pay when an order is handed over | FCM, SMS gateway |
| `suggest_relocations` | `scooters`, `staging_nodes`, `orders`, `zone_settings` | `relocations`, `rider_state`, `outbox_events` | On a timer, from v0.7.0 | |
| `suggest_restocks` | `scooters`, `scooter_stock`, `lots`, `zone_settings` | `restock_requests`, `restock_lines`, `rider_state`, `outbox_events` | On a timer, from v0.7.0 | |
| `open_incidents` | `outbox_events`, `problem_reports`, `arrival_reviews`, `support_tickets` | `incidents` | Not a background task (#148): database triggers (migration 0044) open the incident in the same transaction as the support alert, problem report, arrival review or ticket, and close it when the ticket or review closes. Migration 0044 backfills what existed | |
| `purge_platform_rows` | `idempotency_keys`, `outbox_events`, `refresh_tokens`, `otp_requests` | `idempotency_keys`, `outbox_events`, `refresh_tokens`, `otp_requests` | Daily. Deletes keys older than 24 hours, delivered events older than 7 days, refresh tokens 30 days after they expire or end, and codes older than a day | |
| `create_ping_partitions` | `location_pings` | `location_pings` | Daily. Drops nothing until retention is decided. `ninzo_service` cannot create tables, so this needs a narrow function made by a migration, in v0.5.0 | |

## 6. What must happen in one transaction

| Action | All of these commit together, or none do |
|---|---|
| `createOrder`, first part | The order and its `Idempotency-Key`, items, reservation, stock change and ledger entry. Razorpay is called after this commit. A retry finds the order and resumes |
| Webhook confirms payment | `webhook_events`, `payments`, and under the order's row lock: `cancel_until`, `min_wait_until`, `wait_deadline`, the reservation's new expiry, and the tasks. If the order has already ended, a refund is created instead |
| `cancelOrder` | The state check under the order's row lock, the order, its event, stock release, a pending refund and the stored reply. The Razorpay refund runs afterwards, from a task, so it can retry |
| `start_rider_search` | The same row lock as `cancelOrder`, so exactly one of them wins at the end of the window |
| `acceptOffer` | The offer's check against its fencing token, the order, the timer values fixed on the order, the reservation no longer expiring, and `rider_state` |
| Rematch | Under the order's row lock: the old offer ended, stock released from the old scooter and reserved on the new one, the new offer, and both riders' `rider_state` |
| Zone entry and perimeter | The timer value, written once by a conditional update, with the order's event and `customer_seq` |
| `handOver` | `rider_events`, OTP attempt count, the order, the final stock change and ledger entry, and any refund and review |

**No outside call ever runs inside a transaction.** Razorpay, SMS, routing and telephony run before or after, and retries are idempotent.

## 7. Rules the tables must enforce
These are checked in the database as well as in Go, so a bug cannot break them:
- **Stock:** `scooter_stock` has `on_hand` and `reserved`, and `0 <= reserved <= on_hand` always holds. Reserving raises `reserved`. Releasing lowers it. A handover lowers both. Available stock is `on_hand - reserved`.
- `orders` has a unique `Idempotency-Key` per customer, so one tap makes one order.
- `webhook_events` has a unique Razorpay event ID.
- `rider_events` has a unique `event_id` per rider.
- `refunds` are unique per payment and reason, and money is always integer minor units. The total refunded on a payment can never be more than was paid.
- `stock_ledger`, `order_events` and `audit_events` are append-only.
- Sign-in codes, refresh tokens and passwords are stored only as hashes.
- `idempotency_keys.response` never holds tokens, passwords or a real `handover_otp`. A stored `Order` keeps that field as null, so a replay is still valid, and the app gets the live code from `GET` or the stream. A copy can never outlive the order clearing it.
- A person holds at most one profile per role, one identity per provider, and a provider account belongs to one person.
- A new rider is an `applicant` unless Ninzo sets otherwise, and `riders.status` allows only known values.
- Row Level Security is on for every table, including each `location_pings` partition, with no policies, so the Supabase Data API can read nothing even if it is left on. Services connect as `ninzo_service`, which can bypass it, but cannot change tables, truncate, or read migration history. Migrations run as `postgres`.

## 8. Open points
- **Retention:** the owner decided on 2026-11-05 to keep `location_pings` in full through the testing period, and to set retention after it. Personal data retention may need legal input.
- **Routing engine:** Google Routes or OSRM and Valhalla is still open. It affects `getAvailability`, `createOrder`, `getRoute` and the rider search.
- **Hubs before v0.8.0:** `restock_requests` is built in v0.7.0, and a simulator stands in for hub actions.
- **Lots before v0.8.0:** nothing creates lots until the hub APIs exist, so they are seed data.
