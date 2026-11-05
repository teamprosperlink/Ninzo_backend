# EDD: customer-api

Version: 0.2.0 (draft for owner review) · Updated: 2026-11-04 · Source keys: see `PRD.md`

Engineering design for the customer order flow. The flow itself, its edge rules and its settings are in `docs/engineering/data_flow.md`, section 1. **Proposed** means not agreed. No code exists yet, so nothing here has been tested.

## 1. Design goals
1. The customer sees a promise only after the checks pass.
2. One server-owned clock.
3. Stock is never oversold.
4. Every write is safe to repeat.
5. Every failure ends in a known state, with stock and money settled.

## 2. Order state machine
One transition function in `internal/orders/domain`. Illegal transitions are rejected. Every transition appends to `order_events`.

```
CREATED -> PAYMENT_PENDING -> PAYMENT_SECURED -> OFFERED -> RIDER_ACCEPTED
RIDER_ACCEPTED -> PACKED -> TIMER_RUNNING -> ARRIVED -> HANDED_OVER -> COMPLETED
PACKED -> ARRIVED (rider already inside 100 m; no timer)
PACKED or TIMER_RUNNING -> HANDED_OVER (signal lost: a valid OTP records the arrival first)

CREATED, PAYMENT_PENDING or PAYMENT_FAILED -> CANCELLED (before payment is confirmed)
Before RIDER_ACCEPTED: PAYMENT_FAILED, NO_SCOOTER, TIMED_OUT
PAYMENT_FAILED -> PAYMENT_PENDING (retry) or TIMED_OUT
NO_SCOOTER -> OFFERED (customer takes a longer ETA, and only after `cancel_until`), CANCELLED, or TIMED_OUT (no choice in time, refunded automatically)
PAYMENT_SECURED -> CANCELLED (within 5 seconds of payment being confirmed)
PAYMENT_SECURED -> OFFERED (only after those 5 seconds)
OFFERED, RIDER_ACCEPTED or PACKED -> CANCELLED (after the maximum wait, before the timer starts)
From RIDER_ACCEPTED on, including a hidden ARRIVED: FAILED. During the minimum wait, a failure rematches silently, or ends as TIMED_OUT with no remedy and reason `no_rider_found`, so nothing reveals the rider
timer_missed is a flag on the order, not a state
```

**Why these differ from the sources:**
- `TIMER_RUNNING` and `ARRIVED` come from [decision 0007](../../../docs/decisions/0007-zone-timer-order-flow.md). They replace `ACCEPTED`, `EN_ROUTE` and the KT's `accepted_at`.
- `PAYMENT_SECURED` replaces `PAYMENT_AUTHORIZED`, because payment is charged up front.
- A rider is contacted only after the webhook confirms, not at creation as ARCH says. This means no rider makes a wasted trip for a failed payment. **Proposed.**
- A missed timer is a flag, because the order must still reach `COMPLETED`. The customer can still receive the food.

**Customer stage.** The stream sends a `customer_stage` separate from the order state: `payment`, `payment_failed`, `looking_for_rider`, `no_scooter`, `rider_accepted`, `timer_running`, `arrived`, `handed_over`, `ended`. `looking_for_rider` holds until the rider has accepted and the minimum wait has passed. The customer API never returns the order state or `rider_accepted_at`, because they would contradict the screen. `order_events` always keeps the true times. The mapping is in `contracts/README.md`.

## 3. Main flow by module

| Step | What happens | Owner module |
|---|---|---|
| 1 | `POST /v1/orders`: check the gate, zone switch and gate cap | `geo`, `orders` |
| 2 | Availability: online scooters with stock, not on restock, whose ETA fits the maximum wait | `dispatch`, `geo`, `inventory` |
| 3 | Reserve stock in one transaction: `FOR UPDATE SKIP LOCKED` on stock rows, a reservation row, a ledger entry, an outbox event, and the order with its `Idempotency-Key` as a unique value | `inventory`, `orders` |
| 4 | Create the Razorpay order and return it. A retry after a crash finds the order from step 3 and resumes here | `payments` |
| 5 | Webhook confirms: set `cancel_until`, and extend the reservation (section 5). A due row in Postgres sends the offer after `cancel_until`, never before. A webhook on an ended order refunds automatically | `payments`, `orders`, `dispatch` |
| 6 | Rider accepts: record `rider_accepted_at`, and keep the reservation from expiring. When the customer stage reaches `rider_accepted`: commit the reservation | `orders`, `inventory` |
| 7 | Ready pressed or packing limit passes: move to `PACKED` | `orders` |
| 8 | `tracking` confirms the rider is inside the zone: if packed and the minimum wait has passed, start the timer | `tracking`, `orders` |
| 9 | `tracking` confirms the rider is inside 100 m: stop the timer, move to `ARRIVED` | `tracking`, `orders` |
| 10 | Rider submits OTP and quantities. Stock is decremented for good | `orders`, `inventory` |

