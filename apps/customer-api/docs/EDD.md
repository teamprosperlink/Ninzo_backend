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

Before RIDER_ACCEPTED: CANCELLED, PAYMENT_FAILED, NO_SCOOTER, TIMED_OUT
From RIDER_ACCEPTED on: FAILED
timer_missed is a flag on the order, not a state
```

**Why these differ from the sources:**
- `TIMER_RUNNING` and `ARRIVED` come from [decision 0007](../../../docs/decisions/0007-zone-timer-order-flow.md). They replace `ACCEPTED`, `EN_ROUTE` and the KT's `accepted_at`.
- `PAYMENT_SECURED` replaces `PAYMENT_AUTHORIZED`, so the name holds for either result of Q5. **Proposed.**
- A rider is contacted only after the webhook confirms, not at creation as ARCH says. This means no rider makes a wasted trip for a failed payment. **Proposed.**
- A missed timer is a flag, because the order must still reach `COMPLETED`. The customer can still receive the food.

**Customer stage.** The stream sends a `customer_stage` separate from the order state: `payment`, `looking_for_rider`, `rider_accepted`, `timer_running`, `arrived`, `handed_over`. `looking_for_rider` holds until the rider has accepted and the minimum wait has passed. `order_events` always keeps the true times.

## 3. Main flow by module

| Step | What happens | Owner module |
|---|---|---|
| 1 | `POST /v1/orders`: check the gate, zone switch and gate cap | `geo`, `ops` |
| 2 | Availability: online scooters with stock, not on restock, whose ETA fits the maximum wait | `dispatch`, `geo`, `inventory` |
| 3 | Reserve stock in one transaction: `FOR UPDATE SKIP LOCKED` on stock rows, a reservation row, a ledger entry and an outbox event | `inventory` |
| 4 | Create the Razorpay order and return it | `payments` |
| 5 | Webhook confirms. Extend the reservation (section 5). Send the offer | `payments`, `orders`, `dispatch` |
| 6 | Rider accepts: commit the reservation, record `rider_accepted_at`. Settle payment afterwards by idempotent retry | `orders`, `inventory`, `payments` |
| 7 | Ready pressed or packing limit passes: move to `PACKED` | `orders` |
| 8 | `tracking` confirms the rider is inside the zone: if packed, start the timer | `tracking`, `orders` |
| 9 | `tracking` confirms the rider is inside 100 m: stop the timer, move to `ARRIVED` | `tracking`, `orders` |
| 10 | Rider submits OTP and quantities. Stock is decremented for good | `orders`, `inventory` |

**Why step 6 settles after the commit:** a payment provider call must not sit inside a database transaction. If settlement fails, the worker retries. If it keeps failing, it raises an ops alert, and the food is not withheld. **Proposed.**

**Candidate selection.** Rank by p90 ETA, then longest idle. Offer to the top 1-2 (EDGE). The 55 s sweet spot no longer gates the offer, but it is still used to place scooters at staging nodes.

## 4. The timer
- `timer_started_at`, `timer_duration_s` and `timer_stopped_at` are written once, by the server, and signed when sent to devices (ARCH).
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
| After the webhook confirms | now + maximum offer wait + safety buffer |
| After the rider accepts | Committed, no expiry |

All values are settings, not constants. Expiry runs from rows in Postgres, so a restart loses nothing.

**Late webhook.** Payment can succeed after the reservation has expired. The handler tries to reserve again. If that fails, it moves to `TIMED_OUT` and voids or refunds automatically. The customer is never left paid with no order.

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
- Events carry `seq`, `customer_stage`, the timer fields, `eta_source` and `server_time`. Clients re-fetch state after reconnect, so `GET /v1/orders/{id}` is always authoritative and cheap.
- The rider's position keeps streaming after the timer stops, until handover.
- Fan-out through Redis pub/sub. If Redis fails, the stream degrades, but order correctness does not.
- **Proposed:** the stream runs inside `customer-api` for the pilot, using the `realtime` module.

## 8. Modules used

| Module | Used for |
|---|---|
| `platform` | Config, database, logs, telemetry, idempotency, middleware |
| `identity` | OTP, tokens |
| `geo` | Gates, zones, polygons, radius lookup, H3 |
| `catalog` | Menu, prices |
| `inventory` | Stock ledger, reservations |
| `orders` | State machine, events, timer fields |
| `payments` | Razorpay, webhooks, refunds |
| `dispatch` | Candidates, offers |
| `tracking` | Pings, smoothing, zone events |
| `ops` | Console settings |
| `audit`, `notify`, `realtime`, `support` | As named |

## 9. Unresolved design issues

### 9.1 Where the timer stops
Resolved by decision 0007: the 100 m perimeter.

### 9.2 Handover OTP
CUST and the rider doc both submit an OTP, and nothing gives the customer one to show. **Proposed (status known gaps):** the server issues the OTP, the customer app displays it, and the rider submits it with quantities. `handover-confirm` is removed from the customer API.

### 9.3 Late location from a dead zone
The server only knows what it has received. If a rider enters the perimeter with no signal, the timer could end first and show a false miss. **Proposed:** official times are server receipt times. A late event with a device time inside the timer and a plausible GPS track goes to audit review before any remedy, with a time limit, so the customer is not left waiting. Status open question 8.

### 9.4 Offline OTP check
Rider R9 requires handover to work offline. An offline rider app cannot check the OTP unless it can verify it locally. **Proposed:** the server sends the rider app a salted hash of the OTP for that order at arrival. The rider app checks it locally, and the server re-checks on sync. Needs a rider-side review.

### 9.5 Payment holds (Q5)
The design hides the difference behind the `payments` surface: `Secure`, `Settle` and `Release`.

| Action | With a hold | With charge at order |
|---|---|---|
| `Secure` | Authorise | Capture |
| `Settle` at rider accepted | Capture | No-op |
| `Release` on cancel or failure | Void | Refund |

### 9.6 Other known gaps
- Missing endpoints: `project_spec.md` section 3.
- Customer no-show: wait window and refund policy values are not set (EDGE).
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
| Webhook after reservation expiry | Re-reserved, or automatic void or refund |
| Rider drops before accepting | Release and rematch |
| Rider packed, enters zone | Timer starts once, after two pings |
| Rider enters zone, not yet packed | Timer waits for Ready or the packing limit |
| Rider already in zone | Shorter wait above zero, then timer starts when packed |
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
