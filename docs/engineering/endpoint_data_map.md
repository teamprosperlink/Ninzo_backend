# Endpoint data map: ninzo-backend

Version: 0.1.0 (draft for owner review) · Updated: 2026-11-05

What each customer and rider endpoint reads and writes behind its contract. Migrations are written from this. `tests/contracts` fails if an endpoint is missing here, or if a row names a table not listed in section 1.

Apps never touch the database directly: **app → HTTP → API (Go) → Postgres**. Postgres is the system of record. Redis holds only what can be rebuilt.

**Proposed:** this design has not been built or tested.

## 1. Tables by module

Each table belongs to one module, and only that module has SQL for it (`project_structure.md`). "Built in" is the version whose migration creates it.

| Module | Table | Holds | Built in |
|---|---|---|---|
| `platform` | `idempotency_keys` | Stored `2xx` replies, by key | v0.2.0 |
| `platform` | `outbox_events` | Events saved with the change that caused them | v0.2.0 |
| `platform` | `scheduled_tasks` | Due work: the cancel window closing, offer and reservation expiry, stage changes, deadlines | v0.2.0 |
| `audit` | `audit_events` | Append-only record of writes | v0.2.0 |
| `identity` | `customers` | Customer accounts | v0.2.0 |
| `identity` | `riders` | Rider accounts and their zone | v0.2.0 |
| `identity` | `rider_verifications` | Document status | v0.2.0 |
| `identity` | `otp_requests` | Hashed sign-in codes and attempts | v0.2.0 |
| `identity` | `refresh_tokens` | Hashed refresh tokens and their chains | v0.2.0 |
| `notify` | `devices` | Push tokens | v0.2.0 |
| `geo` | `zones` | Service areas | v0.3.0 |
| `geo` | `zone_settings` | Timer length, perimeter, waits and limits, by version | v0.3.0 |
| `geo` | `gates` | Delivery gates | v0.3.0 |
| `geo` | `gate_history` | Earlier versions of each gate | v0.3.0 |
| `geo` | `location_zones` | The polygon and radius where a timer can start, per location | v0.3.0 |
| `geo` | `staging_nodes` | Where scooters wait | v0.3.0 |
| `geo` | `hubs` | Restock sites | v0.3.0 |
| `catalog` | `menu_items` | Items and current prices | v0.3.0 |
| `catalog` | `price_history` | Earlier prices | v0.3.0 |
| `catalog` | `availability_rules` | When items can be sold | v0.3.0 |
| `inventory` | `scooters` | Each scooter, and its current rider | v0.3.0 |
| `inventory` | `lots` | Labelled batches, with expiry and status | v0.3.0 |
| `inventory` | `scooter_stock` | Units per scooter and lot. Quantity can never go negative | v0.3.0 |
| `inventory` | `stock_ledger` | Append-only record of every stock change | v0.3.0 |
| `inventory` | `reservations` | Short holds on units | v0.4.0 |
| `orders` | `orders` | The order, its state, stage, timer values and both `seq` values | v0.4.0 |
| `orders` | `order_items` | Items, with the price fixed at creation | v0.4.0 |
| `orders` | `order_events` | Append-only true history of each order | v0.4.0 |
| `orders` | `availability_notifications` | "Notify me" requests | v0.6.0 |
| `payments` | `payments` | Razorpay orders and status | v0.4.0 |
| `payments` | `refunds` | Refunds, with reason and method | v0.4.0 |
| `payments` | `webhook_events` | Razorpay events already handled | v0.4.0 |
| `dispatch` | `offers` | Offers to riders, with fencing tokens | v0.5.0 |
| `dispatch` | `relocations` | Suggested moves | v0.7.0 |
| `tracking` | `shifts` | Rider shifts | v0.3.0 |
| `tracking` | `rider_events` | Results of queued offline events, by `event_id` | v0.5.0 |
| `tracking` | `location_pings` | Recent pings, kept as evidence. Partitioned by day, with short retention | v0.5.0 |
| `restock` | `restock_requests` | Requests and their states | v0.7.0 |
| `restock` | `restock_lines` | Items and filled quantities | v0.7.0 |
| `restock` | `shortfalls` | What a hub could not fill | v0.7.0 |
| `support` | `support_tickets` | Customer reports | v0.6.0 |
| `support` | `ticket_messages` | Messages on a ticket | v0.6.0 |
| `support` | `problem_reports` | Rider problem buttons | v0.6.0 |
| `support` | `earnings` | Per-order pay, incentives and paid repositioning time | v0.7.0 |