**Payment:** charged up front at step 4 and confirmed by the webhook at step 5. Every refund is one idempotent call, retried by the worker. If it keeps failing, it raises an ops alert. A provider call never sits inside a database transaction.

**Candidate selection.** Rank by p90 ETA, then longest idle. Offer to the top 1-2 (EDGE). The 55 s sweet spot no longer gates the offer, but it is still used to place scooters at staging nodes.

## 4. The timer
- `timer_started_at`, `timer_duration_s` and `timer_stopped_at` are written once, by the server. They are not signed, because apps cannot check a signature without the secret key. **Proposed,** to revisit for offline events.
- The start and stop are triggered by GPS only, and confirmed by two consecutive pings after speed checks.
- Pings run every 2-3 s from packed to arrived, and slower otherwise. At 20-25 km/h a rider moves about 11-21 m between pings, so a two-ping check adds up to about 6 s of delay.
- The customer's countdown is `timer_started_at + timer_duration_s`, corrected for phone clock offset.
- The rider sees a fixed label built from the timer length: 90 s shows "1.5 mins", 120 s shows "2 mins". **Assumed:** a fixed label, not a ticking countdown.

**Budget check.** With a 350 m zone and a 100 m perimeter, the rider covers 250 m under the timer: 36-45 s straight, or 50-63 s if roads add 40% (an assumption, to be replaced with trial data). A long red signal can use up the rest of the margin, which is why the radius varies by location.

## 5. Timers and reservations
Reservation TTL must outlast the payment window and the wait for a rider.

| Stage | Reservation expires at |
|---|---|
| At creation | now + payment window |
| After the webhook confirms | now + cancel window + maximum offer wait + safety buffer |
| After the rider accepts | No expiry. Committed when the customer stage reaches `rider_accepted`, or released on cancel. A later cancel after the maximum wait reverses it with a ledger entry |

All values are settings, not constants. Expiry runs from rows in Postgres, so a restart loses nothing.

**Late webhook.** Payment can succeed after the reservation has expired. The handler tries to reserve again. If that fails, it moves to `TIMED_OUT` and refunds automatically. The customer is never left paid with no order.

**Rematch.** A reservation is tied to one scooter. Re-offering to a different scooter means release and reserve in one transaction. If no stock can be reserved, the order moves to `NO_SCOOTER`.

## 6. Concurrency and safety

| Risk | Control |
|---|---|
| Double tap, retry | `Idempotency-Key`: store key, actor, route, request hash and response for 24 h |
| Duplicate webhook | Unique Razorpay event ID in `webhook_events`, checked in the same transaction as its effect |
| Timer written twice | Conditional update (`WHERE timer_started_at IS NULL`) |
| Stale rider device | Fencing token on the offer |
| Oversell | Row locks and a `CHECK` that quantity cannot go negative |
| Console change during an order | The order keeps its own timer length and radius |
| Audit gaps | Audit row in the same transaction as the change |
| Lost events | Outbox in the same transaction ([0003](../../../docs/decisions/0003-redis-and-postgres-outbox.md)) |

Outbox polling must not carry time-critical steps. Offers and zone events run directly. Notifications and analytics can use the outbox.

## 7. Live channel
- `WS /v1/orders/{id}/stream`, with the token in the first frame.
- Events carry `seq`, `customer_stage`, the timer fields, `eta_source` and `server_time`. `seq` is the order's customer-visible version, so a lost Redis message shows up as a gap, and a `heartbeat` reveals a lost last message. See `contracts/README.md`. Clients re-fetch state after reconnect, so `GET /v1/orders/{id}` is always authoritative and cheap.
- The rider's position streams from when the customer sees "rider has accepted", and keeps going after the timer stops, until handover.
- Fan-out through Redis pub/sub. If Redis fails, the stream degrades, but order correctness does not.
- **Proposed:** the stream runs inside `customer-api` for the pilot, using the `realtime` module.

## 8. Modules used

