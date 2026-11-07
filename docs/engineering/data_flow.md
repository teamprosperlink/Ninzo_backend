# Data flow: ninzo-backend

Version: 0.1.0 · Updated: 2026-11-04

Nothing here is built or tested yet.

## 1. Main flow: order to arrival
Owner decision, recorded in [decision 0007](../decisions/0007-zone-timer-order-flow.md). This overrides the KT docs where they differ.

- **Trigger:** a customer creates an order at a saved gate.
- **Promise:** the rider reaches the customer's location within the timer. Handover is not part of the promise.

| # | Trigger | Rider | Customer sees | Backend |
|---|---|---|---|---|
| 1 | Order created | Not told yet | Payment | Checks stock, rider and gate. Reserves stock on the pre-selected rider's scooter. Creates the Razorpay order. Payment is charged up front |
| 2 | Payment webhook confirms | Gets the offer once the 5-second cancel window has passed | "Looking for riders" | Moves to `OFFERED` after the window, with a fencing token |
| 3 | Rider accepts | Stops and packs | Still "looking for riders" until the minimum wait passes, then "rider has accepted" | Records `rider_accepted_at`, and fixes the timer length and radius on the order. When the customer sees "rider has accepted": commits the stock |
| 4 | Ready pressed, or packing limit passes | Rides if outside the zone. Sees "1.5 mins" | Live map, once the customer sees "rider has accepted" | Records packed time. Pings every 2-3 s |
| 5 | Packed, inside the zone (2 pings), and minimum wait passed | Is not asked to do anything | Timer starts | Records `timer_started_at` |
| 6 | Inside the 100 m perimeter (2 pings) | Checks doors | Timer off, "Rider has arrived at your location", map stays live | Records `timer_stopped_at`. Promise met |
| 7 | Rider submits OTP and quantities | Hands over | OTP screen | Records handover. Stock is decremented for good |

### Rules at each edge
- **Rider already in the zone at step 2:** the "looking for riders" wait uses the shorter setting. It is never zero. The timer starts as soon as the rider is packed and the wait has passed.
- **Rider already inside 100 m when packed:** no timer. Go straight to step 6.
- **Minimum wait (Proposed):** counted from when payment is confirmed. If the rider reaches 100 m during it, there is no timer: the customer sees "rider has accepted" when it ends, and then "arrived".
- **Cancels:** a full refund within 5 seconds of payment being confirmed. After that, no cancel and no refund, until `wait_deadline` passes. See Cancels and refunds in `contracts/README.md`.
- **Failures on Ninzo's side are always refunded,** and the customer is told why and how.
- **Nothing reveals the rider early:** until the customer sees "rider has accepted", no API field, push, SMS, map position or charge shows that a rider is allotted. See `contracts/README.md`.
- **Rider ignores or refuses an offer:** move to the next rider before acceptance, with no penalty. If stock cannot be reserved on another scooter, the order moves to `NO_SCOOTER`.
- **Timer ends before 100 m:** the customer sees a calm message, and the map stays live. The order continues, and the remedy fires automatically.
- **Signal lost during the timer:** the server timer keeps running. No remedy fires until location returns or a limit passes. Official times are server receipt times (status open question 8).
- **No scooter available:** the customer can take a longer ETA or cancel. If they do neither in time, the order times out and the payment is refunded.
- **Maximum wait passes before the timer:** the customer can leave with a full refund, or keep waiting. Stock is returned.
- **GPS jump:** implausible speeds are dropped, and two consecutive pings are needed before a zone event counts.

### Settings
Stored in the database, changed in the admin console, and audited. Until the console exists they are seeded. Running orders keep the values they started with.

| Setting | Scope | Starting value |
|---|---|---|
| Timer length | Zone | 90 s |
| Zone radius | Location, then zone, then global | 350 m |
| Arrival perimeter | Zone | 100 m |
| Minimum "looking for riders" wait | Zone | Not set |
| Shorter wait for a rider already in the zone, above zero | Zone | Not set |
| Packing limit | Zone | Not set |
| Maximum wait before the timer | Zone | 5 minutes |
| Time to choose after no scooter, before an automatic refund | Zone | Not set |
| Cancel window after payment is confirmed | Zone | 5 seconds |
| Remedy if the timer ends before 100 m | Zone | Credit, amount not set |
| Release the handover code after no ping | Zone | 30 s |
| Reassign the order after no ping | Zone | 2 minutes (decision 0011) |
| Sweet spot travel budget | Zone | 80 s, until the road test in Bengaluru |
| Straight-line speed, until real road times | Zone | 15 km/h, a placeholder |
| Widest rider search, past which the order is refused before payment | Zone | 1,500 m, a placeholder |

The names, units and allowed ranges are in `internal/geo/settings.go`. Until the console exists, `task settings` changes them locally, and `task staging:settings` on staging, which shows the target and asks first, with a reason and a name. Each change is a new version and is audited.

The location-based radius algorithm, written later by the owner, will supply the location value. It plugs in behind the same lookup, so the order flow does not change.

Locations and polygons are pre-built and mapped through Google Maps. At runtime, a zone event is only a point-in-polygon check, and a radius is stored as a circular polygon.

### Idempotency and ordering
- Every repeatable write carries `Idempotency-Key`.
- Timer start and stop are single conditional updates, so each is written once.
- True event times are always stored in `order_events`, whatever stage the customer is shown.

## 2. Data stores

| Store | Holds | Notes |
|---|---|---|
| Postgres | Orders, stock, payments, settings, audit, polygons | System of record |
| Redis | Latest rider positions, fan-out to WebSockets | Can be rebuilt. Never the source of truth for stock or orders |

Retention is not set yet.

## 3. Events and messages

| Name | Producer | Consumers | Delivery |
|---|---|---|---|
| Order state changes | `orders` | `notify`, `realtime`, `audit` | Outbox, at least once |
| Zone entered, perimeter entered | `tracking` | `orders` | Direct call, not the outbox, because the timer is time-critical |
| Stock changes | `inventory` | Availability cache | Outbox, at least once |

## 4. External systems

| Name | Purpose | Direction | If it fails |
|---|---|---|---|
| Razorpay | Payments | Both, webhook in | No new paid orders. Webhooks reconcile later |
| Google Maps | Location mapping | Out | Pre-built data keeps working |
| Firebase Cloud Messaging | Push | Out | Retry. The app fetches true state when opened |
| SMS gateway | OTP, updates | Out | Sign-in is blocked until it returns |