**Changes from the earlier lists:**
- Added `scooters`, `location_zones`, `zone_settings`, `hubs`, `scheduled_tasks`, `rider_events`, `devices`, `relocations`, `earnings`, `problem_reports` and `ticket_messages`, which the contracts need.
- `offers` belongs to `dispatch`, which writes it.
- `order_legs` waits for multi-scooter orders, which are P2. The pilot is single-leg.
- `hub_queue_slots` and hub staff tables wait for the partner API, in v0.8.0. Console staff, roles and approvals wait for v0.9.0.

## 2. What Redis holds
Redis is never the source of truth. If it is lost, positions refill from the next pings, and everything else comes from Postgres.

| Use | Key idea | Written by |
|---|---|---|
| Latest rider position | `rider:{id}` | `locationPing` |
| Riders by map cell | `cell:{h3}` | `locationPing` |
| Live stream fan-out | Pub/sub per order and per rider | Any change a stream reader can see |
| Rate limits | OTP and ping counters | `requestOtp`, `verifyOtp`, `locationPing` |
| Availability cache | Short-lived, per gate | `getAvailability` |

`locationPing` writes a ping to Postgres only as evidence. The zone check itself uses `location_zones` and `orders`, so a Redis failure cannot change a timer. If Redis is down, no new offers go out, because rider positions are unknown.

## 3. Customer API

| Endpoint | Reads | Writes | Redis | Outside calls |
|---|---|---|---|---|
| `requestOtp` | `customers` | `otp_requests` | Rate limit | SMS gateway |
| `verifyOtp` | `otp_requests` | `otp_requests`, `customers`, `refresh_tokens`, `audit_events` | Rate limit | |
| `refreshToken` | `refresh_tokens` | `refresh_tokens` | | |
| `getMe` | `customers` | | | |
| `updateMe` | `customers` | `customers`, `audit_events`, `idempotency_keys` | | |
| `registerDevice` | `devices` | `devices`, `idempotency_keys` | | |
| `removeDevice` | `devices` | `devices`, `idempotency_keys` | | |
| `listGates` | `gates` | | | |
| `createGate` | `zones`, `gates` | `gates`, `gate_history`, `audit_events`, `idempotency_keys` | | |
| `listMenuItems` | `menu_items` | | | |
| `getMenuItem` | `menu_items` | | | |
| `getAvailability` | `gates`, `zones`, `zone_settings`, `menu_items`, `availability_rules`, `scooter_stock`, `lots`, `scooters` | | Positions, cells, cache | Routing engine |
| `notifyWhenAvailable` | `gates` | `availability_notifications`, `idempotency_keys` | | |
| `listOrders` | `orders`, `order_items` | | | |
| `createOrder` | `gates`, `zones`, `zone_settings`, `menu_items`, `availability_rules`, `scooters`, `scooter_stock`, `lots` | `orders`, `order_items`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `payments`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Positions | Razorpay, routing engine |
| `getOrder` | `orders`, `order_items`, `payments`, `riders` | | | |
| `cancelOrder` | `orders`, `payments`, `reservations`, `offers` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `offers`, `refunds`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | Razorpay |
| `changeOrderGate` | `orders`, `gates`, `zones`, `zone_settings` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `payments`, `audit_events`, `idempotency_keys` | | Razorpay |
| `retryPayment` | `orders`, `payments`, `reservations` | `orders`, `order_events`, `reservations`, `scooter_stock`, `stock_ledger`, `payments`, `idempotency_keys` | Fan-out | Razorpay |
| `acceptLongerEta` | `orders`, `reservations` | `orders`, `order_events`, `scheduled_tasks`, `outbox_events`, `idempotency_keys` | Fan-out | |
| `startMaskedCall` | `orders`, `riders`, `customers` | `audit_events`, `idempotency_keys` | | Telephony |
| `getOrderRefund` | `orders`, `refunds` | | | |
| `createTicket` | `orders` | `support_tickets`, `ticket_messages`, `outbox_events`, `idempotency_keys` | | |
| `getTicket` | `support_tickets`, `ticket_messages` | | | |
| `razorpayWebhook` | `webhook_events`, `payments`, `orders`, `reservations` | `webhook_events`, `payments`, `orders`, `order_events`, `reservations`, `refunds`, `scheduled_tasks`, `outbox_events`, `audit_events` | Fan-out | Razorpay, for refunds |

