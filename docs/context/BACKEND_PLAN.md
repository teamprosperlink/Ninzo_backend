# Ninzo Backend Plan (Developer 1)

Source: PRD + the 9 KT master docs. I could not find the frontend code, so every contract below comes from the docs, not from the Flutter or Next.js repos. The ops doc mentions "the backend handover", which is not in the KT folder. Please share it.

## 1. What the backend has to guarantee

The PRD promise: after the server **accepts** an order, a scooter reaches the customer's gate within 90 s. Everything else follows from this.

1. **One clock, owned by the server.** `accepted_at` is server-signed. No device can change it.
2. **No promise without checks.** Stock, route, rider and gate checks pass before a 90 s offer is shown. If they fail, withdraw the offer instead of stretching it.
3. **Stock is never oversold.** Reservations have a TTL and are atomic.
4. **Every write is idempotent.** Taps, retries and offline replays are all safe.
5. **Every miss has an automatic remedy** (refund, credit or replacement) and an audit trail.

## 2. Architecture recommendation

The architecture doc says "microservices". For a pilot with 4-8 scooters and 1-3 menu items, I recommend a **modular monolith in Go** (chi, pgx, sqlc, already decided in the ops doc), with the **tracker as a separate process**. Split further only when a module has a real scaling or team reason. The module boundaries below make that split cheap.

| Piece | Choice | Why |
|---|---|---|
| API and WS gateway | Go, chi | Decided in the ops doc |
| System of record | PostgreSQL + PostGIS | Orders, stock, ledger, audit |
| Live state | Redis | Scooter positions, H3 cell to scooter sets, pub/sub for WS fan-out. **Never the source of truth for stock.** |
| Event bus | **NATS JetStream** (or a Postgres outbox to start) | Kafka is overkill for the pilot. Same event-driven shape, far less ops. |
| Routing | Self-hosted **OSRM or Valhalla** with an OSM extract of the pilot city | CH is built in. A Google Routes API fallback for the live ETA check is optional. |
| Spatial index | H3 | Sweet spot stored as a set of cells |
| Placement optimizer | OR-Tools in an **offline Python job** | Not in the request path |
| Payments | Razorpay (webhook-driven) | See risk R3 |
| Push / SMS | FCM / SMS gateway (test OTP until DLT is done) | |
| Observability | OpenTelemetry, Prometheus, Grafana, Sentry | Per the architecture doc |

Modules (packages in one repo):

```
identity   OTP, staff login+TOTP, sessions, RBAC permission table
geo        gates, zones, staging nodes, sweet-spot precompute, H3
catalog    menu, prices, availability rules
inventory  lots, scooter stock ledger, reservations (TTL)
orders     order state machine, event log, cancel/refund hooks
payments   Razorpay order/auth/capture/refund, webhook
dispatch   candidate filter, ranking, batch assignment, offers
tracker    location ingest, smoothing, geofence, hysteresis (own process)
restock    suggestion engine, request lifecycle, hub queue slots
hub        hub staff API: pick, scan, shortfall, cassette, labels
ops        live feed, zone kill switch, rules, timers
support    timeline, refunds, replacements, incidents, masked calls
audit      append-only log written in the same tx as every change
notify     FCM/SMS templates per state
realtime   WS gateways: customer, rider, hub, ops
```

## 3. API conventions (all apps depend on these)

- Prefix `/v1`. Hub is `/v1/hub`, ops is `/v1/ops`, `/v1/admin`, `/v1/support`.
- **Every response carries `server_time`.**
- Errors have one shape: `{code, message, retryable}`. A 403 names the missing permission.
- `Idempotency-Key` is required on every repeatable write. Store `(key, actor, route, request_hash, response)` for 24 h. A replay with the same key returns the stored response. A different body with the same key returns 422.
- WS: the token goes in the first frame. Every event has `seq` and `server_time`. Clients reconnect and re-GET state first, so keep GET-state endpoints cheap and authoritative.
- Offline batches (`/riders/me/events`, `/hub/events`): apply in the order given, dedupe by `event_id`, return `accepted_ids[]` and `rejected[{id, reason}]`.
- Cursors for lists, no offset paging.

## 4. Core data model

Postgres tables (names indicative):

