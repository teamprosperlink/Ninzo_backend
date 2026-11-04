# Project spec: customer-api

Version: 0.1.0 (draft for owner review) · Updated: 2026-11-04 · Source keys: see `PRD.md`

GitHub Issues do not exist yet, so the Issue column is left out. Add it when the features are filed.

**Proposed** means the behaviour depends on something not yet agreed.

## 1. Conventions
All from CUST, "API endpoints", except where marked.
- All paths are under `/v1`. Every request sends `Authorization: Bearer <token>`, except the OTP endpoints.
- Every response includes `server_time`.
- Errors have one shape: `code`, `message`, `retryable`.
- Every repeatable write requires `Idempotency-Key`.
- Same key and same body returns the stored response. Same key and a different body returns 422. Keys are kept for 24 h. (PLAN section 3)
- Lists use cursors. (CUST `GET /v1/orders`)
- Field names are final only after the first contract review with Developer 2. (CUST, Notes)

## 2. Features

| ID | Feature | Priority | Done when | Source |
|---|---|---|---|---|
| CA-01 | OTP sign-in and refresh | P1 | A customer signs in with a test OTP. Wrong code, expired code and resend limit each return a distinct error. Refresh issues new tokens | CUST C2 |
| CA-02 | Gates | P1 | A saved gate stays on the account after reinstall. A pin outside the service area and a duplicate gate are rejected | CUST C3 |
| CA-03 | Menu and availability | P1 | Only items truly in reach are returned, with `qty_left` and `stale_at`. A zone with the offer off and an unverified gate return no offer | CUST C4, EDGE rule 1 |
| CA-04 | Create order | P1 | One key creates exactly one order. 50 parallel orders for the last unit produce one winner and no negative stock | CUST C5, PLAN section 10 |
| CA-05 | Payment and webhook | P1 | Only a verified webhook moves payment state. A duplicate webhook has one effect. Failed payment releases stock | CUST rule 6, EDGE |
| CA-06 | Waiting stages | P1 | "Looking for riders" then "rider has accepted", with a shorter wait above zero for a rider already in the zone. State survives an app restart. After the maximum wait, the customer can leave with a full refund or keep waiting | Decision 0007, CUST C6 |
| CA-07 | Zone timer | P1 | The timer starts once, when the rider is packed and inside the zone and the minimum wait has passed, with no rider action. It stops once at the 100 m perimeter. A console change does not affect a running timer | Decision 0007, EDGE rule 3 |
| CA-08 | Live order stream | P1 | Token in the first frame. Events carry `seq`, `customer_stage`, the timer fields, `eta_source` and `server_time`. After reconnect, `GET` returns the true state | CUST C6-C8, C13 |
| CA-09 | Arrival and handover | P1 | At 100 m the customer sees "rider has arrived at your location", and the map stays live until handover. Handover is recorded once even if sent twice. A wrong OTP is rejected. See EDD 9.2 for who submits what | Decision 0007, CUST C8, Rider R9 |
| CA-10 | No scooter available | P1 | The customer can wait, take a longer ETA with explicit consent, ask to be notified, or cancel with a full refund. No order is silently re-promised | CUST C9, EDGE, PLAN gap 8 |
| CA-11 | Cancel | P1 | Full refund within 5 seconds of payment being confirmed. After that, rejected with a clear code and no refund, until `wait_deadline` passes. No rider is contacted until the window closes | Owner decision 2026-11-04, CUST rule 7 |
| CA-12 | History and reorder data | P1 | Past orders list by cursor. An order returns enough item data to refill a cart | CUST C10 |
| CA-13 | Help and refund status | P1 | A customer files a report and sees why, how and when they are refunded. Failures on Ninzo's side are always refunded | CUST C11, owner decision 2026-11-04 |
| CA-14 | Notifications | P1 | Push and SMS are sent at each customer stage, and never reveal a later state. Device tokens can be registered. Payload carries order ID, and the app fetches the true state | CUST C12, status known gaps |
| CA-15 | Automatic refunds and remedy | P1 | A failure on Ninzo's side always triggers a refund, and an audit record, with no staff action. A timer that ends before 100 m on an order still delivered triggers the set remedy (credit to start) | Owner decision 2026-11-04, EDGE rule 4, Decision 0007 |
| CA-16 | Zone switch and gate cap | P1 | With a zone switched off or a gate at its cap, new orders get no offer | EDGE, ARCH glossary |
| CA-17 | Masked call | P2 until provider chosen | Customer and rider can call without seeing each other's numbers | CUST feature table |
| CA-18 | Rider position on the map | P1 | From when the customer sees "rider has accepted" until handover, including after the timer stops, the customer sees the rider's position, with no rider data beyond what is needed | CUST feature table, Decision 0007 |

CA-17 is listed as a core countdown feature in CUST. Priority P2 is **Proposed**, because no telephony provider is chosen.

## 3. Endpoints

