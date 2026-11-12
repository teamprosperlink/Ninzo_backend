# Geo: maps, cells, routing, rider placement and matching

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Status: **Draft, for owner review**

This folder is the decision record and the research record for everything that depends on a map: road times, the cell grid, where scooters wait, who gets an order, routes, ETAs and demand. Read it before working on any of these, and before asking the owner something it already answers. Update it in the same PR as any change it describes.

| File | What it holds |
|---|---|
| `README.md` (this file) | The goal, how the algorithm documents fit the flow in force, principles, the workstreams and their order, scale stages, the decision log, open questions, the glossary |
| [`research.md`](research.md) | What others in the market do, with methods, tools and how strong the evidence is |
| [`w1-road-data-and-routing.md`](w1-road-data-and-routing.md) | OpenStreetMap data, OSRM, the scooter profile, hosting, the road data version |
| [`w2-map-grid-and-distribution.md`](w2-map-grid-and-distribution.md) | H3, the cells around each gate (timer zone, sweet spot, rings), pre-mapping |
| [`w3-live-rider-data.md`](w3-live-rider-data.md) | GPS pings, cleaning them, cells on pings, latest positions, history |
| [`w4-rider-placement.md`](w4-rider-placement.md) | Where scooters wait: staging nodes, coverage, placement tables, relocation |
| [`w5-matching.md`](w5-matching.md) | Who gets the offer: filter, rank, cost, second orders |
| [`w6-routes-and-eta.md`](w6-routes-and-eta.md) | The rider's route, the customer's ETA, learning real travel times |
| [`w7-demand.md`](w7-demand.md) | Where and when orders come: counting and forecasting per cell and hour |
| [`w8-simulation-and-proof.md`](w8-simulation-and-proof.md) | The simulator, the road test and the proof of concept |
| [`w9-map-providers.md`](w9-map-providers.md) | Google Maps, Mappls, Ola Maps and DIGIPIN: what each is for, and their terms |

