# Rider app (APK) compatibility

The finished rider app was built against a copy of our backend, and the owner will not change it (Issue 235, 2026-11-13). So our rider API serves every call the app makes. This file lists each call and how we serve it. The app's own client is `lib/core/api/api_endpoints.dart`, `rider_api.dart`, `rider_api_client.dart` and `rider_api_session.dart`. The copy's contract was the second source. The contract is `contracts/rider/openapi.yaml`, the mock is `internal/mockserver/rider_apk.go`, and the tests are `tests/contracts/riderapp_test.go` and `internal/mockserver/rider_apk_test.go`.

Status of each call:

- **a**: ours already serves it as the app sends it.
- **b**: ours has it at another path or in another shape. The fix is an added path (an alias with its own operationId) or added optional fields. Ours keeps working as it was.
- **c**: ours had no such operation, so it is added.

"Server" says what part B of Issue 235 must change. Until then the mock answers, and the contract marks the new fields optional so the server's database tests stay green.

## The 52 calls

| # | Call (app method) | Method and path | operationId | Status | What was done | Server (part B) |
|---|---|---|---|---|---|---|
| 1 | requestOtp | POST `/auth/otp/request` | requestOtp | b | New 400 `invalid_phone` for a malformed phone (rider only) | Answer `invalid_phone` |
| 2 | verifyOtp | POST `/auth/otp/verify` | verifyOtp | a | Retry-After note added | The pause is per `request_id`, never per phone |
| 3 | refresh | POST `/auth/refresh` | refreshToken | a | none | none |
| 4 | signOut | POST `/auth/signout` | signOut | a | none | none |
| 5 | getRider | GET `/riders/me` | getRider | b | `phone`, `email`, `area` added to Rider | Send them (null if unset) |
| 6 | updateRider | PATCH `/riders/me` | updateRider | b | Accepts `email` and `area` besides `name` and `preferred_area`. `preferred_area` and `area` together is 400 | Store email. Keep `area.name` as `preferred_area` |
| 7 | getRiderState | GET `/riders/me/state` | getRiderState | b | Shift gets `online_at`, `planned_end_at`, `break`. Offers and the current order get `total`, `payment_method` | Send them |
| 8 | registerDevice | POST `/riders/me/devices` | registerRiderDevice | a | none | none |
| 9 | removeDevice | DELETE `/riders/me/devices/{device_id}` | removeRiderDevice | a | none | none |
| 10 | getVerification | GET `/riders/me/verification` | getVerification | a | Ours also carries `documents` | none |
| 11 | startVerification | POST `/riders/me/verification/start` | startVerification | a | none | none |
| 12 | getEarnings | GET `/riders/me/earnings` | getEarnings | a | none | none |
| 13 | reportProblem | POST `/riders/me/problems` | reportProblem | a | none | none |
| 14 | sendEventBatch | POST `/riders/me/events` | sendEventBatch | a | none | none |
| 15 | locationPing | POST `/riders/me/location` | locationPing | a | none | none |
| 16 | live stream | GET `/riders/me/stream` (WebSocket) | rider `stream.asyncapi.yaml` | a | Identical file. The Offer and order messages inherit the new optional fields | Send `total`, `payment_method` on them |
| 17 | listNotifications | GET `/riders/me/notifications` | listRiderNotifications | a | Ours is a superset (`order_id`, paging) | none |
| 18 | getSettings | GET `/riders/me/settings` | getRiderSettings | c | Added, with `max_break_s` besides the app's fields | Build from the console's zone settings |
| 19 | getHelp | GET `/riders/me/help` | getRiderHelp | c | Added | Articles served from the backend, edited in the console |
| 20 | sendFeedback | POST `/riders/me/feedback` | sendRiderFeedback | c | Added | A feedback table |
| 21 | getShiftPlan | GET `/riders/me/shift-plan` | getShiftPlan | c | Added. `plan` is null unless operations made one | Return null until the console can make plans |
| 22 | replyToShiftPlan | POST `/riders/me/shift-plan/reply` | replyToShiftPlan | c | Added. 404 with no plan, 409 `shift_plan_closed` once packing starts | Record the reply |
| 23 | listNodes | GET `/nodes` | listNodes | a | none | none |
| 24 | startShift | POST `/riders/me/shifts/start` | startShift | b | `checklist` added. `take_over` stays | Keep the ticked codes with the shift |
| 25 | endShift | POST `/riders/me/shifts/end` | endShift | a | none | none |
| 26 | listShifts | GET `/riders/me/shifts` | listRiderShifts | b | Same path. Accepts `days` besides `from`, `to`, `cursor`, `limit`. Entries gain `online_at`, `planned_end_at`, `orders`, `portions`, `avg_delivery_s` | `days` returns all shifts, `next_cursor` null. A shared path cannot have two operationIds, so ours stays |
| 27 | goOnline | POST `/riders/me/shifts/online` | goOnline | c | Added | `shifts.online_at`, set once |
| 28 | takeBreak | POST `/riders/me/shifts/break` | takeBreak | b | Alias of our `startBreak`, with `length_s` | Cap by `max_break_s`. Extending a break is allowed |
| 29 | endBreak | POST `/riders/me/shifts/break/end` | endShiftBreak | b | Alias of our `endBreak` (the app's operationId `endBreak` was ours already) | Same handler |
| 30 | setPlannedEnd | POST `/riders/me/shifts/planned-end` | setPlannedEnd | c | Added | Record `planned_end_at`. Nothing ends the shift |
| 31 | listDocuments | GET `/riders/me/documents` | listRiderDocuments | c | Added | File metadata, photos in Supabase Storage |
| 32 | uploadDocument | POST `/riders/me/documents` | uploadRiderDocument | c | Added, base64 body as the app sends | Decode, write to Supabase Storage, never bytea. Map `drivingLicence` and `vehicleRc` to our document names |
| 33 | acceptRelocation | POST `/riders/me/relocations/{relocation_id}/accept` | acceptRelocation | a | none | none |
| 34 | getStock | GET `/riders/me/stock` | getStock | a | none | none |
| 35 | scanStock | POST `/riders/me/stock/scan` | scanStock | a | none | none |
| 36 | quarantineLot | POST `/riders/me/stock/{lot_id}/quarantine` | quarantineLot | c | Added | Take the lot off the scooter |
| 37 | listOffers | GET `/riders/me/offers` | listOffers | b | Offer gets `total`, `payment_method` | Send them |
| 38 | acceptOffer | POST `/offers/{offer_id}/accept` | acceptOffer | b | The order reply gets the same two fields | Send them |
| 39 | declineOffer | POST `/offers/{offer_id}/decline` | declineOffer | a | none | none |
| 40 | markReady | POST `/offers/{offer_id}/ready` | markReady | b | Same, on the order | Send them |
| 41 | getOrder | GET `/orders/{order_id}` | getRiderOrder | b | Same, on the order | Send them |
| 42 | getRoute | GET `/orders/{order_id}/route` | getRoute | a | none | none |
| 43 | startMaskedCall | POST `/orders/{order_id}/call` | startRiderMaskedCall | a | `call_not_available` is the mock default too. Scenario `masked_call_works` shows the success shape | none |
| 44 | handOver | POST `/orders/{order_id}/handover` | handOver | b | The order reply gets the same two fields | Send them |
| 45 | checkGate | POST `/orders/{order_id}/gate-check` | checkGate | c | Added, with no Idempotency-Key, as the app sends none | Compare only. Never decides arrival |
| 46 | createRestock | POST `/restock` | createRestock | a | none | none |
| 47 | getRestockOptions | GET `/restock/options` | getRestockOptions | b | Hubs gain `manager_phone` | Send it (null if unknown) |
| 48 | listRestockSuggestions | GET `/riders/me/restock-suggestions` | listRestockSuggestions | a | none | none |
| 49 | getRestock | GET `/restock/{request_id}` | getRestock | a | none | none |
| 50 | confirmRestock | POST `/restock/{request_id}/confirm` | confirmRestock | a | none | none |
| 51 | declineRestock | POST `/restock/{request_id}/decline` | declineRestock | a | none | none |
| 52 | respondToShortfall | POST `/restock/{request_id}/shortfall-response` | respondToShortfall | a | none | none |

Totals: **a 27, b 14, c 11** (requestOtp moved to b by the review fix).

## In the copy's contract, not in the app

| Copy's operation | What we did |
|---|---|
| `reportScooterTelemetry`, POST `/scooters/{code}/telemetry` | Added, but signed with the key of one scooter in `X-Telemetry-Key`, never a key shared by the fleet. The app never calls it. It sits outside bearer sign-in and takes no Idempotency-Key, and `tests/contracts/rules_test.go` lists it on purpose |
| `connectCall`, GET `/telephony/connect` | Not copied. It is a provider call bridge with the key in the URL. Masked calls stay `call_not_available` |

## Owner rules, and where they are written

All of them are in the description of the rider contract (`info.description`), and `TestRiderContractKeepsTheOwnerRules` fails if one is deleted.

| Rule | How the app is still served |
|---|---|
| Shifts are open-ended | `getShiftPlan` returns `plan: null` unless operations make one. The reply is accepted and recorded |
| Planned end and Add time | `setPlannedEnd` is recorded as `planned_end_at`. Nothing ends the shift |
| Break length | `takeBreak` is capped by the console's `max_break_s`. The shift shows the real end in both forms |
| Help articles | Served by the backend, edited in the console |
| Gate code | `checkGate` is accepted, but arrival is by location only (decision 0007) |
| Masked calls | `call_not_available`, no call bridge |
| Telemetry | A key for each scooter |
| Document photos | Supabase Storage. The database keeps only the file's path, type, size and time |
| Sign-in pause | `otp_attempts_exceeded` (429) with `Retry-After`, counted against one `request_id`, never a phone number |
| Cancelled-order pay | Only if the rider had already started moving toward the gate. `cancel_pay_cap` is in the settings |

## Error codes the app handles

The app turns a reply's `code` into screens: `invalid_otp`, `otp_expired`, `otp_attempts_exceeded`, `account_blocked`, `order_cancelled`, `order_reassigned`, `wrong_otp`, `verification_in_progress`, `shift_already_open`, `no_open_shift`, `order_in_progress`, `restock_already_open`, `lot_already_on_scooter`, `lot_expired`, `bad_scan`, `lot_not_expected`, `lot_quarantined`, plus the shift start reasons `rider_not_approved`, `rider_not_verified`, `shift_limit_reached` and `no_scooter_at_node`. All are already in our contract or mock.

## What cannot be matched

- **Bad phone on sign-in (solved, owner approved 2026-11-13).** The app maps `invalid_phone` to its invalid number screen. The rider `requestOtp` now answers a malformed phone with 400 `invalid_phone` (a missing phone or non-JSON stays `invalid_request`). The customer contract is unchanged. The server does this in part B. The app also reads `validation_failed` and `rate_limited`: `rate_limited` exists, `validation_failed` is not used by either contract.
- **The `/riders/me/shifts` operationId.** The app's `listShifts` and our `listRiderShifts` share one path and method, so one operation serves both. The path, query and reply match. Only the name differs, and the app does not send a name.
- **The 16-hour limit on `setPlannedEnd`** is the server's. The mock only refuses a past time, because the example in the contract is a fixed far date.

## Phone-local keys

The app also reads keys such as `phase`, `spot`, `previews`, `manifest`, `welcomed`, `delivered` and `returned`. These are kept on the phone (`RiderDayStore`) or come from Google's APIs. They are not in any reply of ours.
