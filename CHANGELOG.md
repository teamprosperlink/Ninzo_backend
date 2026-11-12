# Changelog

Format: newest first. One section per version; inside it, one entry per feature or module,
each with its Issue number. Entries are added in the same PR that ships the feature.

## [Unreleased]

### The pilot's hexagon grid (#193)
- Decided (G18): no pure-Go H3 covers point to cell, centre, disk and outline and also builds on the Windows development PC, so the pilot uses its own pure-Go hexagon grid. The spike result is in `docs/engineering/geo/w2-map-grid-and-distribution.md`.
- Added: `internal/hexgrid`, pointy-topped hexagons on a local flat projection; Bengaluru grid `blr76` with 76 m edges (the area of H3 res 10). It provides `CellAt`, `Centre`, `Outline`, `Neighbours`, `Disk`, `Distance`, `ID` and `Parse`.
- Evidence: docs/testing/reports/193.md

### Road data versions (#190)
- Added: migration 0045, table `road_data_versions`: one row per OSRM build (source and sha256, clip box, extract timestamp, profile and version, image, algorithm), unique per extract, profile, image and algorithm.
- Added: `internal/roaddata` and `cmd/roaddata -local|-remote register <build.json>|list`, which checks the file before touching any database and stores each build once; `task geo:roaddata:register`.
- Evidence: docs/testing/reports/190.md

### Golden routes in CI (#187)
- Added: the CI job `routes` builds OSRM on the committed test-zone extract and runs the golden routes with `REQUIRE_ROUTING_TESTS=yes`, so a change that breaks one cannot merge.
- Evidence: docs/testing/reports/187.md
### Console hub operations: the server side (#149)
- Added: `listHubOps`, `listStuckRestocks` (permission `hub_ops.read`) and `expireRestock` (`hub_ops.manage`) on console-api, as defined in `contracts/console`. A hub_manager sees only their own hub on both lists and cannot expire.
- Added: the setting `restock_stuck_s` (default 600 seconds, range 60 to 7200, per zone, one line in `internal/geo/settings.go`, migration 0046). A request waiting longer than it in confirmed, hub_accepted or packing is stuck, counted from when it entered that state.
- Added: `restock.ExpireTx`. Ending a request by hand shares `endRequest` with the expiry on its own (lots back on the shelf, an open shortfall refused), then raises the rider's and the hub's seq, pushes and notifies the rider. Allowed in confirmed, hub_accepted, packing or partial; else 409 `restock_not_expirable`. The staff's reason is in the audit row only.
- Changed: the mock server filters `listHubOps` by `zone_id`; its stuck sample waits longer than the default limit.
- Evidence: docs/testing/reports/149.md

### OSRM on Bengaluru with a scooter profile (#183)
- Added: `tools/osrm/` builds OSRM v6.0.0 (MLD) road data from OpenStreetMap: Geofabrik's southern zone clipped to Bengaluru (about 30 s, peak 750 MB), or the committed test-zone extract (5 s). `build.json` records the extract's timestamp and sha256, the profile version and the image.
- Added: the scooter profile `scooter-2` (left-hand driving, a speed table that `maxspeed` cannot exceed, 40 km/h cap, 30 s per signal, 60 s U-turn, two-wheeler access and restriction tags, no motorways unless tagged for two-wheelers; placeholders until the road test).
- Added: a local OSRM on 127.0.0.1:5001 (`task geo:osrm:build`, `geo:osrm:up`, `geo:osrm:down`) and golden routes on real roads (`task geo:test:routes`).
- Evidence: docs/testing/reports/183.md
### Console incidents: one queue for what needs a person (#148)
- Added: migration 0044 (tables `incidents` and `incident_notes`, `problem_reports.handled_at`, triggers that open an incident in the same transaction as each support alert, problem report, lost signal, arrival review and ticket and close it when a ticket or review closes, and a backfill of what exists). `listIncidents`, `getIncident` (permission `incidents.read`), `assignIncident`, `addIncidentNote` and `closeIncident` (`incidents.manage`) on console-api. Every write needs an Idempotency-Key, is audited, and locks the incident row.
- Changed: closing a support alert's incident marks the alert handled (`outbox_events.delivered_at`), which fixes the #138 downside that alerts stayed in the outbox's due index; closing a problem report's incident sets `handled_at`. A ticket or arrival review is never changed from the queue (`source_open`). The mock's incident list gains a lost-signal sample.
- Evidence: docs/testing/reports/148.md