| Module | Used for |
|---|---|
| `platform` | Config, database, logs, telemetry, idempotency, middleware |
| `identity` | OTP, tokens |
| `geo` | Gates, zones, polygons, radius lookup, H3, console settings in `zone_settings` |
| `catalog` | Menu, prices |
| `inventory` | Stock ledger, reservations |
| `orders` | State machine, events, timer fields |
| `payments` | Razorpay, webhooks, refunds |
| `dispatch` | Candidates, offers |
| `tracking` | Pings, smoothing, zone events |
| `audit`, `notify`, `realtime`, `support` | As named |

## 9. Unresolved design issues

### 9.1 Where the timer stops
Resolved by decision 0007: the 100 m perimeter.

### 9.2 Handover OTP
CUST and the rider doc both submit an OTP, and nothing gives the customer one to show. **Proposed (status known gaps):** the server issues the OTP, the customer app displays it, and the rider submits it with quantities. `handover-confirm` is removed from the customer API.

### 9.3 Late location from a dead zone
The server only knows what it has received. If a rider enters the perimeter with no signal, the timer could end first and show a false miss. **Proposed:** official times are server receipt times. A late event with a device time inside the timer and a plausible GPS track goes to audit review before any remedy, with a time limit, so the customer is not left waiting. Status open question 8.

### 9.4 Offline OTP check
Rider R9 requires handover to work offline. A salted hash on the device was rejected: review cracked a 6-digit OTP from one in under a second, which would let a rider fake a delivery. **Proposed:** the app queues the typed OTP and the server checks it on sync. A wrong OTP after the food was given alerts support. See Rider offline events in `contracts/README.md`.

### 9.5 Payment
Decided by the owner on 2026-11-04: charge up front and refund automatically, with no holds. The `payments` surface has `Charge` and `Refund`. Refunds take days to reach the customer, so the app tells them why, how and by when.

### 9.6 Other known gaps
- Missing endpoints: `project_spec.md` section 3.
- Customer no-show: no refund, **Proposed.** The wait window is not set (EDGE).
- Refund amount rules, and who approves above a limit (PLAN question 6).
- Google Maps terms may limit storing Google-derived data. To be checked before any is stored, and this also affects the routing engine choice.

## 10. Test plan
Written before the code they cover (PLAN section 10). Run against Docker Postgres with PostGIS.

| Test | Pass condition |
|---|---|
| 50 parallel orders for the last unit | One winner, no negative stock |
| Double tap, webhook, accept, Ready, handover | One effect each |
| Stale rider accept after re-offer | Rejected |
| Payment fails after reserve | Stock released, no rider contacted |
| Webhook after reservation expiry | Re-reserved, or automatic refund |
| Webhook on an order already cancelled | Automatic refund, reason `customer_cancelled` |
| Cancel at 4.9 seconds racing the rider search | Exactly one wins, under the row lock |
| Rider drops before accepting | Release and rematch |
| Rider packed, enters zone | Timer starts once, after two pings |
| Rider enters zone, not yet packed | Timer waits for Ready or the packing limit |
| Rider already in zone | Shorter wait above zero, then timer starts when packed and the wait has passed |
| Customer cancels within 5 seconds of payment | Full refund, stock released, no rider contacted |
| Customer cancels after 5 seconds, before `wait_deadline` | `cancel_not_allowed`, the same whether or not a rider accepted |
| Failure on Ninzo's side, at any stage | Automatic refund, with reason and method |
| Customer cancels after the maximum wait, at `rider_accepted` | Cancel succeeds, stock reversed in the ledger, payment refunded, rider told |
| `POST /v1/orders` crashes after reserving, then retries | One order, one reservation |
| Customer does nothing at `no_scooter` | Order times out, stock released, payment refunded |
| Hidden change during the wait | Customer `seq` does not rise |
| Last stream message lost | Next `heartbeat` shows a higher `seq`, and the client fetches state |
| During the wait | No push, SMS, position, charge or API field reveals the rider |
| Rider reaches 100 m during the wait | No timer. `rider_accepted`, then `arrived` |
| Minimum wait passes with no state change | `seq` rises, and a stage event is sent |
| Rider already inside 100 m when packed | No timer, straight to arrived |
| Single GPS jump into the zone or perimeter | No timer change |
| Enters 100 m | Timer stops once, map keeps streaming |
| Timer ends before 100 m | Flag set, remedy and audit record, no staff action, order continues |
| Signal lost during the timer | No remedy until location returns or the limit passes |
| Console change during a running timer | That order's timer is unchanged |
| Replayed location event | Official times unchanged |
| Zone switched off during an order | No new timers. Behaviour for orders in flight to be specified |
| Contract | Schemathesis against the OpenAPI file |
| Socket drop | Re-fetch returns the true state |
