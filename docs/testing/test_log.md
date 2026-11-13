# Test log

Index of test evidence. One row per feature (or phase) validated. Evidence is in `reports/`.

| Date | Issue | Feature / phase | Commit | Result | Report |
|---|---|---|---|---|---|
| 2026-11-13 | #237 | Razorpay payments in test mode | See PR | PASS | [reports/237.md](reports/237.md) |
| 2026-11-13 | #238 | security headers (HSTS and more), govulncheck and gitleaks in CI, Dependabot, Go 1.27.4 | See PR | PASS | [reports/238.md](reports/238.md) |
| 2026-11-13 | #174 | staging database moved to Singapore; writes 1-3.6 s down to about 0.3 s | See PR | PASS | [reports/174.md](reports/174.md) |
| 2026-11-13 | #226 | relocation v2 behind relocation_by_plan (off) | See PR | PASS | [reports/226.md](reports/226.md) |
| 2026-11-13 | #192 | an expired confirmed suggestion raises the hub's seq | See PR | PASS | [reports/192.md](reports/192.md) |
| 2026-11-13 | #224 | placement plans per zone, weekday and half-hour | See PR | PASS | [reports/224.md](reports/224.md) |
| 2026-11-13 | #222 | uneven demand in the simulator, placement on demand per cell | See PR | PASS | [reports/222.md](reports/222.md) |
| 2026-11-13 | #220 | demand per cell and half-hour, forecast v1 | See PR | PASS | [reports/220.md](reports/220.md) |
| 2026-11-13 | #217 | tiny negative OSRM numbers count as 0 | See PR | PASS | [reports/217.md](reports/217.md) |
| 2026-11-13 | #216 | placement by maximum expected covering, in the simulator | See PR | PASS | [reports/216.md](reports/216.md) |
| 2026-11-13 | #214 | the simulator gets fleet state | See PR | PASS | [reports/214.md](reports/214.md) |
| 2026-11-13 | #211 | reach by road time when matching_by_road is on | See PR | PASS | [reports/211.md](reports/211.md) |
| 2026-11-13 | #209 | road-time matching behind a setting | See PR | PASS | [reports/209.md](reports/209.md) |
| 2026-11-13 | #207 | live rider positions and a rider's tier for a gate | See PR | PASS | [reports/207.md](reports/207.md) |
| 2026-11-13 | #205 | the geo simulator v1 | See PR | PASS | [reports/205.md](reports/205.md) |
| 2026-11-13 | #203 | export a gate's tiers as GeoJSON | See PR | PASS | [reports/203.md](reports/203.md) |
| 2026-11-13 | #199 | tiers from road times and settings | See PR | PASS | [reports/199.md](reports/199.md) |
| 2026-11-13 | #200 | renumber 0047_gate_cells to 0048 (two 0047s on main) | See PR | PASS | [reports/200.md](reports/200.md) |
| 2026-11-13 | #196 | road-time cell sets per gate | See PR | PASS | [reports/196.md](reports/196.md) |
| 2026-11-13 | #193 | the H3 spike and the pilot's hexagon grid | See PR | PASS | [reports/193.md](reports/193.md) |
| 2026-11-13 | #150 | console live map, the server side (v0.10.0 E) | See PR | PASS | [reports/150.md](reports/150.md) |
| 2026-11-13 | #190 | road data versions | See PR | PASS | [reports/190.md](reports/190.md) |
| 2026-11-13 | #187 | the golden routes in CI | See PR | PASS | [reports/187.md](reports/187.md) |
| 2026-11-13 | #149 | console hub operations: queue, stuck requests and expiring one, the server side (v0.10.0 D) | See PR | PASS | [reports/149.md](reports/149.md) |
| 2026-11-12 | #183 | OSRM on Bengaluru, the scooter profile and golden routes | See PR | PASS | [reports/183.md](reports/183.md) |
| 2026-11-12 | #180 | the OSRM routing client with a straight-line fallback | See PR | PASS | [reports/180.md](reports/180.md) |
| 2026-11-12 | #175 | more test riders and scooters on staging (`testdata testers`) | See PR | PASS | [reports/175.md](reports/175.md) |
| 2026-11-12 | #169 | geo work's own local Postgres (5433) and Redis (6381) | See PR | PASS | [reports/169.md](reports/169.md) |
| 2026-11-12 | #148 | console incidents: one queue for what needs a person, the server side (v0.10.0 C) | See PR | PASS | [reports/148.md](reports/148.md) |
| 2026-11-12 | #147 | console support actions: tickets, refunds, replacements and credits, the server side (v0.10.0 B) | See PR | PASS | [reports/147.md](reports/147.md) |
| 2026-11-12 | #146 | console order search and timeline, the server side (v0.10.0 A) | See PR | PASS | [reports/146.md](reports/146.md) |
| 2026-11-12 | #178 | customer app additions: veg flag, scooter plate, notification settings, gate landmark and type (part A, contract and mock) | See PR | PASS | [reports/178.md](reports/178.md) |
| 2026-11-12 | #178 | customer app additions, the server (part B): migration 0043, is_veg, plate, notification settings and the pusher, gate landmark and type | See PR | PASS | [reports/178.md](reports/178.md) |
| 2026-11-11 | #159 | rider operations, the server side (v0.10.0) | See PR | PASS | [reports/159.md](reports/159.md) |
| 2026-11-11 | #157 | the rider app's new operations, contract and mock | See PR | PASS | [reports/157.md](reports/157.md) |
| 2026-11-11 | #162 | geo design and research record (docs only) | See PR | PASS | [reports/162.md](reports/162.md) |
| 2026-11-11 | #164 | `v0.10.0` console ops contract and mock | See PR | PASS | [reports/164.md](reports/164.md) |
| 2026-11-11 | #158 | the rider app's fields | See PR | PASS | [reports/158.md](reports/158.md) |
| 2026-11-11 | #156 | rider verification and route endpoints served | See PR | PASS | [reports/156.md](reports/156.md) |
| 2026-11-11 | #151 | masked calls answer call_not_available | See PR | PASS | [reports/151.md](reports/151.md) |
| 2026-11-11 | #152 | console sign-in with email; authenticator optional until pre-launch | See PR | PASS | [reports/152.md](reports/152.md) |
| 2026-11-11 | #138 | `v0.9.0` audit log, arrival reviews and support alerts | See PR | PASS | [reports/138.md](reports/138.md) |
| 2026-11-11 | #137 | `v0.9.0` menu and prices, gates, hubs and hub staff | See PR | PASS | [reports/137.md](reports/137.md) |
| 2026-11-11 | #136 | `v0.9.0` zones, settings and the kill switch | See PR | PASS | [reports/136.md](reports/136.md) |
| 2026-11-11 | #135 | `v0.9.0` rider review in the console | See PR | PASS | [reports/135.md](reports/135.md) |
| 2026-11-11 | #134 | `v0.9.0` console sign-in, users, approvals and flags | See PR | PASS | [reports/134.md](reports/134.md) |
| 2026-11-11 | #133 | `v0.9.0` console contract and mock | See PR | PASS | [reports/133.md](reports/133.md) |
| 2026-11-11 | #127 | `v0.8.0` the real hub flow, local only | See PR | PASS | [reports/127.md](reports/127.md) |
| 2026-11-11 | #126 | `v0.8.0` hub offline sync, local only | See PR | PASS | [reports/126.md](reports/126.md) |
| 2026-11-11 | #125 | `v0.8.0` shortfall, sealing and quarantine, local only | See PR | PASS | [reports/125.md](reports/125.md) |
| 2026-11-10 | #124 | `v0.8.0` the hub queue, local only | See PR | PASS | [reports/124.md](reports/124.md) |
| 2026-11-10 | #123 | `v0.8.0` hub staff sign-in, local only | See PR | PASS | [reports/123.md](reports/123.md) |
| 2026-11-10 | #122 | `v0.8.0` partner contract and mock | See PR | PASS | [reports/122.md](reports/122.md) |
| 2026-11-10 | #120 | `v0.7.0` on staging, test data only | 0ee6692 | PASS: a restock suggestion confirmed with a changed quantity; a second request merged into it; 500 units refused as hub_out_of_stock; the simulator moved it to hub_accepted (with a slot), packing and ready; scanning the cassette's lot made it received and loaded 10 units; a handed-over order earned 4000 paise, shown by getEarnings, and a bad date was 400; listNodes carries the relocation field and an unknown move is 404; no errors in the logs. Not run on staging: shortfall answers and relocation moves, which need a second hub and several riders (tested locally, #107 and #108) | — |
| 2026-11-10 | #109 | `v0.7.0` earnings, local only | See PR | PASS | [reports/109.md](reports/109.md) |
| 2026-11-10 | #108 | `v0.7.0` relocation, local only | See PR | PASS | [reports/108.md](reports/108.md) |
| 2026-11-10 | #107 | `v0.7.0` manual restock and the pickup, local only | See PR | PASS | [reports/107.md](reports/107.md) |
| 2026-11-10 | #106 | `v0.7.0` restock suggestions, local only | See PR | PASS | [reports/106.md](reports/106.md) |
| 2026-11-10 | #105 | `v0.7.0` hubs, restock data and the simulator, local only | See PR | PASS | [reports/105.md](reports/105.md) |
| 2026-11-10 | #112 | `v0.6.0` on staging, test data only | 7b652d8 | PASS: missed timer owed a credit; problem report recorded; queued pings and a queued handover of one of two accepted, repeat answered the same; missing item refunded (missing_food, 14900), payment still paid; ticket created and read back; notify me saved; 3 pushes logged by the stand-in | This row |
| 2026-11-10 | #101 | `v0.6.0` support and notifications, local only | See PR | PASS | [reports/101.md](reports/101.md) |
| 2026-11-09 | #98 | `v0.6.0` the offline event batch, local only | See PR | PASS | [reports/98.md](reports/98.md) |
| 2026-11-09 | #96 | `v0.6.0` rider problems and reassignment, local only | See PR | PASS | [reports/96.md](reports/96.md) |
| 2026-11-09 | #94 | `v0.6.0` remedies and the longer ETA, local only | See PR | PASS | [reports/94.md](reports/94.md) |
| 2026-11-09 | #92 | `v0.5.0` on staging, test data only | 589c648 | PASS: order, test payment, offer, accept (1.5 mins), Ready, pings 1 km, 280 m, 270 m (timer started, 90 s), 55 m, 50 m (arrived, not missed, code shown), wrong code 409, handover 200; customer stream through every stage and closed at handover; rider stream offer, accept, handover | This row |
| 2026-11-09 | #90 | Streams outlive the server's limits, local | See PR | PASS | [reports/90.md](reports/90.md) |
| 2026-11-09 | #88 | `v0.5.0` live streams, local only | See PR | PASS | [reports/88.md](reports/88.md) |
| 2026-11-09 | #86 | `v0.5.0` the zone timer and handover, local only | See PR | PASS | [reports/86.md](reports/86.md) |
| 2026-11-09 | #84 | `v0.5.0` offers, local only | See PR | PASS | [reports/84.md](reports/84.md) |
| 2026-11-09 | #82 | `v0.4.0` on staging, test data only | f352483 | PASS: create 201, test payment paid, cancel inside 5 s refunded (customer_cancelled, processed), failed then retry then cancel before paying (no refund), too many gives sold_out, list, stock back to 10 | This row |
| 2026-11-09 | #80 | `v0.4.0` orders part B, local only | See PR | PASS | [reports/80.md](reports/80.md) |
| 2026-11-09 | #78 | `v0.4.0` orders part A, local only | See PR | PASS | [reports/78.md](reports/78.md) |
| 2026-11-09 | #74 | `v0.3.0` on staging, the real URLs | See PR | PASS | [reports/74.md](reports/74.md) |
| 2026-11-07 | #72 | `v0.3.0` availability, local only | See PR | PASS | [reports/72.md](reports/72.md) |
| 2026-11-07 | #70 | `v0.3.0` stock and the rider shift, local only | See PR | PASS | [reports/70.md](reports/70.md) |
| 2026-11-07 | #68 | `v0.3.0` menu, local only | See PR | PASS | [reports/68.md](reports/68.md) |
| 2026-11-07 | #66 | `v0.3.0` zones, gates and the location check, local only | See PR | PASS | [reports/66.md](reports/66.md) |
| 2026-11-07 | #64 | `v0.3.0` settings store, local only | See PR | PASS | [reports/64.md](reports/64.md) |
| 2026-11-07 | #60 | `v0.2.0` staging deploy, on the real URLs | See PR | PASS | [reports/60.md](reports/60.md) |
| 2026-11-06 | #58 | `v0.2.0` profile and devices, local only | See PR | PASS | [reports/58.md](reports/58.md) |
| 2026-11-06 | #39 | `v0.2.0` rate limits, local only | See PR | PASS | [reports/39.md](reports/39.md) |
| 2026-11-06 | #54 | `v0.2.0` one sign-in method per customer, local only | See PR | PASS | [reports/54.md](reports/54.md) |
| 2026-11-06 | #47 | `v0.2.0` Google sign-in, phone verification and merging, local only, with fake signed tokens | See PR | PASS | [reports/47.md](reports/47.md) |
| 2026-11-06 | #43 | `v0.2.0` phone OTP sign-in, test codes, local only | See PR | PASS | [reports/43.md](reports/43.md) |
| 2026-11-06 | #38 | `v0.2.0` session system, local only | See PR | PASS | [reports/38.md](reports/38.md) |
| 2026-11-05 | #36 | `v0.2.0` idempotency, audit and outbox, local only | See PR | PASS | [reports/36.md](reports/36.md) |
| 2026-11-05 | #32 | `v0.2.0` service skeleton, local only | See PR | PASS | [reports/32.md](reports/32.md) |
| 2026-11-05 | #30 | `v0.2.0` migrations 0007 and 0008 applied to Supabase | See PR | PASS | [reports/30.md](reports/30.md) |
| 2026-11-05 | #28 | `v0.2.0` sign-in contract, mock and tables | See PR | PASS | [reports/28.md](reports/28.md) |
| 2026-11-05 | #22 | `v0.2.0` migrations applied to Supabase | See PR | PASS | [reports/22.md](reports/22.md) |
| 2026-11-05 | #21 | `v0.2.0` database foundation, local Postgres only | See PR | PASS | [reports/21.md](reports/21.md) |
| 2026-11-05 | #8 | `v0.1.0` mock server | See PR | PASS | [reports/8.md](reports/8.md) |
| 2026-11-04 | #15 | `v0.1.0` cancel and refund rule | See PR | PASS | [reports/15.md](reports/15.md) |
| 2026-11-04 | #7 | `v0.1.0` rider contract | See PR | PASS | [reports/7.md](reports/7.md) |
| 2026-11-04 | #6 | `v0.1.0` customer contract | See PR | PASS | [reports/6.md](reports/6.md) |
| 2026-11-04 | #5 | `v0.1.0` shared API rules | See PR | PASS | [reports/5.md](reports/5.md) |

Result values: PASS · FAIL · PARTIAL (explain in the report).