Inputs: `docs/context/algorithms/` (the owner's algorithm documents, including `2026-11-11-owner-notes-hybrid-and-premapping.md`). Flow in force: decision 0007, decision 0011, `docs/engineering/data_flow.md`.

## 1. The goal

**Product goal.** A packed rider who enters the zone around the customer's location reaches the 100 m perimeter before the visible timer ends: **90 seconds** (owner, 2026-11-11, #162), safely. The customer's wait before the timer starts stays short, and an order is refused before payment, not after, when no scooter can serve it.

**What this folder must deliver for the pilot** (Bengaluru, one micro-market, 4 to 8 scooters, 1 to 3 items):

| Measure | Target | Today |
|---|---|---|
| Orders where zone entry to the perimeter takes no more than the timer | Owner to set. Proposed: 95% | Not measured. The zone is a 350 m circle that ignores roads |
| Wait from payment to the timer starting | Under `max_wait_before_timer_s` (5 min), and as short as possible | Not measured. Scooters are chosen by straight-line distance |
| Orders refused before payment for "not available right now" | As low as possible at the pilot fleet size | Counted by straight-line reach |
| Road-time figures behind every decision | Every travel time comes from roads, or says it does not | Straight lines at a set speed everywhere |
| Safety | No rule works only if riders speed. Speed on the timed leg is measured from pings, and a rule that pushes riders over the profile's speed cap is rejected | Not measured |

**What it must allow later:** more zones in Bengaluru, then more cities, without changing the order flow or the contracts. See section 6.

## 2. The idea, and what the problem really is

Swiggy waits for the kitchen; Blinkit and Zepto wait for the picker. Ninzo removes both waits, because **the stock is already on the scooter**: each scooter is a small dark store on wheels. After payment there is only the ride left, and the timer covers only its last part, from the zone to the 100 m perimeter.

So the outcome is mostly decided **before the order exists**: is a scooter with the right stock already parked close enough, by road? Choosing a rider for an order is the small problem. **Keeping scooters in the right places is the big one.**

That makes this the **ambulance problem**: a few vehicles waiting at chosen stations, each must reach any caller within a set time, and each one is unavailable while busy. Emergency services have solved it for decades, and their methods fit almost directly (see `research.md`, section 2).

## 3. How the algorithm documents fit the flow in force

The owner's algorithm documents assume automatic assignment, with the timer starting at assignment. **Decision 0011 keeps the 2026-11-04 flow**, so we take the documents' **algorithms** and fit them into that flow. Nobody builds the documents' flow.

| The documents say | What we do instead | Why |
|---|---|---|
| Automatic assignment, no Accept button | The rider gets an offer and accepts (one rider at a time) | Decision 0011 #1 |
| Timer starts at assignment | Timer starts from GPS, once packed and inside the timer zone | Decisions 0007, 0011 |
| The sweet spot protects the 90 s | Split into two areas: the **timer zone** protects the 90 s, the **sweet spot** keeps the wait before it short (section 4) | Our timer starts later than theirs |
| "Expanding the search" shown to the customer | The customer sees only "looking for riders". Rings are an internal order of who to try. Past the widest search, refused before payment | Decision 0011 #8 |
| Customer map lagged on purpose | No. The map stays live | Decision 0011 #2 |
| Reassign after 15 to 20 s without a ping | Code released after 30 s; reassigned after 2 minutes; both settings | Decision 0011 #3 |
| Free cancel 10 to 15 s after assignment | 5 s after payment, full refund | Decision 0007 |
| H3 resolution 11 for the 100 m zone | Exact distance for the perimeter | Proposed, G8 |
| Valhalla isochrones for sweet spots | OSRM table calls from cell centres | OSRM chosen; G10 |
| Hungarian or OR-Tools for assignment | Ranked choice for dispatch; the Hungarian method only for relocation | Proposed, G12 and G15 |
| Battery penalty in the cost | Possible: the rider contract has scooter `battery_pct` (#158), null until scooter data exists | Open, G21 |
| Kafka or NATS | Postgres outbox and Redis | Decision 0003 |

## 4. The two areas around each gate

The documents' "sweet spot" does two jobs at once. In our flow they separate:

| | **Timer zone** | **Sweet spot**, then **rings** |
|---|---|---|
| Protects | The visible timer: entering the zone to the 100 m perimeter | The wait before the timer: from the offer to entering the zone |
| Budget | `timer_length_s` (90 s), with a safety margin | `sweet_spot_budget_s` (80 s today), **measured to the gate**, as the setting and the owner's documents already define it; larger budgets for each ring, up to `max_search_radius_m` |
| Today | A circle of `zone_radius_m` (350 m) around the gate | Straight-line reach from the scooter's staging node at `straight_line_speed_kmh`, capped at `max_search_radius_m` |
| Hybrid form | H3 cells inside the circle **whose road time to the perimeter fits the timer budget** | H3 cells whose road time to the gate fits each budget |
| Used by | The timer start (GPS) | Availability, scooter choice, the order in which riders are offered |

The circle stays as the hard outer limit that ops can tune. Road time removes what is near in a straight line but slow by road: a median, a one-way street, a long signal. In Bengaluru this matters most at signals and divided roads, not at speed: 250 m in 90 s needs only about 10 km/h on average. Details: `w2-map-grid-and-distribution.md`.

## 5. Principles

1. **Heavy work offline, light work live.** Road times and cell sets are computed in the background; the live path reads them.
2. **Every number is a setting**, changed through the console with approval (decision 0011). No budget, radius or weight is a constant in code.
3. **Always a fallback.** If OSRM or the cell sets are missing, the straight-line stand-in answers, and the reply says so (`eta_source`).
4. **Road data has a version.** Every stored road time records which map data and profile made it, so a result can be traced and rebuilt.
5. **Learn from our own trips.** Start from OSRM's estimate; replace it with what our GPS shows once there are enough trips.
6. **Simple first, upgraded on evidence.** Ranked choice before optimisers, averages before machine learning. An upgrade ships only when the simulator or real data shows a gain.
7. **Test before the street.** Every rule is tried in the simulator before real orders (`w8`).
8. **Licences.** Route only on OpenStreetMap data with OSRM. Use Google Maps and Mappls only through their APIs and SDKs within their terms; never download or store their map data for routing. Credit OpenStreetMap where its data is shown.
9. **The flow and contracts do not change** for a geo improvement. If one must, it goes through the owner and the shared Doc first.

## 6. Scale stages

| Stage | Size | What changes |
|---|---|---|
| **Pilot** | 1 zone, 4 to 8 scooters, 1 to 3 items | Everything in this folder at its simplest: one OSRM server, cell sets per gate, ranked choice, placement tables for 1 to 8 free scooters, averages for demand |
| **City** | Several Bengaluru zones, tens of scooters | Cell sets per zone instead of per gate, if gates are many. Batched dispatch every 1 to 2 s with an assignment solver when orders compete. OSRM's traffic-ready mode (MLD) with our own measured speeds. Demand forecast by gradient-boosted trees. Positions in Redis only, streams pushed instead of polled |
| **Multi-city** | Hundreds of scooters, several cities | One OSRM data set per city or region. A separate dispatch worker. A geofence engine (Tile38 or similar) if Redis is not enough. Placement by an exact solver (ILP, OR-Tools). ETA models per city |

Each workstream file has its own "Path to scale" section.

## 7. Workstreams and build order

Each workstream is solved one step at a time: one Issue, its own branch, tests (including a deliberate break to prove the tests catch it), an independent review, and a PR that merges only when every check passes (verify, contracts, mock-contract, database and, from #187, routes).

| # | Workstream | Depends on | First deliverable |
|---|---|---|---|
| W0 | Own local database for this work: Postgres on 5433, Redis on 6381 | None | Built (#169): `deploy/compose.geo.yml`, `task geo:db:up`, `task geo:db:migrate`, `task geo:test:db` |
| W1 | [Road data and routing](w1-road-data-and-routing.md) | W0 | OSRM with a scooter profile on Bengaluru, a Go client with a straight-line fallback. Client built (#180): `internal/routing` |
| W2 | [Map grid and distribution](w2-map-grid-and-distribution.md) | W1 | Cell sets per gate (timer zone, sweet spot, rings), stored and versioned |
| W8 | [Simulation and proof](w8-simulation-and-proof.md) | W1, W2 | A simulator that reports the timer success rate on the test zones |
| W3 | [Live rider data](w3-live-rider-data.md) | W2 | Each ping stamped with its cell; the latest position per rider |
| W5 | [Matching](w5-matching.md) | W2, W3 | Offers ranked by road time, by tier |
| W6 | [Routes and ETA](w6-routes-and-eta.md) | W1, W3 | `getRoute` and the customer ETA from OSRM, `eta_source: measured` |
| W4 | [Rider placement](w4-rider-placement.md) | W2, W7, W8 | Staging nodes scored by coverage; placement tables; relocation toward them |
| W7 | [Demand](w7-demand.md) | W3 | Orders counted per cell and hour |
| W9 | [Map providers](w9-map-providers.md) | None | What the backend calls, if anything, decided with the owner |

The simulator (W8) comes early on purpose: it is the test bench for every later rule.

## 8. Decision log

Status: **Decided** (with its source), **Proposed** (engineering's recommendation, awaiting the owner), **Open** (needs an answer). When the owner accepts a Proposed row, it moves to Decided with the date, and a decision file goes in `docs/decisions/` if it changes a rule.

| ID | Decision | Status | Source |
|---|---|---|---|
| G1 | The 2026-11-04 flow stays; the algorithm documents give algorithms only | Decided | 0011 #1 |
| G2 | The timer stays at 90 s for now (a setting, `timer_length_s`) | Decided | Owner, 2026-11-11, #162 |
| G3 | Road times from self-hosted OSRM on OpenStreetMap data. This settles the "Valhalla or OSRM, to be chosen" left open in 0011 #12 | Decided | Owner, 2026-11-10, first written down here (#162); `tech_stack.md` updated |
| G4 | Pilot city: Bengaluru | Decided | 0011 #5 |
| G5 | Google Maps and Mappls only through their APIs and SDKs within their terms; nothing of theirs stored for routing | Decided | Owner's brief for this work, 2026-11-11 (#162) |
| G6 | Every budget, radius and weight is a setting | Decided | 0011 |
| G7 | A hexagon grid the size of H3 resolution 10 (cell area about 0.015 km²) is the main grid: for the pilot, `internal/hexgrid` grid `blr76` (76 m edges, G18) | Decided (#193) | `w2` |
| G8 | The 100 m perimeter stays an exact distance check, not H3 resolution 11 | Proposed | `w2` |
| G9 | Two areas per gate: the timer zone (circle cut by road time) and the sweet spot with rings | Proposed | Section 4 |
| G10 | Cell sets are built with OSRM table calls from cell centres, not isochrone polygons | Proposed | `w1`, `w2` |
| G11 | Cell sets are built when a gate is created and refreshed nightly, tied to a road data version | Proposed | `w2` |
| G12 | Matching filters by tier, then ranks by road time plus coverage lost plus stock fit; no optimiser at pilot size | Proposed | `w5` |
| G13 | Rings stay internal; the customer sees only "looking for riders"; refused before payment past the widest search | Decided in part (0011 #8); rings as tiers proposed | `w5` |
| G14 | The straight-line stand-in stays as the fallback when road data is missing | Proposed | Principle 3 |
| G15 | Placement by covering models and placement tables; the Hungarian method only for moving free scooters | Proposed | `w4` |
| G16 | A simulator tests every rule before real orders | Proposed | `w8` |
| G17 | Full ping history in Postgres (kept in full through testing); latest position as one Postgres row per rider for the pilot, Redis at city scale | History decided (owner, 2026-11-05); the rest proposed | `w3` |
| G18 | How Go gets H3. Spike (#193): no pure-Go H3 covers the four functions and builds on the Windows development PC (production is Linux), so the pilot uses our own pure-Go hexagon grid (`internal/hexgrid`), sized like H3 res 10; H3 at city scale is a rebuild of the cell sets | Decided (#193, engineering) | `w2` |
| G19 | Can a busy rider take a second order? | Open | 0011 #6 |
| G20 | Build the road-time timer zone now, or keep the plain circle and do the sweet spot first | Open | Section 4 |
| G21 | Use scooter battery in the cost? | Open | `w5` |
| G22 | What the backend does with Google Maps and Mappls, if anything. `owner_setup_checklist.md` already lists `GOOGLE_MAPS_SERVER_API_KEY`, and `data_flow.md` says locations and polygons are mapped through Google Maps | Open | `w9` |
| G23 | Where OSRM runs, and its cost | Open | `w1` |
| G24 | The first sweet spot budget: seed from OSRM until the Bengaluru road test | Open | 0011 #4, `w8` |
| G25 | A running order keeps the zone cells and road data version it started with, as it already keeps its timer length and radius, so a rebuild never moves a running timer | Proposed | Decision 0007, `w2` |

## 9. Open questions for the owner

1. **G20, the timer zone:** decision 0007 says you will write the location-based radius algorithm yourself. The road-time timer zone is offered as a proposal for it, not a replacement. Build it now behind a setting, or keep the 350 m circle and do the sweet spot first? Recommendation: build it now behind a setting, because it is what protects the 90 s.
2. **G19, second order:** if a rider is already delivering and a new order comes in nearby, may the system ever give it to the same rider? Until answered: one order per rider.
3. **G21, battery:** use the scooter's battery in the ranking once scooter data exists? Recommendation: yes, only as a filter (too low to finish the trip and return), not a weight.
4. **G22, Google Maps and Mappls:** does the backend need them at all (for example, turning an address into a gate pin), or do only the apps use them?
5. **G23, OSRM hosting:** a Railway service with about 1 to 2 GB of memory for staging? (Measured in `w1` step 2 before asking for a final figure.)
6. **G24, sweet spot budget:** seed it from OSRM times until the road test gives a real value?
7. **Target:** what share of orders must meet the timer for the pilot to count as working? Proposed: 95%.
8. **Lost demand:** may an availability check before sign-in add one anonymous count to its cell, with no location, device or person stored? Decision 0011 #9 says nothing is saved, so no unless you say otherwise (`w7`).
9. **Ownership:** `getRoute` and the order ETA are in `internal/orders/route.go` and `orders.go`. May this session change those two files for `w6`?

## 10. What exists today

| Part | Today | Code |
|---|---|---|
| Availability and scooter choice | Straight line from the scooter's staging node, reach = the smaller of `max_search_radius_m` and `straight_line_speed_kmh` × `sweet_spot_budget_s`; nearest scooter holding the whole basket | `internal/availability/availability.go`, `pick.go` |
| Offers | The order's own scooter first, then the nearest free one holding the basket; one rider at a time; `offer_timeout_s` | `internal/orders/offers.go` |
| Timer start and stop | GPS pings, straight line to the gate, two pings in a row. Pings with an accuracy worse than 100 m are refused; jumps over 40 m/s are dropped; pings over 30 s late do not move the timer but are kept for late-arrival review. Each order keeps the timer length and radius it started with (decision 0007) | `internal/orders/tracking.go` |
| Rider's route | A straight line from the last position to the gate, `eta_source: placeholder` | `internal/orders/route.go` |
| Relocation | Recent paid orders counted at the node nearest each gate; straight-line distance | `internal/relocation/relocation.go` |
| Live streams | Each stream reads the saved `seq` about once a second; the rider's position every 3 s | `internal/orders` |
| Pings | Stored in full in Postgres (`location_pings`) | migration 0020 |
| Grid | None. No H3 anywhere | |
| Redis | In the plan (decision 0003), not used by any code yet | |
| Build | Images built with `CGO_ENABLED=0` on a static distroless base | `deploy/Dockerfile.service` |
| Spatial database | PostGIS 3.3.7 on Supabase; `pgrouting` 3.4.1 available there but not installed; **no H3 extension** on Ninzo's Supabase project (checked 2026-11-11) | |

## 11. Glossary

| Term | Meaning |
|---|---|
| Gate | The exact point a customer picks for delivery: a map pin plus entry notes |
| Service area | A zone in the `zones` table: the polygon where Ninzo serves, with its settings |
| Timer zone | The area around a gate where the visible timer starts. Today a 350 m circle; proposed: the circle cut down by road time |
| Arrival perimeter | 100 m around the gate (`arrival_perimeter_m`). Entering it stops the timer |
| Sweet spot | The area from which a scooter reaches the gate within `sweet_spot_budget_s` (80 s today) by road |
| Ring, tier | A wider band after the sweet spot, with a larger time budget, tried in order |
| Staging node | A legal place where a scooter waits for orders |
| Hub | A site where scooters restock |
| Cell | One H3 hexagon. At resolution 10, edges about 66 m and about 120 m across, about 0.015 km² |
| Road data version | The OpenStreetMap extract date plus the OSRM profile version that produced a road time |
| Coverage | The share of expected orders that at least one free scooter can serve within the budget |
| Placement table | For each number of free scooters, the best set of staging nodes to hold them |
| p50, p90 | The time that half, or 9 in 10, trips beat |