## 4. Rider API

| Endpoint | Reads | Writes | Redis | Outside calls |
|---|---|---|---|---|
| `requestOtp` | `riders` | `otp_requests` | Rate limit | SMS gateway |
| `verifyOtp` | `otp_requests`, `riders` | `otp_requests`, `refresh_tokens`, `audit_events` | Rate limit | |
| `refreshToken` | `refresh_tokens` | `refresh_tokens` | | |
| `getRider` | `riders` | | | |
| `getRiderState` | `riders`, `shifts`, `offers`, `orders`, `order_items`, `restock_requests`, `restock_lines`, `shortfalls` | | | |
| `getVerification` | `rider_verifications` | | | |
| `startVerification` | `riders` | `rider_verifications`, `audit_events`, `idempotency_keys` | | DigiLocker, a mock for now |
| `registerRiderDevice` | `devices` | `devices`, `idempotency_keys` | | |
| `removeRiderDevice` | `devices` | `devices`, `idempotency_keys` | | |
| `startShift` | `riders`, `rider_verifications`, `shifts`, `staging_nodes` | `shifts`, `scooters`, `audit_events`, `idempotency_keys` | | |
| `endShift` | `shifts`, `orders` | `shifts`, `scooters`, `audit_events`, `idempotency_keys` | | |
| `listNodes` | `zones`, `staging_nodes`, `scooters`, `relocations` | | Positions | |
| `acceptRelocation` | `relocations` | `relocations`, `audit_events`, `idempotency_keys` | | |
| `scanStock` | `lots`, `scooters`, `scooter_stock`, `restock_requests` | `scooter_stock`, `stock_ledger`, `audit_events`, `idempotency_keys` | | |
| `getStock` | `scooters`, `scooter_stock`, `lots` | | | |
| `listOffers` | `offers`, `order_items`, `gates` | | | |
| `acceptOffer` | `offers`, `orders` | `offers`, `orders`, `order_events`, `scheduled_tasks`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | |
| `declineOffer` | `offers` | `offers`, `order_events`, `scheduled_tasks`, `audit_events`, `idempotency_keys` | Fan-out | |
| `markReady` | `offers`, `orders`, `zone_settings`, `location_pings` | `orders`, `order_events`, `outbox_events`, `audit_events`, `idempotency_keys` | Position, fan-out | |
| `getRiderOrder` | `orders`, `order_items`, `offers`, `gates` | | | |
| `getRoute` | `orders`, `gates` | | Position | Routing engine |
| `handOver` | `orders`, `order_items`, `location_pings`, `rider_events` | `orders`, `order_events`, `scooter_stock`, `stock_ledger`, `rider_events`, `refunds`, `outbox_events`, `audit_events`, `idempotency_keys` | Position, fan-out | Razorpay, for a partial refund |
| `startRiderMaskedCall` | `orders`, `customers` | `audit_events`, `idempotency_keys` | | Telephony |
| `locationPing` | `orders`, `location_zones`, `zone_settings` | `location_pings`, `orders`, `order_events`, `scheduled_tasks`, `outbox_events` | Position, cells, rate limit, fan-out | |
| `sendEventBatch` | `rider_events`, `orders`, `offers`, `order_items`, `restock_requests` | `rider_events`, `location_pings`, `orders`, `order_events`, `scooter_stock`, `stock_ledger`, `problem_reports`, `restock_requests`, `outbox_events`, `audit_events` | Fan-out | |
| `listRestockSuggestions` | `restock_requests`, `restock_lines`, `hubs` | | | |
| `confirmRestock` | `restock_requests` | `restock_requests`, `restock_lines`, `outbox_events`, `audit_events`, `idempotency_keys` | Fan-out | |
| `declineRestock` | `restock_requests` | `restock_requests`, `audit_events`, `idempotency_keys` | | |
| `createRestock` | `hubs`, `restock_requests` | `restock_requests`, `restock_lines`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `getRestockOptions` | `riders`, `hubs`, `menu_items` | | | |
| `respondToShortfall` | `shortfalls`, `restock_requests` | `shortfalls`, `restock_requests`, `audit_events`, `idempotency_keys` | Fan-out | |
| `getRestock` | `restock_requests`, `restock_lines`, `shortfalls`, `hubs` | | | |
| `reportProblem` | `orders` | `problem_reports`, `outbox_events`, `audit_events`, `idempotency_keys` | | |
| `getEarnings` | `earnings` | | | |