| Method and path | Feature | Origin |
|---|---|---|
| `POST /v1/auth/otp/request` | CA-01 | CUST |
| `POST /v1/auth/otp/verify` | CA-01 | CUST |
| `POST /v1/auth/refresh` | CA-01 | CUST |
| `GET /v1/gates`, `POST /v1/gates` | CA-02 | CUST |
| `GET /v1/menu/items/{id}` | CA-03 | CUST |
| `GET /v1/availability?gate_id=` | CA-03 | CUST |
| `POST /v1/orders` | CA-04 | CUST |
| `GET /v1/orders`, `GET /v1/orders/{id}` | CA-06, CA-12 | CUST |
| `POST /v1/orders/{id}/cancel` | CA-11 | CUST |
| `WS /v1/orders/{id}/stream` | CA-08 | CUST |
| `POST /v1/orders/{id}/handover-confirm` | CA-09 | CUST. Removed: the customer app shows `handover_otp`, and the rider submits it. See EDD 9.2 |
| `POST /v1/support/tickets`, `GET /v1/support/tickets/{id}` | CA-13 | CUST |
| `GET /v1/orders/{id}/refund` | CA-13 | CUST |

**Missing from the KT doc, Proposed additions.** Final shapes are set in the contract.

| Purpose | Proposed path | Feature |
|---|---|---|
| Razorpay webhook | `POST /v1/webhooks/razorpay` (signature, no bearer token) | CA-05 |
| Retry payment | `POST /v1/orders/{id}/payment/retry` | CA-05 |
| Menu list | `GET /v1/menu/items` | CA-03 |
| Customer profile | `GET /v1/me` | CA-01 |
| Device token | `POST /v1/me/devices` | CA-14 |
| Accept a longer ETA | `POST /v1/orders/{id}/accept-eta` | CA-10 |
| Notify me | `POST /v1/availability/notify` | CA-10 |
| Change gate before paying | `PATCH /v1/orders/{id}/gate` | CA-02, EDGE |
| Handover OTP for display | Included in `GET /v1/orders/{id}` at arrived, or in the signal-lost case in `contracts/README.md` | CA-09 |
| Masked call | `POST /v1/orders/{id}/call` | CA-17 |

Rider position is delivered on the order stream, not through a new endpoint.

The contract is `contracts/customer/`, and it wins if this table differs. It also adds `DELETE /v1/me/devices/{id}`, for sign-out.

## 4. Business rules
1. The countdown is `timer_started_at + timer_duration_s`, from the server, corrected for phone clock offset. (Decision 0007, replacing CUST rule 1)
2. `eta_source` is `measured`, `placeholder` or `unavailable`. The server must never send a time as `measured` unless it is measured. (CUST rule 2)
3. A notification carries no authoritative state. (CUST rule 4)
4. Refund wording comes from the copy deck, and must match how the money returns. It is never "nobody is charged". The API returns the reason and method so the app can tell the customer why and how. (CUST rule 7, owner decision 2026-11-04)
5. A gate can change only before paying, and a change returns a new payment to complete. After payment the address cannot change. The customer can only cancel within the 5-second window. Owner decision, 2026-11-04, overriding EDGE.
6. If checks fail, the offer is withdrawn, never stretched. (EDGE)
7. Stock can be held only by a reservation with a TTL, and can never go negative. (ARCH, PLAN section 1)
8. Prices are fixed at order creation. **Proposed,** implied by the Razorpay order amount in the `POST /v1/orders` response.
9. Timer length, zone radius, perimeter and waits come from console settings, never constants. Each order keeps the values it started with. (Decision 0007)

## 5. Edge cases
From EDGE, the scenario catalog, limited to what the customer sees.

| Scenario | Expected behaviour |
|---|---|
| No scooter available | No offer. Longer ETA with consent, or wait with a timeout |
| Rider already in the zone | Shorter wait above zero, then the timer starts when packed and the wait has passed |
| Rider already inside 100 m when packed | No timer, straight to "arrived" |
| Timer ends before 100 m | Calm message, map stays live, remedy fires |
| One scooter in range, two orders | First wins on ETA. The second is re-checked after the first leaves |
| Two customers want the last unit | Reservation decides. The loser sees alternatives |
| Burst of orders to one gate | Cap on concurrent orders per gate. Queue with honest ETA |
| Rider ignores an offer | Short offer timeout, then re-offer |
| Rider drops before accepting | Release reservation and rematch |
| Breakdown after the rider accepts | Safety first, tell the customer, and refund. If the rider was already shown, log the miss |
| Rain, traffic or road closure | Reduce the radius, or suspend the offer in the zone |
| Gate closed or entry refused | Use the pin, call the customer, hand over at the nearest open point |
| Customer unreachable at gate | Wait window, call, then end. This is not a failure on Ninzo's side, so no refund. **Proposed.** Wait window not set |
| Wrong pin or customer moves | Confirm before payment. No change after payment. Cancel only within the 5-second window |
| Temperature breach | Quarantine lot, pull scooter from search, and refund |
| Payment fails after reserve | Release stock, no rider contacted, allow retry |
| App crash or no network at gate | Server timer keeps running. Reconcile later |
| Duplicate or stale commit | Idempotency key and fencing token reject the later event |
| False arrival claim | Two-ping and speed checks on GPS, plus customer OTP at handover |

## 6. Assumptions
- Single-leg orders only until multi-scooter is built. **Proposed.**
- The pilot serves a small set of ops-verified gates. **Proposed,** Q3.
- Scan-only temperature checks for the pilot. **Proposed,** Q6.
- The rider side is driven by `cmd/rider-sim` until `rider-api` exists. (Project status, build order)

Open questions and known gaps are tracked in `docs/planning/project_status.md` and the EDD, section 9.
