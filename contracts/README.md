# API contracts

The contract comes first, and code follows it. Every customer and rider endpoint follows these rules. Shared schemas are in `shared/common.yaml`.

Check them with `task verify`. `task contracts:lint` checks that the files are valid. `tests/contracts` enforces the rules it can: `server_time`, the error shape, `Idempotency-Key`, sign-in, cursor paging, and no hidden fields in the customer contract. The other rules below are checked by review.

| Contract | Files |
|---|---|
| Customer | `customer/openapi.yaml`, `customer/stream.asyncapi.yaml` |
| Rider | `rider/openapi.yaml`, `rider/stream.asyncapi.yaml` |

Values marked **Proposed** are engineering defaults that the owner has not reviewed.

## General
- All paths are under `/v1`.
- JSON field names are `snake_case`.
- Times are UTC, in RFC 3339 format, and set by the server.
- Money is an integer in minor units with a currency. Never a decimal.
- IDs are opaque strings.
- Every JSON response object has `server_time` at the top level.
- Clients must ignore fields they do not know, so fields can be added without breaking them.

## Errors
Every error uses one shape:

```json
{
  "code": "token_expired",
  "message": "The access token has expired.",
  "retryable": false,
  "server_time": "2026-11-04T10:15:30.250Z"
}
```

- Apps act on `code` and `retryable`, never on `message`.
- A 403 also carries `missing_permission`.
- `code` is an open list, because contracts add their own. If an app does not know a `code`, it follows `retryable`.

Shared codes:

| Status | Code | Retryable |
|---|---|---|
| 400 | `invalid_request`, `idempotency_key_required` | No |
| 401 | `unauthorized`, `token_expired` | No. On `token_expired`, refresh and send again |
| 403 | `forbidden` | No |
| 404 | `not_found` | No |
| 409 | `conflict` | No |
| 409 | `idempotency_in_progress` | Yes, after `Retry-After` |
| 422 | `idempotency_key_reused` | No |
| 429 | `rate_limited` | Yes, after `Retry-After` |
| 500 | `internal_error` | Yes |
| 503 | `service_unavailable` | Yes, after `Retry-After` |

## Idempotency
Every `POST`, `PUT`, `PATCH` and `DELETE` requires an `Idempotency-Key` header. These are the only exceptions:

| Endpoint | Why |
|---|---|
| OTP request, OTP verify, token refresh | Limited by rate and by single-use codes |
| Location ping | Pings are not repeated actions |
| Offline event batch | Each event has its own `event_id` |
| Razorpay webhook | Deduplicated by Razorpay's event ID |

- A key is unique per intended action. A retry sends the same key.
- A key is kept for 24 hours, per signed-in user, method and exact path.
- The stored response is saved in the same transaction as the action's effect.

| Case | Result |
|---|---|
| Key is missing | 400 `idempotency_key_required` |
| Same key, same body, earlier result stored | The stored response, with `Idempotency-Replayed: true` |
| Same key, same body, first request still running | 409 `idempotency_in_progress`, with `Retry-After` |
| Same key, different body | 422 `idempotency_key_reused` |

**What gets stored:** only `2xx` responses. A rejected request had no effect, so a retry with the same key is checked again against the current state. For example, a cancel rejected with `cancel_not_allowed` can succeed once the maximum wait has passed.

**When to change the key:** keep the same key after a timeout, a network error, a `5xx`, a 401 or 429, or `idempotency_in_progress`, including when the user taps again, because the first request may have worked. Use a new key only after a final `4xx` rejection, or for a new action.

**After a crash:** a running request holds its key for 30 seconds (**Proposed**), and then a retry runs again. An endpoint with several steps must be able to resume, because some may already be committed. For example, `POST /v1/orders` saves the key with the order, so a retry finds that order and continues instead of reserving stock twice.

## Lists
- Requests take `cursor` and `limit` (default 20, maximum 100).
- Responses have `next_cursor`, which is `null` on the last page.
- Cursors are opaque. There is no offset paging.

## Sign-in
- Requests send `Authorization: Bearer <access_token>`, except OTP, refresh and webhook endpoints.
- Access tokens last 15 minutes by default.
- Refresh tokens are single use. Each refresh returns a new pair.
- A client sends only one refresh at a time. Other requests wait for its result.
- If a used token comes back and the pair issued from it has not been used, the response was probably lost, so the server returns a valid pair (**Proposed**).
- If that pair has been used, the token may have been stolen, so all that user's tokens are revoked and the user signs in again (**Proposed**).

## Live streams
1. The client connects and sends `{"type": "auth", "token": "<access_token>"}` as the first frame, within 5 seconds (**Proposed**).
2. The server replies with an `auth_ok` event, and then sends events.
3. Every event has `type`, `server_time` and `data`. State changes also have `seq`.
4. Before the token expires, the client sends a new `auth` frame on the same connection. If it does not, the server closes with `4401` at expiry.

**`seq`** is the version of what that stream's reader can see, and `GET` returns the current value. It is saved with each change.

