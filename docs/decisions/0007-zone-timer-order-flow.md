# 0007: Zone-based order timer

- Status: Proposed (flow decided by the owner on 2026-11-04; starting values and this write-up await owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
The KT docs and the earlier plan start the 90 s clock at server acceptance, after the rider has packed, and the plan measures a miss up to handover. The owner replaced this with a Blinkit-style flow: the timer covers only the final approach, and it ends before the rider reaches the door.

## Options considered
1. KT flow: the clock runs from acceptance to the gate. One simple promise, but the rider rides under the clock for the whole trip.
2. Zone timer: the clock runs only from zone entry to the 100 m perimeter. Payment, packing and the first part of the ride are outside it, and the rider has time to check doors.

## Decision
Option 2. The flow is defined in `docs/engineering/data_flow.md`, section 1.

- **The promise is reaching the customer's location,** not handover or door delivery.
- **Timer start:** the rider is packed and inside the zone, confirmed by two GPS pings, and the minimum wait has passed. Packed means Ready was pressed or the packing limit passed, so the rider never has to start the timer.
- **Timer stop:** the rider enters the 100 m perimeter. The customer sees "rider has arrived at your location", and the map stays live.
- **Proof:** GPS alone stops the timer, and OTP is the proof of delivery. The required QR or code scan on arrival, and `POST /v1/orders/{id}/arrive`, are dropped (owner, 2026-11-04).
- **Settings:** timer length (90 s), zone radius (350 m, set per location) and the others are console settings. The owner will write the location-based radius algorithm later.
- **Customer stages:** "looking for riders" then "rider has accepted". The wait is shorter, but never zero, for a rider already in the zone.
- **Rider display:** a calm "1.5 mins", not a bumper-style countdown.
- **Wording:** marketing and branding own the public claim and copy.
- **Cancels and refunds:** a full refund if cancelled within 5 seconds of payment being confirmed. After that, no cancel and no refund. Failures on Ninzo's side are always refunded, and the customer is told why and how. The customer can also leave with a refund once the 5 minute maximum wait passes.
- **Address:** no gate or address change after payment.

Starting values proposed by engineering, changeable in the console: 5 minute maximum wait before the timer; credit if the timer ends before 100 m.

## Consequences
- `accepted_at` splits into `rider_accepted_at`, `timer_started_at` and `timer_stopped_at`. This changes the customer contract in the KT doc, so Developer 2 must be told.
- Rider location now drives the customer's timer, so the core of `rider-api` must be built alongside `customer-api`. A simulator alone cannot prove the flow.
- GPS alone starts and stops the timer. Two-ping confirmation and speed checks guard against jumps, and OTP stays the proof of delivery.
- Each order keeps the timer length and radius it started with, so a console change cannot affect a running timer.
- Until the console exists, these settings are seeded in the database.
- The 55 s sweet spot no longer gates the customer's offer. It remains an input for placing scooters at staging nodes.

## Links
`docs/engineering/data_flow.md`, `apps/customer-api/docs/EDD.md`. Overrides: customer app doc rule 1 and C7; rider app doc rule 4 and the arrival part of R9; `docs/context/BACKEND_PLAN.md` sections 1 and 5.