- **identity:** `customers`, `riders` (+`rider_verifications`), `staff` (+`roles`, `permissions`, `role_permissions`), `sessions`, `otp_requests`
- **geo:** `zones` (time_budget_s, gate_cap, offer_90s_enabled, version), `gates` (geography point, entry_notes, verified, version) + `gate_history`, `staging_nodes` (cap), `sweet_spots` (gate_id, h3_cells[], per-cell travel_s, computed_at, graph_version)
- **catalog:** `menu_items`, `price_history`, `availability_rules`
- **inventory:** `lots` (item_id, prepared_at, expires_at, hub_id, state: ok/quarantined), `scooter_stock` (scooter_id, lot_id, qty, temp_status), `stock_ledger` (append-only: scan_in, reserve, release, commit, handover, waste), `reservations` (order_id, scooter_id, lot_id, qty, expires_at, state)
- **orders:** `orders`, `order_items`, `order_legs` (one scooter's share), `order_events` (event sourced: state, at, evidence, actor, source), `offers` (order_id, rider_id, expires_at, state, fencing_token)
- **payments:** `payments` (razorpay ids, state), `refunds`, `webhook_events` (dedupe)
- **restock:** `restock_requests` (+states from the restock doc), `restock_lines`, `hub_queue_slots`, `shortfalls`
- **ops:** `settings` (timers, rules, with version), `flags`, `approvals`, `incidents`, `support_tickets` (Chatwoot link), `audit_events`
- **tracker:** `shifts`, `location_pings` (partitioned by day, short retention), `gps_anomalies`

Rules: `version` columns for optimistic concurrency (the gate edit conflict scenario). The audit row is written **in the same transaction** as the change.

## 5. The order flow

Order states (event sourced, one transition function, illegal transitions rejected):

```
CREATED -> PAYMENT_PENDING -> PAYMENT_AUTHORIZED -> MATCHING
MATCHING -> OFFERED -> RIDER_ACCEPTED -> PACKING -> READY -> ACCEPTED (90 s clock starts)
ACCEPTED -> EN_ROUTE -> ARRIVED -> HANDED_OVER -> COMPLETED
Exits: CANCELLED (pre-acceptance), NO_SCOOTER, TIMED_OUT, PAYMENT_FAILED, MISSED_PROMISE (late, auto-remedy), FAILED (breakdown etc.)
```

Sequence at order creation (`POST /v1/orders`, Idempotency-Key):

1. Validate the gate (verified, inside zone, pin unchanged), the zone kill switch, and the per-gate concurrent-order cap.
2. **Availability check** = sweet-spot cells of the gate, intersected with the scooters in those cells that have stock, are online and are not on restock. Compute the cheap check first, then confirm with a live A* ETA (OSRM). If it fails, return "no 90 s offer" with the honest ETA.
3. **Reserve stock atomically.** `SELECT ... FOR UPDATE SKIP LOCKED` on the candidate scooter's stock rows, insert a `reservations` row with `expires_at`, and write the ledger entry. This is the saga step "reserve". The compensation is release (TTL expiry or cancel).
4. Create the Razorpay order and return `razorpay_order_id`. The app pays. The **webhook** moves the order to PAYMENT_AUTHORIZED. The app never marks it paid.
5. Dispatch offers to the top 1-2 riders by p90 ETA, then by longest idle time. The offer has a short TTL and a **fencing token**, so only the holder's accept is valid.
6. The rider accepts, stops, packs and presses Ready (`/offers/{id}/ready`). The server checks that the rider's latest speed is plausible, then runs the **final live ETA check**. If it still fits the budget, **`ACCEPTED`**: the server signs `accepted_at`, captures the payment (idempotent) and commits the reservation (the saga "commit"). If it does not fit, run the compensations and re-match.
7. En route: the tracker pushes progress. Arrival needs evidence (QR or code plus geofence, never GPS alone). Handover needs the OTP plus quantities. Completion decrements stock for good.
8. If the order is not accepted within the timeout, cancel, release stock and void the authorization.

Remedy automation: if `handed_over_at - accepted_at > 90 s`, or the order fails after acceptance, write a `MISSED_PROMISE` event and trigger the stated refund or credit. The rider is never penalised for a lawful, safe delay.

## 6. Dispatch and sweet spot

- **Sweet-spot precompute (offline, per gate):** reverse Dijkstra from the gate over the road graph with a 55 s cutoff (Valhalla isochrone or OSRM one-to-many), converted to H3 cells with per-cell travel seconds. Recompute when the graph, the zone weights or the gate changes. Store `graph_version`.
- **Staging nodes:** chosen by set cover (greedy) or ILP in the offline job, each with a cap.
- **Tracker:** ingest pings (202 immediately, process async), drop implausible speed jumps, Kalman-smooth, map-match, then map to an H3 cell. **Hysteresis:** two consecutive in-zone pings before a state flips. Redis keeps `scooter:{id}` and `cell:{h3} -> set(scooter)`.
- **Matching:** at pilot scale (4-8 scooters) a scored brute-force over candidates is enough and is deterministic. The Hungarian batch (every 1-2 s) and min-cost max-flow come in when order bursts justify them. Keep the dispatcher behind an interface so this swap is cheap.
- **Multi-scooter (6+4) orders:** bottleneck assignment plus subset enumeration of 2-3 scooters. Build this last, behind a flag. The pilot has 1-3 items, so start single-leg.
- **Rebalancing:** every few minutes, with node caps, a marginal-coverage score, a cooldown and a "relocate only if the gain beats a threshold" rule. Repositioning is paid time (record `repositioning_seconds` for earnings).

## 7. Restock and the hub

- Reorder point plus shelf-life check drafts a `Suggested` request. The rider confirms, edits or declines. The states are Suggested → Confirmed → Hub accepted → Packing → Ready → Picked up → Received → Closed, plus Expired / Cancelled / Partial.
- Enforce: one open request per scooter (merge duplicates); a per-zone cap on scooters away; a scooter on restock is not offered orders; zero stock removes it from search at once, **after settling open reservations**; hub queue slots; a late hub slot is reassigned and the rider is told.
- Hub pick list ordering is soonest valid expiry first, with expired lots impossible to pick. Wrong lot or temperature failure is rejected and the request stays open. Quarantine blocks the lot everywhere, behind an approval.
- `Ready` records the lot numbers in the cassette. The rider's scan at pickup checks them against the cassette. A mismatch is rejected and the lot is quarantined.

## 8. Ops, admin, support

- RBAC with a central permission table, checked in middleware on every route. Test it with a role × endpoint matrix. Staff TOTP is required for admin and approvers.
- `approvals` table for two-person actions (refund above limit, quarantine, flags, kill switch). A write returns `needs_approval`, and the second person calls the approve endpoint.
- The kill switch and zone settings are versioned. Changes need a reason and are audited, and the dispatcher reads them from cache with fast invalidation, because a kill switch that lags is a safety bug.
- Masked calls through a telephony provider (not chosen yet). Support never sees full phone numbers.

## 9. Build order

Phase 0 (week 1, the blocking one: three frontend teams wait on it)
1. One **OpenAPI 3.1 file** for customer, rider, hub, ops, admin and support. Use the conventions above. Resolve the contract conflicts in section 11.
2. **Prism mock** with `X-Mock-Scenario` for every scenario the docs list, plus a WS replay mock per channel.
3. Fixtures: test gates, items, lots, riders (verified and unverified), a test hub, six staff roles, test OTP numbers.
4. CI: Schemathesis against the spec, plus the Bruno collection.

Phase 1: foundations
- Repo, migrations, config, auth (OTP + staff + TOTP), idempotency middleware, audit, outbox, WS gateway with seq and server_time.
- Gates, zones, menu. Location ingest returning 202 (unblocks the rider's background-location spike).

Phase 2: the 90 s core (the staging URL with Razorpay test keys)
- Inventory ledger and reservations, orders state machine, Razorpay + webhook, sweet-spot precompute, availability, single-leg dispatch, offers, ready, acceptance, arrival, handover, cancel, refund.
- A **simulator** (SimPy or custom) replaying scooters and orders, used to set the default time budget, gate cap and node caps.

Phase 3: hub and restock (real hub data for the scan test)
- Lots, restock engine, hub APIs, offline event batches, labels, shortfall, quarantine.

Phase 4: ops and support
- Live feed and map data, kill switch, rules and timers, refunds with approvals, timeline, replacements, incidents, audit viewer.

Phase 5: hardening before the pilot
- Rebalancing, dead-zone alerts, GPS spoof checks, rain/closure edge weights, load test the reservation path, chaos tests (drop WS, replay events, double webhooks), security scan, internal-tester week.

Later (per the future-suggestions doc): demand forecast from rolling averages, then a model, then rider nudges, with auto-submit restock. None of it may override safety rules.

## 10. Test priorities (write these first)

- Concurrency: 50 parallel orders for the last unit produce exactly one winner and no negative stock.
- Idempotency: double tap, double webhook, double accept, double Ready and double handover each produce exactly one effect.
- Fencing: a stale rider's accept after re-offer is rejected.
- Clock: an offline arrival event that is replayed later does not change the official time.
- Saga: payment fails after reserve → stock released. The rider drops before acceptance → release and rematch.
- Offline batch: 20 min of events, replayed twice, applied in order once.
- RBAC matrix, audit-in-transaction, and the kill switch propagating within seconds.

## 11. Gaps and contradictions I found in the docs (please decide)

**Contract conflicts**
1. **Handover is defined twice.** The customer app posts `/v1/orders/{id}/handover-confirm` with an OTP. The rider app posts `/v1/orders/{id}/handover` with an OTP and quantities. Recommendation: the rider submits the customer's OTP and quantities (one proof), and the customer endpoint becomes read-only confirmation, or is removed.
2. Hub paths: the earlier backend draft had no `/v1` prefix. Align on `/v1/hub`.
3. Quarantine exists at `/v1/hub/lots/{id}/quarantine` and `/v1/ops/lots/{id}/quarantine/approve`, and `/internal/lots/{id}/quarantine` in the architecture doc. Make it one flow: hub requests, ops approves.
4. `POST /v1/offers/{id}/ready`: the server needs the rider's speed or a "stationary" proof. The doc has only a client-side lock. Enforce it server-side from the last location pings.

**Missing endpoints / features nobody owns**
5. **Who creates lots?** The kitchen produces and labels lots, but no kitchen API exists (lot creation, batch view, production plan, par levels and the hub → central kitchen request mentioned in the restock doc).
6. **Temperature data source.** The PRD sells "temperature-tracked", but the only input is `temp_status` on a scan. If scooters have sensors, you need a telemetry ingest and breach rules. If not, decide what "tracked" means.
7. **Customer C9 options** (wait, switch, notify me) have no endpoints. Also missing: FCM device-token registration, zones/service-area lookup, gate verification flow (`verified` exists but nothing sets it), `GET /v1/orders/{id}/route` for the rider is listed, but the customer's rough scooter position feed is not.
8. **Customer consent to a longer ETA.** Needs an explicit "accept slower offer" endpoint, so the order is never silently re-promised.
9. Support/ops "tickets" are linked to Chatwoot, but the webhook or sync direction is not defined.

**Product and feasibility risks**
- **R1. Time budget math.** The travel cutoff is 55 s of a 90 s promise. The remaining 35 s is not accounted for: what is it for (handover? OTP? packing)? At about 20-25 km/h a 55 s trip covers only about 300-380 m, so each gate is covered by very few staging nodes. The simulator (Phase 2) should settle the budget split and the node count before launch.
- **R2. "Accepted" semantics.** Per the docs, the rider packs *before* acceptance. A customer who has already paid waits through offer, accept and pack with no guaranteed time. I assumed the "finding a scooter" screen covers this with a timeout. Please confirm, and set that timeout.
- **R3. Payment hold.** The customer doc says "payment is held, then taken at acceptance". Razorpay authorize-then-capture works well for cards, but **UPI generally can't do a hold/capture**, and the auth window is limited. Check what the pilot city mostly uses. If it is UPI, the alternative is to charge at order and auto-refund on failure, which changes the customer copy ("no payment taken").
- **R4. Three server-side timers** (offer TTL, reservation TTL, matching timeout) must nest correctly: reservation TTL > matching timeout > offer TTL. Make them settings, not constants.
- **R5. Time zone / clock.** Fine as designed (server time everywhere), but keep NTP-synced hosts and one monotonic source for `accepted_at`.
- **R6. Legal items are mocked:** DigiLocker, DLT SMS and FSSAI (food labelling and expiry rules affect lot and label fields). Design the schema now so these slot in later, but they are on the critical path to a real pilot.
- **R7. Data residency.** The ops doc wants India-region hosting for Chatwoot and Metabase. Pick the backend host with that in mind.

## 12. Questions that block or shape the work

1. Where is the frontend code, and the "backend handover" doc?
2. Do I own only the backend, or also the partner app (the doc says Developer 1 owns it) and the tracker?
3. Pilot city and its road/map extract, which sets OSRM or Valhalla data and the payment-method mix (R3).
4. Do scooters have temperature sensors (R1/6)?
5. Is iOS in the pilot for customers and riders? This changes mostly the frontends, but also push setup.
6. The refund limit and the second-approver rule (product and finance), and who may quarantine alone.