- On the customer stream, it is per order, and rises only when something the customer can see changes: the stage, a timer field, or a contract field the customer receives. It does not rise on hidden changes, such as a rider accepting during the wait, because a rising `seq` would reveal them.
- On the rider stream, it is per rider, and rises with each change the rider can see.

**Heartbeat:** the server sends a `heartbeat` event with the current `seq` every 15 seconds (**Proposed**), so a lost last message still shows up. If no event arrives for 45 seconds, the client treats the connection as dropped.

| What the client sees | What it does |
|---|---|
| `seq` is at or below what it has | Ignores the event |
| `seq` is exactly one higher | Applies the event |
| `seq` skips a number, or a `heartbeat` has a higher `seq` | Fetches current state |
| Connection drops | Reconnects with a growing delay, and fetches current state before showing anything |
| Event has no `seq`, such as rider position | Applies it. Passing updates can be lost, and that is fine |

The stream never decides state. `GET` is the authority.

**Close codes:**

| Code | Meaning | What the client does |
|---|---|---|
| `4401` | Token missing, late, invalid or expired | Refreshes once and reconnects. If that fails, signs in again |
| `4403` | Not allowed on this stream | Does not reconnect |
| `4404` | The stream's subject does not exist | Does not reconnect |
| `4410` | The order has ended | Does not reconnect. Fetches the final state |
| Anything else | A network or server problem | Reconnects with a growing delay |

## Order states and customer stages
`OrderState` is the true state. Only rider and console contracts expose it.

The customer is shown "looking for riders" while a rider is already allotted. So until the customer's stage reaches `rider_accepted`, nothing the customer can see may reveal that rider. That covers the customer contract, push and SMS, and timing they can observe:

- The customer contract never returns `OrderState`, `OrderInternalTimes` or anything else showing the true state.
- The rider's identity and position are not sent before `rider_accepted`.
- Push and SMS follow `customer_stage`, never the order state.
- Payment is settled when the stage reaches `rider_accepted`, because a charge would reveal an earlier accept.
- A cancel during the wait succeeds as free, whatever the rider has done by then. The rider is told the offer was cancelled.
- `eta_source` stays `placeholder` and `eta_s` stays `null` until `rider_accepted`, and `wait_deadline` does not change because a rider accepts.
- If the rider breaks down or drops out during the wait, the server rematches silently. If the order must end, it ends the same way as finding no rider: `ended_reason` is `timed_out`, the customer is not charged or is refunded, and there is no `remedy`, because a promise was never shown.
- A cancel after the maximum wait is also free. At `looking_for_rider`, it works like any other cancel during the wait. At `rider_accepted`, the stock is already committed and the payment settled, so the stock is returned to the scooter's count, and the payment is refunded.

True times are always kept in `order_events`.

| Order state | `customer_stage` |
|---|---|
| `CREATED`, `PAYMENT_PENDING` | `payment` |
| `PAYMENT_FAILED` | `payment_failed`. The customer can retry |
| `PAYMENT_SECURED`, `OFFERED` | `looking_for_rider` |
| `NO_SCOOTER` | `no_scooter`. The customer can take a longer ETA, or cancel. If they do neither in time, the order times out and is refunded |
| `RIDER_ACCEPTED`, `PACKED` | `looking_for_rider` until the minimum wait passes, then `rider_accepted` |
| `TIMER_RUNNING` | `timer_running` |
| `ARRIVED` | `arrived`, or `looking_for_rider` if the minimum wait has not passed |
| `HANDED_OVER`, `COMPLETED` | `handed_over` |
| `CANCELLED`, `TIMED_OUT`, `FAILED` | `ended`, which is final. The customer contract adds a reason field |

**The minimum wait (Proposed):**
- It is counted from when payment is confirmed, and its length is a console setting.
- When it passes, the server saves a stage change, which raises `seq` and sends an event, even if the order state did not change.
- The timer starts only after it has passed. So the rider must be packed, inside the zone, and past the wait.
- If the rider reaches the arrival perimeter during the wait, there is no timer. The server sends `rider_accepted`, and then `arrived`.

`seq` decides which update is newest, not the order of stages. An app that reconnects can miss a stage, and `no_scooter` can go back to `looking_for_rider`.

The handover OTP is sent to the customer at `arrived`, or in the signal-lost case under Rider offline events. Neither can happen during the wait, because the signal-lost case needs `rider_accepted`.

### What the customer order carries at each stage
These fields are always present, and are `null` when not listed as set. Fields not shown here, such as `timer`, follow their own rules.

| Stage | Set | `cancellable` |
|---|---|---|
| `payment` | `payment_request` | `true` |
| `payment_failed` | Nothing extra. Use payment retry | `true` |
| `looking_for_rider` | `wait_deadline`: payment confirmed plus the maximum wait, or the agreed time after a longer ETA | `true` |
| `no_scooter` | `wait_deadline`: the time to choose before an automatic refund. `eta_offer` if a longer ETA is on offer | `true` |
| `rider_accepted` | `rider`, `wait_deadline`, and `eta_s` if `eta_source` is `measured` | `false`, or `true` once `wait_deadline` has passed |
| `timer_running` | `rider`, and `eta_s` if `eta_source` is `measured` | `false` |
| `arrived` | `rider`, `handover_otp` | `false` |
| `handed_over` | `rider` | `false` |
| `ended` | `ended_reason`, and `rider` if one was shown | `false` |