### Routing client with a straight-line fallback (#180)
- Added: `internal/routing`, the OSRM client: `Route`, `Table` (many to one, split to OSRM's limit) and `Nearest`. Every answer says `osrm` or `placeholder`; no road is unreachable, never a straight line; a circuit breaker and one timeout per call. Not wired into matching yet (W5).
- Changed: `env.example` describes `ROUTING_ENGINE_URL` and adds `ROUTING_PROFILE`.
- Evidence: docs/testing/reports/180.md

### More test riders and scooters on staging (#175)
- Added: `testdata testers`, run by `task staging:testdata`. It adds three test riders (+919800000081 to 0083), approved in the first test zone, and two more scooters per test zone, so several testers can use staging at once. Running it again changes nothing, and a real person with one of the numbers is never touched.
- Evidence: docs/testing/reports/175.md
### Customer app additions, the server (#178 part B)
- Added: migration 0043. `menu_items.is_veg` (set in the console menu create and update, audited, shown on the customer menu); `gates.landmark` and `gates.gate_type` (stored on create, returned on read, kept in `gate_history`, landmark cleared when a customer is erased); table `customer_notification_settings` with `GET` and `PATCH /me/notification-settings` (defaults: offers and news off, support replies on; `order_updates` in the body is a 400; audited; Idempotency-Key).
- Changed: the customer order's `rider.scooter_plate` is now the code of the scooter on the rider's shift (null when none is on record), shown by the same stage rule as the first name. The push worker skips a `ticket_reply` push when the customer turned support replies off; order pushes are never skipped. Nothing sends offers or news pushes yet.
- Evidence: docs/testing/reports/178.md
### Customer app additions, contract and mock (#178 part A)
- Added: `is_veg` on the customer MenuItem and on the console menu create, update and read; `scooter_plate` (nullable) beside the rider's first name on the customer order; `getNotificationSettings` and `updateNotificationSettings` (`/me/notification-settings`); optional `landmark` (up to 80 characters) and `gate_type` (open list) on the customer gate create and read. Mock samples show all of them.
- Changed: the server sends `is_veg` false and `scooter_plate` null until part B builds them.
- Evidence: docs/testing/reports/178.md

### Geo work has its own local Postgres and Redis (#169)
- Added: `deploy/compose.geo.yml` (compose project `ninzo-geo`, Postgres on 5433 and Redis on 6381) and the tasks `geo:db:up`, `geo:db:down`, `geo:db:migrate` and `geo:test:db`, so the geo session never shares a test server with the console session.
- Changed: `task test:db` takes `TEST_DB_PORT` (default 5432, so nothing changes for anyone else).
- Evidence: docs/testing/reports/169.md
### Console support actions: the server side (#147)
- Added: `listTickets`, `getTicket`, `replyTicket`, `closeTicket` (permission `tickets.manage`), `requestRefund` (`refunds.request`), `issueReplacement` and `issueCredit` (`remedies.issue`) on console-api, as defined in `contracts/console` (#164, #165). Every write needs an Idempotency-Key and a reason where the contract says so, is audited, and saves its reply in the same transaction.
- Added: the setting `refund_approval_paise` (20000 paise, range 0 to 10,000,000, one line in `internal/geo/settings.go`). A refund is made at once (200) when the console's refunds on the order, plus any waiting refund approval, plus this one are at or below it; above it an approval of kind `refund` waits (202) and `console.ApplyRefund` makes the refund when a second admin approves. A rejected, cancelled or lapsed approval frees its amount. An approval whose refund no longer fits fails with `refund_too_large` or `not_paid` and changes nothing.
- Added: `orders.RecordSupportRefund`, so a console refund is a row of the existing `refunds` table (processed at once for the test provider, pending for Razorpay), read by the customer's `getRefund` and order views as before. Its `reason_code` is `support_refund_<n>`.
- Behaviour: a refund locks the order, then its paid payment, so two requests on one order run one after the other and cannot pass the paid amount or the limit. A replacement lists items on the order (no more than ordered); an order has one remedy of each type (`remedy_exists`). A ticket reply is a support message in `ticket_messages` and queues one `push.customer` event naming the ticket (`stage: ticket_reply`), never the words, a phone or an address; replying to or closing a resolved ticket is 409 `ticket_closed`.
- Added: migration 0042: `refunds.requested_by` and `note`; `remedies.created_by` and `items`; `support_tickets.closed_at`, `closed_by`, `close_reason` and an index; `ticket_messages.author_id`; `zone_settings.refund_approval_paise`. Not applied to Supabase.
- Changed: the order timeline now fills `requested_by` (refunds) and `created_by` and `items` (remedies), names the person on their events, and uses a ticket's `closed_at`. `internal/orders/orders.go` takes a refund's starting status from one function. `console_test.go` mounts the new routes and the `refund` kind (two lines).
- Evidence: docs/testing/reports/147.md

### Console order search and timeline: the server side (#146)
- Added: `listAdminOrders` (`GET /admin/orders`) and `getOrderTimeline` (`GET /admin/orders/{order_id}/timeline`) on console-api, permission `orders.read`, as defined in `contracts/console` (#164, #165). `q` takes an order number such as A1001 (any case; matched on `order_no` so a repeated short form returns both orders), an order id, or the last four phone digits of the customer or rider; anything else is 400. Filters `state`, `zone_id`, `from`, `to`; cursor paging, newest first.
- Added: the timeline merges `order_events`, `offers`, `payments`, `refunds`, `remedies`, `problem_reports`, `support_tickets` and their messages, `arrival_reviews` and `audit_events` into one list, oldest first (ties in the contract's source order), in one read-only repeatable-read snapshot. Pings are summarised (`count`, `first_at`, `last_at`).
- Privacy: only `right(phone, 4)` is read from `people`; the gate shows its label and pin; ticket text, problem notes, audit before/after and entry notes never leave.
- Added: migration 0041: indexes for the list (`orders_created_idx`, `orders_zone_created_idx`, `orders_rider_created_idx`, `people_phone_last4_idx`) and for the timeline reads (`offers`, `support_tickets`, `arrival_reviews`, `refunds` by order). No table or column changes.
- Changed: `refund.requested_by` and `remedy.created_by` are null until the support actions (#147) record a creator; the data map, stand-ins list and known downsides say so.
- Tests: `internal/console/orders_test.go` checks both replies against the console contract's schemas.

### Rider operations: the server side (#159)
- Added: migration 0040: `riders.preferred_area`; `shifts.device_id`, `break_started_at`, `break_ends_at` and `returned_items`; `shift_breaks`; `orders.rider_waiting_since`; `rider_notifications`; `stock_counts` and `stock_count_lines`; `verification_documents`; the setting `max_break_s` (1800, range 300 to 7200). Not applied to Supabase.
- Added: `updateRider`, `listRiderNotifications`, `listRiderShifts`, `getShiftSummary`, `startBreak`, `endBreak`, `cancelRestock`, `markWaitingAtGate`, `submitStockCount`; `startShift` takes `device_id` (400 `unknown_device`) and `take_over`; `getVerification` returns `documents`; `getRider` returns `preferred_area`; the customer's order returns `rider_waiting_since`; the rider's shift returns the break and device fields.
- Behaviour: a break is refused with a live order (`order_in_progress`) or an unanswered offer (`offer_open`), ends on its own at `break_ends_at` (a background pass in the customer API), and ends with the shift. A rider on a break is skipped by the free-rider selection in `internal/orders/offers.go`, and their scooter is not counted by `reachable` in `internal/availability` (the two conditions agreed with the geo session). Every `push.rider` event now also writes a `rider_notifications` row through `internal/notify`.
- Changed: `tests/db` lists the v0.10.0 tables built so far; the data map gains `stock_count_lines` and loses the `scheduled_tasks` writes for breaks (a pass reads `break_ends_at`).
- Evidence: docs/testing/reports/159.md
### Rider operations: contract and mock (#157)
- Added to the rider contract: `updateRider` (PATCH /riders/me, name and `preferred_area`), `listRiderNotifications`, `listRiderShifts` (both cursor-paged), `getShiftSummary`, `startBreak`, `endBreak`, `cancelRestock`, `markWaitingAtGate`, `submitStockCount`; `device_id` (optional) on `startShift`.
- Added fields: `Rider.preferred_area`, `Shift.break_started_at`, `break_ends_at` and `device_id`, `RiderOrder.waiting_since`, `VerificationResponse.documents`, and the customer order's `rider_waiting_since`. All required, null where nothing is set.
- Mock: a route for each, and the scenarios `no_open_shift`, `order_in_progress`, `on_break`, `restock_not_cancellable`, `not_arrived`, `rider_waiting` (rider and customer), `shift_on_other_phone`, `documents_rejected`.
- Changed: the allowed-field lists for the customer order and the rider order now include `rider_waiting_since` and `waiting_since`. The data map names the planned tables `rider_notifications`, `shift_breaks`, `stock_counts` and `verification_documents` for v0.10.0. Server side: #159.
### Console amounts have a range (#167)
- Fixed: the console mock answered 201 with a wrapped-round negative amount for `amount_paise` past an int (found by the fuzzer in CI). `requestRefund` and `issueCredit` now say 1 to 10,000,000 paise in the contract, and the mock refuses anything outside it, or not whole, with 400.
### Geo design and research record (#162)
- Added: `docs/engineering/geo/`, the decision and research record for maps, the H3 grid, OSRM routing, rider placement, matching, routes and ETA, demand, simulation and map providers. A draft for owner review; no code.
- Added: the owner's notes of 2026-11-11 on the hybrid grid, pre-mapping and the sweet spot, as `docs/context/algorithms/2026-11-11-owner-notes-hybrid-and-premapping.md`.
- Evidence: docs/testing/reports/162.md

### Console ops and support: contract and mock (#146)
- Added: 18 console operations in `contracts/console/openapi.yaml` with the mock: orders (`listAdminOrders`, `getOrderTimeline`), support actions (`listTickets`, `getTicket`, `replyTicket`, `closeTicket`, `requestRefund`, `issueReplacement`, `issueCredit`), incidents (`listIncidents`, `getIncident`, `assignIncident`, `addIncidentNote`, `closeIncident`), hub operations (`listHubOps`, `listStuckRestocks`, `expireRestock`) and the live map (`getLiveZone`). No server code yet.
- Added: permissions `orders.read`, `tickets.manage`, `refunds.request`, `remedies.issue`, `incidents.read`, `incidents.manage`, `hub_ops.read`, `hub_ops.manage`, `map.read`, in the contract's Permission list, the mock and `console.RolePermissions` (decision 0015's roles).
- A refund is made at once (200) when the console refunds on the order plus any waiting refund approval plus this one are at or below `refund_approval_paise` (20000 paise, Rs 200, from #147); otherwise an approval of kind `refund` waits for a second admin (202).
- Data map: rows for each, and the planned tables `incidents` and `incident_notes` (v0.10.0).

### Rider app fields (#158)
- Added: rider `created_at`, `approved_at` (the newest `rider.approved` or `rider.approve` audit row, null before); shift `node_assigned_at` (the shift start, or the last arrived move in the shift) and `scooter` (`id`, `plate`, `battery_pct`, null until scooter data exists); node `hub_distance_m` (straight line to the nearest active hub, null if none); `order_number` on the rider's order and offer and the customer's order, a letter and four digits such as A2931 (`orders.OrderNumber`); relocation suggestion `orders_waiting`; restock request `ready_in_s`; `lat` and `lng` on every `HubRef`, and `distance_m` on a shortfall's other hub; stock lot `image_url`.
- Added: migration 0039, `orders.order_no` (identity, existing orders numbered) and `relocations.orders_waiting` (null for older rows). Not applied to Supabase.
- Changed: the contract's allowed-field lists for the customer and rider order now include `order_number`.
### Rider verification and route served (#156)
- Fixed: `GET /riders/me/verification`, `POST /riders/me/verification/start` and `GET /orders/{order_id}/route` were in the rider contract but only the mock served them, so staging answered 404 (found by the rider app developer, #154).
- Verification is a stand-in until DigiLocker: start records a pending request (409 `already_verified` or `verification_in_progress` otherwise), and the status follows the shift-start rule. The route is a straight line from the last position to the gate, with a placeholder time.
- Evidence: docs/testing/reports/156.md

### Masked calls answer call_not_available (#151)
- Fixed: `POST /orders/{order_id}/call` in the customer and rider APIs answered 404, because neither mounted it. It now checks the order is the caller's and answers 409 `call_not_available` until a telephony provider is chosen.
- Fixed: the console's unknown-login reply says email, as the other one does.
- Evidence: docs/testing/reports/151.md

### Console sign-in with an email address (#152)
- Changed: the console `login_id` accepts an email address (owner, 2026-11-11); the older ID form still works.
- Changed: the authenticator code is optional until pre-launch (`CONSOLE_TOTP=optional` on staging; production refuses it). The login reply gains `totp_required` and, when false, `session`.
- Added: migration 0038, the wider `console_users.login_id` check. Not applied to Supabase.
- Evidence: docs/testing/reports/152.md

### Audit log, arrival reviews and support alerts (#138)
- Added: `listAuditEvents` (newest first, filters by time, actor, action and entity; secrets and full phone numbers hidden), `listArrivalReviews` (open first), `decideArrivalReview` (arrived_in_time stops the timer at the rider's evidence and clears the miss; late keeps it and its credit), `listSupportAlerts`.
- Changed: a refused handover's support alert names its order, and a lost signal now raises a support alert (its problem report already said support was alerted).
- Added: migration 0037, `arrival_reviews.decided_by` and `decision_reason`. Not applied to Supabase.
- Evidence: docs/testing/reports/138.md

### Menu, gates, hubs and hub staff in the console (#137)
- Added: `listAdminMenu`, `createMenuItem` (added hidden), `updateMenuItem` (a price change keeps its reason in the history), `getPriceHistory`.
- Added: `listAdminGates`, `updateGate` (a new version with who and why; a moved pin stays in its zone; an erased customer's gate is left alone), `getGateHistory`.
- Added: `listAdminHubs`, `createHub`, `updateHub`, `listHubStaff`, `addHubStaff`, `updateHubStaff`. Suspending staff ends their sessions; a hub_manager works only in their own hub.
- Added: migration 0036, `gate_history.changed_by`. Not applied to Supabase.
- Evidence: docs/testing/reports/137.md

### Zones, settings and the kill switch (#136)
- Added: `listZones`, `getGlobalSettings`, `getZoneSettings`, `requestGlobalSettingsChange`, `requestZoneSettingsChange` and `requestKillSwitch` in the console. Every change waits for a second admin.
- A settings request is checked at once (range, known key, base_version, and every zone still valid) and again when approved; one the settings moved past fails as stale_version.
- The kill switch pauses or resumes a zone.
- Evidence: docs/testing/reports/136.md

### Rider review (#135)
- Added: `listRiderReviews` (applicants and failed or expired verifications, oldest first, the last four phone digits only) and `decideRider` (approve into a zone once verified, suspend, reinstate). Suspending ends the rider's sessions and open shift.
- Changed: ending a shift is one function, `shifts.EndShiftTx`, for the rider and the console.
- Evidence: docs/testing/reports/135.md

### Console sign-in, users, approvals and flags (#134)
- Added: `apps/console-api`. Sign-in is an ID and password (PBKDF2), then a TOTP code (RFC 6238) set up at first sign-in; a temporary password must be changed first; 5 failures in 15 minutes lock the account for 15 minutes.
- Added: console users and roles, a permission check on every call (403 names it), approvals (request, approve by a second admin, reject, cancel, lapse after 24 hours, fail if stale), and feature flags through approvals.
- Added: `cmd/console` (`task console`, `task staging:console`) makes the first admin and resets a user.
- Added: migration 0035, `console_users`, `console_challenges`, `approvals`, `feature_flags`. Not applied to Supabase.
- Evidence: docs/testing/reports/134.md

### Console contract and mock (#133)
- Added: `contracts/console/openapi.yaml`, 40 operations: two-step sign-in (ID and password, then TOTP), users and roles, approvals, rider review, flags, zones, settings and the kill switch, menu and prices, gates and their history, hubs and hub staff, the audit log, arrival reviews and support alerts.
- Added: the mock serves it at `/console/v1`; CI checks the mock against it. Decision 0015.
- Evidence: docs/testing/reports/133.md
### The real hub flow (#127)
- Changed: the customer API runs only restock's own jobs: the stock check, the expiry of uncollected cassettes, and, new, the expiry of a confirmed request no hub accepts within `restock_accept_s` (900). Suggestions prefer a hub with staff. Hub actions come from staff through the partner API; the simulator is for tests only.
- Added: migration 0034, `restock_accept_s`. Not applied to Supabase.
- Evidence: docs/testing/reports/127.md

### Hub offline sync (#126)
- Added: `POST /hub/events` applies queued accept, scan, unscan, shortfall, ready and quarantine in order, each once by event_id, with the same rules as their endpoints. One rejected event never blocks the rest; a direct call and its queued copy count once.
- Added: migration 0033, `hub_events`. Not applied to Supabase.
- Evidence: docs/testing/reports/126.md

### Shortfall, sealing and quarantine (#125)
- Added: `recordShortfall` (every short line; partial, substitute or another hub in the zone), `markHubRequestReady` (the lots scanned; partial with a shortfall, even empty; again changes nothing), `getCassetteLabel`, `listHubLots` and `quarantineLot` (hub lead only; the lot leaves any unsealed cassette).
- Changed: a rider accepting a substitute reopens packing with the substitute in place of what was short; the simulator records its short lines.
- Added: migration 0032, `shortfall_lines` (with the simulator's past shortfalls filled in), the shortfall's reason and who recorded it, and who quarantined a lot and why. Not applied to Supabase.
- Evidence: docs/testing/reports/125.md

### The hub queue (#124)
- Added: `listHubRequests`, `getHubRequest`, `acceptHubRequest` (one acceptance only), `getPickList` (soonest expiry first), `scanHubLot`, `unscanHubLot` and the hub live channel `/v1/hub/stream`.
- Added: migration 0031, `hub_state`, and `seq` and `accepted_by` on requests. Every change the rider sees also raises the hub's seq.
- Changed: the hub simulator stands aside at a hub with active staff.
- Evidence: docs/testing/reports/124.md

### Hub staff sign-in (#123)
- Added: `apps/partner-api`, the partner (hub) API. Hub staff and hub leads sign in with phone and OTP (decision 0014); `GET /hub/me` returns the staff member, their role and hub, and whether the hub is open.
- Added: migration 0030, `hub_staff`, and the staff type on codes, sessions and devices. Not applied to Supabase.
- Added: test staff in `cmd/testdata staff`.
- Evidence: docs/testing/reports/123.md

### Partner contract and mock (#122)
- Added: `contracts/partner/openapi.yaml` and the hub live channel, for the partner (hub) app: staff sign-in by phone and OTP, the queue, accept, pick list, scan, unscan, shortfall, ready, label, lots, quarantine (hub lead only) and the offline event batch.
- Added: the mock serves them at `/partner/v1` with 27 scenarios; CI checks the mock against the contract.
- Evidence: docs/testing/reports/122.md

### Earnings (#109)
- Added: a handover records the order's pay; the day's incentive comes once the rider's handed-over orders reach the level; an accepted move is paid by the started minute until it ends, up to a cap. `getEarnings` returns a local day.
- Added: migration 0029, `earnings`, and test rates as settings: `pay_per_order_paise` (4000), `incentive_orders` (10), `incentive_paise` (20000), `relocation_paise_per_minute` (200), `relocation_max_paid_s` (1200). Not applied to Supabase.
- Evidence: docs/testing/reports/109.md

### Relocation (#108)
- Added: a check every 30 seconds that asks a spare rider (from a node with two or more waiting) to move to the nearest node with recent orders and nobody waiting. `listNodes` shows the suggestion; `acceptRelocation` moves the rider's node and scooter and starts paid time; a ping near the node marks arrival. Ignoring or refusing has no penalty.
- Added: migration 0028, `relocations` and five settings: `relocation_window_s` (1800), `relocation_min_orders` (2), `relocation_ttl_s` (300), `relocation_rest_s` (900), `relocation_arrive_m` (100). Not applied to Supabase.
- Evidence: docs/testing/reports/108.md

### Manual restock and the pickup (#107)
- Added: `createRestock` (merges into a confirmed request at the same hub, replaces a suggestion) and `respondToShortfall` (a partial fill or another hub, which opens a follow-up request there).
- Added: scanning cassette lots moves a request to `picked_up`, then `received`, and settles an open shortfall. A cassette nobody collects 30 minutes after `arrive_by` expires and its lots go back on the shelf.
- Added: migration 0027, `restock_requests.follow_of`. Not applied to Supabase.
- Evidence: docs/testing/reports/107.md

### Restock suggestions (#106)
- Added: a stock check that drafts one suggestion when a scooter runs low, from the nearest hub; `listRestockSuggestions`, `confirmRestock` (with changed quantities) and `declineRestock`; suggestions expire.
- Added: settings `restock_low_qty` (3), `restock_target_qty` (10), `restock_suggestion_ttl_s` (600) and `restock_check_s` (300). Migration 0026, not applied to Supabase.
- Changed: queued `restock_confirm` and `restock_decline` events apply in the offline batch.
- Evidence: docs/testing/reports/106.md

### Hubs and restock data (#105)
- Added: hubs, lots waiting at a hub, restock requests, their lines and packed lots, and shortfalls. Migration 0025, not applied to Supabase.
- Added: `getRestockOptions`, `getRestock`, and the rider's `restock_request` in `getRiderState`.
- Added: a hub simulator, standing in for hub staff until v0.8.0, and a test hub with lots in each test zone.
- Evidence: docs/testing/reports/105.md

### Support and notifications (#101)
- Added: `createTicket`, `getTicket` and `notifyWhenAvailable`.
- Added: a push per customer stage change, following `customer_stage` only, and per new offer for the rider; a support alert for a rejected queued handover. Pushes go through the outbox to a pusher that only logs until FCM is connected.
- Migration 0024, not applied to Supabase.
- Evidence: docs/testing/reports/101.md

### The offline event batch (#98)
- Added: `sendEventBatch`. Queued location, handover, problem and stock scan events apply in order, each on its own, with the same rules as the direct requests. A repeated `event_id` returns its first final result; a direct request sent first with the same key counts once.
- Added: `rider_events`, migration 0023, not applied to Supabase.
- Evidence: docs/testing/reports/98.md

### Rider problems and reassignment (#96)
- Added: `reportProblem`. A breakdown or accident takes the scooter out of service; an order the customer has not seen accepted goes to another rider silently, and one they have seen fails with a full refund (`order_failed`). Other problems are recorded for support.
- Added: when a rider's on-time pings stop for `reassign_after_s` (2 minutes) before the timer, the order is reassigned, unless the customer already has the code; once the timer runs, support is alerted instead.
- Changed: a scooter in maintenance gets no offers.
- Migration 0022, not applied to Supabase.
- Evidence: docs/testing/reports/96.md

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