## 5. What must happen in one transaction

| Action | All of these commit together, or none do |
|---|---|
| `createOrder`, first part | The order and its `Idempotency-Key`, items, reservation, stock change and ledger entry. Razorpay is called after this commit. A retry finds the order and resumes |
| Webhook confirms payment | `webhook_events`, `payments`, the order's `cancel_until`, the reservation's new expiry, and the task that sends the offer after the window |
| `cancelOrder` | The state check under the order's row lock, the order, its event, stock release, a pending refund and the stored reply. The Razorpay refund runs afterwards, from a task, so it can retry |
| Starting the rider search | The same row lock as `cancelOrder`, so exactly one of them wins at the end of the window |
| `acceptOffer` | The offer's state check against its fencing token, the order and its event |
| Zone entry and perimeter | The timer value, written once by a conditional update, with the order's event and customer `seq` |
| `handOver` | OTP attempt count, the order, the final stock change and ledger entry, and the event result |

**No outside call ever runs inside a transaction.** Razorpay, SMS, routing and telephony run before or after, and retries are idempotent.

## 6. Rules the tables must enforce
These are checked in the database as well as in Go, so a bug cannot break them:
- `scooter_stock.qty` can never go negative.
- `orders` has a unique `Idempotency-Key` per customer, so one tap makes one order.
- `webhook_events` has a unique Razorpay event ID.
- `rider_events` has a unique `event_id` per rider.
- `refunds` allows one refund per reason per order, and money is always integer minor units.
- `stock_ledger`, `order_events` and `audit_events` are append-only.
- Sign-in codes and refresh tokens are stored only as hashes.
- Row Level Security is on for every table, with no policies, so the Supabase Data API can read nothing even if it is left on.

## 7. Open points
- **Retention:** how long to keep `location_pings`, and personal data. Proposed: 30 days for pings. It may need legal input.
- **Routing engine:** Google Routes or OSRM and Valhalla is still open. It affects `getAvailability`, `createOrder` and `getRoute`.
- **Hubs before v0.8.0:** `restock_requests` is built in v0.7.0, and a simulator stands in for hub actions.
- **Lots before v0.8.0:** nothing creates lots until the hub APIs exist, so they are seed data.