`handover_otp` is also set in the signal-lost case under Rider offline events. `remedy` can be set at `handed_over` or `ended`, or from the moment a timer is missed. A longer ETA replaces `wait_deadline`, so the customer always has one clear time to wait for.

## Timer
`OrderTimer` has `timer_started_at`, `timer_duration_s`, `timer_stopped_at` and `timer_missed`. `rider_accepted_at` is in `OrderInternalTimes`.

- Only the server sets these.
- `timer_started_at`, `timer_duration_s` and `timer_stopped_at` are written once. `timer_missed` can change from `false` to `true`, once.
- The countdown ends at `timer_started_at + timer_duration_s`, corrected for phone clock offset. Apps never start a timer on their own.
- An order's timer length and zone radius are fixed when the rider accepts. Later console changes do not affect it.
- A rider already inside the arrival perimeter has no timer, so `timer_started_at` stays `null`.
- `eta_source` says whether to show a time. Show one only for `measured`.
- Times are not signed, because apps cannot check a signature without the secret key. The server's values are official (**Proposed**).

## Rider view
- The rider sees `OrderState`, because it is the rider's own work.
- The rider sees `timer_label`, such as "1.5 mins": a fixed label built from the order's timer length, available from the offer being accepted. The rider contract has no countdown fields, so the clock never pressures the rider. The app must not build a countdown from `TIMER_RUNNING` either.
- Arrival comes from GPS, so there is no arrive endpoint. Pressing Ready never starts the timer by itself.
- `next_ping_s` on each location response tells the app how often to ping: about every 2 to 3 seconds from packed to arrived, and slower otherwise. Only pings with different `device_time` values count towards the two-ping check.
- The rider never sees the customer's phone number or the handover OTP. The customer shows the OTP, and the rider types it. The app holds nothing that can check an OTP, because a short code can be cracked on the device in under a second. Only the server checks it.
- A rider who no longer has an order, for example after a rematch, sees `ended_reason` and never another rider's progress.

### Rider offline events
Every action is queued first with a unique `event_id`. A direct request uses that `event_id` as its `Idempotency-Key`. For a rider's actions that can be queued, the server tracks the key per rider, not per path, so a direct request and its queued copy count as one action, and a difference in their bodies does not cause a 422.

- Events apply in order, and each is checked on its own. The batch itself fails only if the body cannot be read.
- A repeated `event_id` returns the stored result if that result was final: accepted, or rejected with `retryable: false`. A rejection with `retryable: true` is not stored, so the event is checked again.
- `retryable` on a rejected event means the same event may succeed later. If it is false, the app shows the rejection to the rider and removes the event from the queue.
- `device_time` is kept as evidence. The official time is when the server received the event (**Proposed**).

| Event arrives when | Result | What happens next |
|---|---|---|
| Handover, with a wrong OTP | Rejected, `wrong_otp` | The rider is told, and support is alerted if the food was already given |
| Handover, after the customer cancelled or the order was reassigned | Rejected, `order_cancelled` or `order_reassigned` | Support is alerted if the food was already given |
| Handover, with no earlier arrival | Accepted | Arrival is recorded first, then handover |
| Problem, on a finished order | Accepted | Support sees it |
| Restock confirm, after the suggestion expired | Rejected, `suggestion_expired` | The next suggestion is drafted |
| Location, older than one already applied | Accepted | Kept as evidence, and does not change the order |
| Location, with a `device_time` more than 30 seconds before it arrived (**Proposed**) | Accepted | Evidence only. It never starts or stops a timer, so a flush of old pings cannot do both at once |

### Signal lost at the gate
A rider with no signal sends no pings, so the server cannot detect arrival. The customer must still be able to get the food (**Proposed**):

- `handover_otp` is also set if no ping has arrived for 30 seconds, and either the timer has ended or `wait_deadline` has passed at `rider_accepted`. Both conditions are needed, so a late rider with working GPS never releases the OTP early.
- `otp_available` tells the rider that the customer has it. Offline, it may be stale, so the app must still allow the rider to enter an OTP, and queue it.
- A handover made in the last 30 seconds (by `device_time`) is rejected with `rider_not_near` if fresh GPS shows the rider away from the gate. An older queued handover with a valid OTP is accepted and flagged for audit, because the rider may have handed over offline and ridden on.
- A valid OTP with no fresh GPS records the arrival, with the time the server received it, and marks `arrival_source` as `handover`, not `gps`. These are flagged for audit, so a rider who switches GPS off to phone for the OTP can be found.
- A late location with a `device_time` inside the timer goes to audit review before any remedy. If the review is not finished within 10 minutes (**Proposed**), the remedy goes ahead, so the customer is not left waiting.
