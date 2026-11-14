# Test log

Index of test evidence. One row per feature (or phase) validated. Evidence is in `reports/`.

**Commit** is the merge commit on `main` of the PR that shipped the change. It holds the code the report's tests ran on; the report names the branch commit. Rows before #259 were backfilled from GitHub, by the PR that closed each row's Issue. **Date** is the report's date, in IST. Benchmarks (simulation and performance) are recorded separately in [`docs/benchmarks/`](../benchmarks/README.md).

| Date | Issue | Feature / phase | Commit | Result | Report |
|---|---|---|---|---|---|
| 2026-11-14 | #252 | push notifications over FCM, refund processed push | [a6d40fc](https://github.com/teamprosperlink/Ninzo_backend/commit/a6d40fc) (PR #254) | PASS | [reports/252.md](reports/252.md) |
| 2026-11-14 | #249 | relocation by plan: non-test scooters for n, the forecast in orders_waiting | [3c52609](https://github.com/teamprosperlink/Ninzo_backend/commit/3c52609) (PR #250) | PASS | [reports/249.md](reports/249.md) |
| 2026-11-13 | #237 | Razorpay payments in test mode | [f0d3188](https://github.com/teamprosperlink/Ninzo_backend/commit/f0d3188) (PR #246) | PASS | [reports/237.md](reports/237.md) |
| 2026-11-13 | #238 | security headers (HSTS and more), govulncheck and gitleaks in CI, Dependabot, Go 1.27.4 | [10b5ed1](https://github.com/teamprosperlink/Ninzo_backend/commit/10b5ed1) (PR #239) | PASS | [reports/238.md](reports/238.md) |
| 2026-11-13 | #235 | rider app (APK) compatibility, part A: contract and mock (52 calls: 28 a, 13 b, 11 c) | [b082ab8](https://github.com/teamprosperlink/Ninzo_backend/commit/b082ab8) (PR #240) | PASS | [reports/235-A.md](reports/235-A.md) |
| 2026-11-13 | #235 | rider app (APK) compatibility, part B1: fields on existing replies, break aliases, invalid_phone (migration 0056) | [26819a6](https://github.com/teamprosperlink/Ninzo_backend/commit/26819a6) (PR #247) | PASS | [reports/235-B1.md](reports/235-B1.md) |
| 2026-11-14 | #235 | rider app (APK) compatibility, part B2: goOnline, setPlannedEnd, settings, help, feedback, shift plans, and their console operations (migration 0058) | [fef4037](https://github.com/teamprosperlink/Ninzo_backend/commit/fef4037) (PR #251) | PASS | [reports/235-B2.md](reports/235-B2.md) |
| 2026-11-13 | #174 | staging database moved to Singapore; writes 1-3.6 s down to about 0.3 s | [5215139](https://github.com/teamprosperlink/Ninzo_backend/commit/5215139) (PR #233) | PASS | [reports/174.md](reports/174.md) |
| 2026-11-13 | #226 | relocation v2 behind relocation_by_plan (off) | [8f52014](https://github.com/teamprosperlink/Ninzo_backend/commit/8f52014) (PR #230) | PASS | [reports/226.md](reports/226.md) |
| 2026-11-13 | #192 | an expired confirmed suggestion raises the hub's seq | [7c88584](https://github.com/teamprosperlink/Ninzo_backend/commit/7c88584) (PR #229) | PASS | [reports/192.md](reports/192.md) |
| 2026-11-13 | #224 | placement plans per zone, weekday and half-hour | [08c8a24](https://github.com/teamprosperlink/Ninzo_backend/commit/08c8a24) (PR #225) | PASS | [reports/224.md](reports/224.md) |
| 2026-11-13 | #222 | uneven demand in the simulator, placement on demand per cell | [79d76a7](https://github.com/teamprosperlink/Ninzo_backend/commit/79d76a7) (PR #223) | PASS | [reports/222.md](reports/222.md) |
| 2026-11-13 | #220 | demand per cell and half-hour, forecast v1 | [a20830b](https://github.com/teamprosperlink/Ninzo_backend/commit/a20830b) (PR #221) | PASS | [reports/220.md](reports/220.md) |
| 2026-11-13 | #217 | tiny negative OSRM numbers count as 0 | [a19e372](https://github.com/teamprosperlink/Ninzo_backend/commit/a19e372) (PR #218) | PASS | [reports/217.md](reports/217.md) |
| 2026-11-13 | #216 | placement by maximum expected covering, in the simulator | [78eca7e](https://github.com/teamprosperlink/Ninzo_backend/commit/78eca7e) (PR #219) | PASS | [reports/216.md](reports/216.md) |
| 2026-11-13 | #214 | the simulator gets fleet state | [98fd1ed](https://github.com/teamprosperlink/Ninzo_backend/commit/98fd1ed) (PR #215) | PASS | [reports/214.md](reports/214.md) |
| 2026-11-13 | #211 | reach by road time when matching_by_road is on | [0d2d551](https://github.com/teamprosperlink/Ninzo_backend/commit/0d2d551) (PR #212) | PASS | [reports/211.md](reports/211.md) |
| 2026-11-13 | #209 | road-time matching behind a setting | [4eff699](https://github.com/teamprosperlink/Ninzo_backend/commit/4eff699) (PR #210) | PASS | [reports/209.md](reports/209.md) |
| 2026-11-13 | #207 | live rider positions and a rider's tier for a gate | [8986866](https://github.com/teamprosperlink/Ninzo_backend/commit/8986866) (PR #208) | PASS | [reports/207.md](reports/207.md) |
| 2026-11-13 | #205 | the geo simulator v1 | [3610e05](https://github.com/teamprosperlink/Ninzo_backend/commit/3610e05) (PR #206) | PASS | [reports/205.md](reports/205.md) |
| 2026-11-13 | #203 | export a gate's tiers as GeoJSON | [49fb5b8](https://github.com/teamprosperlink/Ninzo_backend/commit/49fb5b8) (PR #204) | PASS | [reports/203.md](reports/203.md) |
| 2026-11-13 | #199 | tiers from road times and settings | [d0b9d54](https://github.com/teamprosperlink/Ninzo_backend/commit/d0b9d54) (PR #202) | PASS | [reports/199.md](reports/199.md) |
| 2026-11-13 | #200 | renumber 0047_gate_cells to 0048 (two 0047s on main) | [0b05b5b](https://github.com/teamprosperlink/Ninzo_backend/commit/0b05b5b) (PR #201) | PASS | [reports/200.md](reports/200.md) |
| 2026-11-13 | #196 | road-time cell sets per gate | [e31528e](https://github.com/teamprosperlink/Ninzo_backend/commit/e31528e) (PR #198) | PASS | [reports/196.md](reports/196.md) |
| 2026-11-13 | #193 | the H3 spike and the pilot's hexagon grid | [ca10c67](https://github.com/teamprosperlink/Ninzo_backend/commit/ca10c67) (PR #195) | PASS | [reports/193.md](reports/193.md) |
| 2026-11-13 | #150 | console live map, the server side (v0.10.0 E) | [dd32f0f](https://github.com/teamprosperlink/Ninzo_backend/commit/dd32f0f) (PR #197) | PASS | [reports/150.md](reports/150.md) |
| 2026-11-13 | #190 | road data versions | [10a5e99](https://github.com/teamprosperlink/Ninzo_backend/commit/10a5e99) (PR #191) | PASS | [reports/190.md](reports/190.md) |
| 2026-11-13 | #187 | the golden routes in CI | [334e431](https://github.com/teamprosperlink/Ninzo_backend/commit/334e431) (PR #188) | PASS | [reports/187.md](reports/187.md) |
| 2026-11-13 | #149 | console hub operations: queue, stuck requests and expiring one, the server side (v0.10.0 D) | [6f8481a](https://github.com/teamprosperlink/Ninzo_backend/commit/6f8481a) (PR #194) | PASS | [reports/149.md](reports/149.md) |
| 2026-11-12 | #183 | OSRM on Bengaluru, the scooter profile and golden routes | [2317f67](https://github.com/teamprosperlink/Ninzo_backend/commit/2317f67) (PR #186) | PASS | [reports/183.md](reports/183.md) |
| 2026-11-12 | #180 | the OSRM routing client with a straight-line fallback | [32f55b5](https://github.com/teamprosperlink/Ninzo_backend/commit/32f55b5) (PR #182) | PASS | [reports/180.md](reports/180.md) |
| 2026-11-12 | #175 | more test riders and scooters on staging (`testdata testers`) | [ca643d5](https://github.com/teamprosperlink/Ninzo_backend/commit/ca643d5) (PR #176) | PASS | [reports/175.md](reports/175.md) |
| 2026-11-12 | #169 | geo work's own local Postgres (5433) and Redis (6381) | [0268ad0](https://github.com/teamprosperlink/Ninzo_backend/commit/0268ad0) (PR #173) | PASS | [reports/169.md](reports/169.md) |
| 2026-11-12 | #148 | console incidents: one queue for what needs a person, the server side (v0.10.0 C) | [7e64be6](https://github.com/teamprosperlink/Ninzo_backend/commit/7e64be6) (PR #189) | PASS | [reports/148.md](reports/148.md) |
| 2026-11-12 | #147 | console support actions: tickets, refunds, replacements and credits, the server side (v0.10.0 B) | [389a7ac](https://github.com/teamprosperlink/Ninzo_backend/commit/389a7ac) (PR #181) | PASS | [reports/147.md](reports/147.md) |
| 2026-11-12 | #146 | console order search and timeline, the server side (v0.10.0 A) | [1a755c7](https://github.com/teamprosperlink/Ninzo_backend/commit/1a755c7) (PR #172) | PASS | [reports/146.md](reports/146.md) |
| 2026-11-12 | #178 | customer app additions: veg flag, scooter plate, notification settings, gate landmark and type (part A, contract and mock) | [12043d5](https://github.com/teamprosperlink/Ninzo_backend/commit/12043d5) (PR #184) | PASS | [reports/178.md](reports/178.md) |
| 2026-11-12 | #178 | customer app additions, the server (part B): migration 0043, is_veg, plate, notification settings and the pusher, gate landmark and type | [12043d5](https://github.com/teamprosperlink/Ninzo_backend/commit/12043d5) (PR #184) | PASS | [reports/178.md](reports/178.md) |
| 2026-11-11 | #159 | rider operations, the server side (v0.10.0) | [2ef7f12](https://github.com/teamprosperlink/Ninzo_backend/commit/2ef7f12) (PR #171) | PASS | [reports/159.md](reports/159.md) |
| 2026-11-11 | #157 | the rider app's new operations, contract and mock | [e584c14](https://github.com/teamprosperlink/Ninzo_backend/commit/e584c14) (PR #163) | PASS | [reports/157.md](reports/157.md) |
| 2026-11-11 | #162 | geo design and research record (docs only) | [477feba](https://github.com/teamprosperlink/Ninzo_backend/commit/477feba) (PR #166) | PASS | [reports/162.md](reports/162.md) |
| 2026-11-11 | #164 | `v0.10.0` console ops contract and mock | [afecdb6](https://github.com/teamprosperlink/Ninzo_backend/commit/afecdb6) (PR #165) | PASS | [reports/164.md](reports/164.md) |
| 2026-11-11 | #158 | the rider app's fields | [1cd3af4](https://github.com/teamprosperlink/Ninzo_backend/commit/1cd3af4) (PR #161) | PASS | [reports/158.md](reports/158.md) |
| 2026-11-11 | #156 | rider verification and route endpoints served | [bebc7de](https://github.com/teamprosperlink/Ninzo_backend/commit/bebc7de) (PR #160) | PASS | [reports/156.md](reports/156.md) |
| 2026-11-11 | #151 | masked calls answer call_not_available | [1bf2a82](https://github.com/teamprosperlink/Ninzo_backend/commit/1bf2a82) (PR #155) | PASS | [reports/151.md](reports/151.md) |
| 2026-11-11 | #152 | console sign-in with email; authenticator optional until pre-launch | [8ac1227](https://github.com/teamprosperlink/Ninzo_backend/commit/8ac1227) (PR #153) | PASS | [reports/152.md](reports/152.md) |
| 2026-11-11 | #138 | `v0.9.0` audit log, arrival reviews and support alerts | [8600025](https://github.com/teamprosperlink/Ninzo_backend/commit/8600025) (PR #145) | PASS | [reports/138.md](reports/138.md) |
| 2026-11-11 | #137 | `v0.9.0` menu and prices, gates, hubs and hub staff | [0866e45](https://github.com/teamprosperlink/Ninzo_backend/commit/0866e45) (PR #144) | PASS | [reports/137.md](reports/137.md) |
| 2026-11-11 | #136 | `v0.9.0` zones, settings and the kill switch | [e375abc](https://github.com/teamprosperlink/Ninzo_backend/commit/e375abc) (PR #143) | PASS | [reports/136.md](reports/136.md) |
| 2026-11-11 | #135 | `v0.9.0` rider review in the console | [1be12a2](https://github.com/teamprosperlink/Ninzo_backend/commit/1be12a2) (PR #142) | PASS | [reports/135.md](reports/135.md) |
| 2026-11-11 | #134 | `v0.9.0` console sign-in, users, approvals and flags | [3cd61bd](https://github.com/teamprosperlink/Ninzo_backend/commit/3cd61bd) (PR #141) | PASS | [reports/134.md](reports/134.md) |
| 2026-11-11 | #133 | `v0.9.0` console contract and mock | [f025224](https://github.com/teamprosperlink/Ninzo_backend/commit/f025224) (PR #140) | PASS | [reports/133.md](reports/133.md) |
| 2026-11-11 | #127 | `v0.8.0` the real hub flow, local only | [876ada5](https://github.com/teamprosperlink/Ninzo_backend/commit/876ada5) (PR #139) | PASS | [reports/127.md](reports/127.md) |
| 2026-11-11 | #126 | `v0.8.0` hub offline sync, local only | [037047f](https://github.com/teamprosperlink/Ninzo_backend/commit/037047f) (PR #132) | PASS | [reports/126.md](reports/126.md) |
| 2026-11-11 | #125 | `v0.8.0` shortfall, sealing and quarantine, local only | [6f2edad](https://github.com/teamprosperlink/Ninzo_backend/commit/6f2edad) (PR #131) | PASS | [reports/125.md](reports/125.md) |
| 2026-11-10 | #124 | `v0.8.0` the hub queue, local only | [30a9a8d](https://github.com/teamprosperlink/Ninzo_backend/commit/30a9a8d) (PR #130) | PASS | [reports/124.md](reports/124.md) |
| 2026-11-10 | #123 | `v0.8.0` hub staff sign-in, local only | [d0b1b2a](https://github.com/teamprosperlink/Ninzo_backend/commit/d0b1b2a) (PR #129) | PASS | [reports/123.md](reports/123.md) |
| 2026-11-10 | #122 | `v0.8.0` partner contract and mock | [951366f](https://github.com/teamprosperlink/Ninzo_backend/commit/951366f) (PR #128) | PASS | [reports/122.md](reports/122.md) |
| 2026-11-10 | #120 | `v0.7.0` on staging, test data only | 0ee6692 | PASS: a restock suggestion confirmed with a changed quantity; a second request merged into it; 500 units refused as hub_out_of_stock; the simulator moved it to hub_accepted (with a slot), packing and ready; scanning the cassette's lot made it received and loaded 10 units; a handed-over order earned 4000 paise, shown by getEarnings, and a bad date was 400; listNodes carries the relocation field and an unknown move is 404; no errors in the logs. Not run on staging: shortfall answers and relocation moves, which need a second hub and several riders (tested locally, #107 and #108) | — |
| 2026-11-10 | #109 | `v0.7.0` earnings, local only | [0ee6692](https://github.com/teamprosperlink/Ninzo_backend/commit/0ee6692) (PR #119) | PASS | [reports/109.md](reports/109.md) |
| 2026-11-10 | #108 | `v0.7.0` relocation, local only | [13410d1](https://github.com/teamprosperlink/Ninzo_backend/commit/13410d1) (PR #117) | PASS | [reports/108.md](reports/108.md) |
| 2026-11-10 | #107 | `v0.7.0` manual restock and the pickup, local only | [0fb9070](https://github.com/teamprosperlink/Ninzo_backend/commit/0fb9070) (PR #116) | PASS | [reports/107.md](reports/107.md) |
| 2026-11-10 | #106 | `v0.7.0` restock suggestions, local only | [001d945](https://github.com/teamprosperlink/Ninzo_backend/commit/001d945) (PR #115) | PASS | [reports/106.md](reports/106.md) |
| 2026-11-10 | #105 | `v0.7.0` hubs, restock data and the simulator, local only | [3d4a6c5](https://github.com/teamprosperlink/Ninzo_backend/commit/3d4a6c5) (PR #114) | PASS | [reports/105.md](reports/105.md) |
| 2026-11-10 | #112 | `v0.6.0` on staging, test data only | 7b652d8 | PASS: missed timer owed a credit; problem report recorded; queued pings and a queued handover of one of two accepted, repeat answered the same; missing item refunded (missing_food, 14900), payment still paid; ticket created and read back; notify me saved; 3 pushes logged by the stand-in | This row |
| 2026-11-10 | #101 | `v0.6.0` support and notifications, local only | [7d0f0e8](https://github.com/teamprosperlink/Ninzo_backend/commit/7d0f0e8) (PR #102) | PASS | [reports/101.md](reports/101.md) |
| 2026-11-09 | #98 | `v0.6.0` the offline event batch, local only | [c4a4c64](https://github.com/teamprosperlink/Ninzo_backend/commit/c4a4c64) (PR #99) | PASS | [reports/98.md](reports/98.md) |
| 2026-11-09 | #96 | `v0.6.0` rider problems and reassignment, local only | [957dc20](https://github.com/teamprosperlink/Ninzo_backend/commit/957dc20) (PR #97) | PASS | [reports/96.md](reports/96.md) |
| 2026-11-09 | #94 | `v0.6.0` remedies and the longer ETA, local only | [a1fe966](https://github.com/teamprosperlink/Ninzo_backend/commit/a1fe966) (PR #95) | PASS | [reports/94.md](reports/94.md) |
| 2026-11-09 | #92 | `v0.5.0` on staging, test data only | 589c648 | PASS: order, test payment, offer, accept (1.5 mins), Ready, pings 1 km, 280 m, 270 m (timer started, 90 s), 55 m, 50 m (arrived, not missed, code shown), wrong code 409, handover 200; customer stream through every stage and closed at handover; rider stream offer, accept, handover | This row |
| 2026-11-09 | #90 | Streams outlive the server's limits, local | [589c648](https://github.com/teamprosperlink/Ninzo_backend/commit/589c648) (PR #91) | PASS | [reports/90.md](reports/90.md) |
| 2026-11-09 | #88 | `v0.5.0` live streams, local only | [64ceea8](https://github.com/teamprosperlink/Ninzo_backend/commit/64ceea8) (PR #89) | PASS | [reports/88.md](reports/88.md) |
| 2026-11-09 | #86 | `v0.5.0` the zone timer and handover, local only | [2f021ed](https://github.com/teamprosperlink/Ninzo_backend/commit/2f021ed) (PR #87) | PASS | [reports/86.md](reports/86.md) |
| 2026-11-09 | #84 | `v0.5.0` offers, local only | [50b43f4](https://github.com/teamprosperlink/Ninzo_backend/commit/50b43f4) (PR #85) | PASS | [reports/84.md](reports/84.md) |
| 2026-11-09 | #82 | `v0.4.0` on staging, test data only | f352483 | PASS: create 201, test payment paid, cancel inside 5 s refunded (customer_cancelled, processed), failed then retry then cancel before paying (no refund), too many gives sold_out, list, stock back to 10 | This row |
| 2026-11-09 | #80 | `v0.4.0` orders part B, local only | [f352483](https://github.com/teamprosperlink/Ninzo_backend/commit/f352483) (PR #81) | PASS | [reports/80.md](reports/80.md) |
| 2026-11-09 | #78 | `v0.4.0` orders part A, local only | [5e661e2](https://github.com/teamprosperlink/Ninzo_backend/commit/5e661e2) (PR #79) | PASS | [reports/78.md](reports/78.md) |
| 2026-11-09 | #74 | `v0.3.0` on staging, the real URLs | [af7e871](https://github.com/teamprosperlink/Ninzo_backend/commit/af7e871) (PR #76) | PASS | [reports/74.md](reports/74.md) |
| 2026-11-07 | #72 | `v0.3.0` availability, local only | [d9e4907](https://github.com/teamprosperlink/Ninzo_backend/commit/d9e4907) (PR #73) | PASS | [reports/72.md](reports/72.md) |
| 2026-11-07 | #70 | `v0.3.0` stock and the rider shift, local only | [e66b42c](https://github.com/teamprosperlink/Ninzo_backend/commit/e66b42c) (PR #71) | PASS | [reports/70.md](reports/70.md) |
| 2026-11-07 | #68 | `v0.3.0` menu, local only | [b0f045f](https://github.com/teamprosperlink/Ninzo_backend/commit/b0f045f) (PR #69) | PASS | [reports/68.md](reports/68.md) |
| 2026-11-07 | #66 | `v0.3.0` zones, gates and the location check, local only | [2f92580](https://github.com/teamprosperlink/Ninzo_backend/commit/2f92580) (PR #67) | PASS | [reports/66.md](reports/66.md) |
| 2026-11-07 | #64 | `v0.3.0` settings store, local only | [af3e627](https://github.com/teamprosperlink/Ninzo_backend/commit/af3e627) (PR #65) | PASS | [reports/64.md](reports/64.md) |
| 2026-11-07 | #60 | `v0.2.0` staging deploy, on the real URLs | [9efbfcc](https://github.com/teamprosperlink/Ninzo_backend/commit/9efbfcc) (PR #62) | PASS | [reports/60.md](reports/60.md) |
| 2026-11-06 | #58 | `v0.2.0` profile and devices, local only | [9213211](https://github.com/teamprosperlink/Ninzo_backend/commit/9213211) (PR #59) | PASS | [reports/58.md](reports/58.md) |
| 2026-11-06 | #39 | `v0.2.0` rate limits, local only | [52833d8](https://github.com/teamprosperlink/Ninzo_backend/commit/52833d8) (PR #57) | PASS | [reports/39.md](reports/39.md) |
| 2026-11-06 | #54 | `v0.2.0` one sign-in method per customer, local only | [a2be8b2](https://github.com/teamprosperlink/Ninzo_backend/commit/a2be8b2) (PR #55) | PASS | [reports/54.md](reports/54.md) |
| 2026-11-06 | #47 | `v0.2.0` Google sign-in, phone verification and merging, local only, with fake signed tokens | [55168be](https://github.com/teamprosperlink/Ninzo_backend/commit/55168be) (PR #49) | PASS | [reports/47.md](reports/47.md) |
| 2026-11-06 | #43 | `v0.2.0` phone OTP sign-in, test codes, local only | [8e3dd1a](https://github.com/teamprosperlink/Ninzo_backend/commit/8e3dd1a) (PR #44) | PASS | [reports/43.md](reports/43.md) |
| 2026-11-06 | #38 | `v0.2.0` session system, local only | [fbe2798](https://github.com/teamprosperlink/Ninzo_backend/commit/fbe2798) (PR #40) | PASS | [reports/38.md](reports/38.md) |
| 2026-11-05 | #36 | `v0.2.0` idempotency, audit and outbox, local only | [b7575b4](https://github.com/teamprosperlink/Ninzo_backend/commit/b7575b4) (PR #37) | PASS | [reports/36.md](reports/36.md) |
| 2026-11-05 | #32 | `v0.2.0` service skeleton, local only | [dd735b7](https://github.com/teamprosperlink/Ninzo_backend/commit/dd735b7) (PR #33) | PASS | [reports/32.md](reports/32.md) |
| 2026-11-05 | #30 | `v0.2.0` migrations 0007 and 0008 applied to Supabase | [8fddaf6](https://github.com/teamprosperlink/Ninzo_backend/commit/8fddaf6) (PR #31) | PASS | [reports/30.md](reports/30.md) |
| 2026-11-05 | #28 | `v0.2.0` sign-in contract, mock and tables | [909901e](https://github.com/teamprosperlink/Ninzo_backend/commit/909901e) (PR #29) | PASS | [reports/28.md](reports/28.md) |
| 2026-11-05 | #22 | `v0.2.0` migrations applied to Supabase | [4059cd3](https://github.com/teamprosperlink/Ninzo_backend/commit/4059cd3) (PR #24) | PASS | [reports/22.md](reports/22.md) |
| 2026-11-05 | #21 | `v0.2.0` database foundation, local Postgres only | [ec6cf89](https://github.com/teamprosperlink/Ninzo_backend/commit/ec6cf89) (PR #23) | PASS | [reports/21.md](reports/21.md) |
| 2026-11-05 | #8 | `v0.1.0` mock server | [6ad2019](https://github.com/teamprosperlink/Ninzo_backend/commit/6ad2019) (PR #18) | PASS | [reports/8.md](reports/8.md) |
| 2026-11-04 | #15 | `v0.1.0` cancel and refund rule | [ee7c91b](https://github.com/teamprosperlink/Ninzo_backend/commit/ee7c91b) (PR #17) | PASS | [reports/15.md](reports/15.md) |
| 2026-11-04 | #7 | `v0.1.0` rider contract | [3e26e5e](https://github.com/teamprosperlink/Ninzo_backend/commit/3e26e5e) (PR #16) | PASS | [reports/7.md](reports/7.md) |
| 2026-11-04 | #6 | `v0.1.0` customer contract | [2e714cd](https://github.com/teamprosperlink/Ninzo_backend/commit/2e714cd) (PR #14) | PASS | [reports/6.md](reports/6.md) |
| 2026-11-04 | #5 | `v0.1.0` shared API rules | [be604bc](https://github.com/teamprosperlink/Ninzo_backend/commit/be604bc) (PR #10) | PASS | [reports/5.md](reports/5.md) |

Result values: PASS · FAIL · PARTIAL (explain in the report).
