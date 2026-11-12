# Research record: how others solve this

Version: 0.1 · Updated: 2026-11-11 · Issue: #162

What the market and the research use for problems like ours, and what Ninzo takes from each. Part of the geo record (`README.md`).

## 1. How strong is each claim?

| Mark | Meaning |
|---|---|
| **Established** | Textbook or peer-reviewed method, decades in use |
| **Published** | The company's own engineering blog, paper, talk or open-source code |
| **Reported** | Secondary sources: case studies, agency guides, summaries. Treat as likely, not certain |
| **Observed** | Visible in the product from outside |
| **Checked** | Verified by us on 2026-11-11; source in section 10 |
| **Ours** | Our own reasoning, to be proved by the simulator or real data |

Company internals are rarely public. Nothing marked Reported should be the only reason for a decision.

## 2. The closest match: ambulance placement and dispatch (Established)

Ninzo's problem has the same shape as emergency medical services (EMS): few vehicles, waiting at chosen stations, each must reach any caller within a time limit, and a vehicle on a job cannot serve anyone else.

| Method | What it answers | Use for Ninzo |
|---|---|---|
| **Set covering** (Toregas and others, 1971) | The fewest stations so every demand point is within time T | How many staging nodes a zone needs at minimum |
| **Maximal covering** (Church and ReVelle, 1974) | With p vehicles, the stations that cover the most demand within T | Which nodes to use for a given fleet size |
| **Maximum expected covering** (Daskin, 1983) | The same, allowing that each vehicle is busy a share q of the time: demand covered k times is worth more than once | Our small fleet is often partly busy, so backup coverage matters. This is the model to start from |
| **Placement tables** ("compliance tables" in EMS dispatch) | For each number of free vehicles, a precomputed best set of stations | Precompute the best nodes for 1 to 8 free scooters; when one goes busy, move the others toward the next row |
| **Preparedness in dispatch** (for example Andersson and Värbrand, 2007) | Do not always send the nearest unit; weigh how much coverage is lost by sending it | A cost term in matching: the coverage lost if this scooter leaves |
| **Relocation as assignment** | Which free vehicle moves to which empty station, with the least total travel | The Hungarian method fits here: small, square, and repeated |

**Ours:** this is the core of `w4` and part of `w5`. It needs only demand per cell, road times between cells and nodes, and the fleet size, all of which the other workstreams produce.

## 3. Ride-hailing

| Practice | Company | Evidence | Use for Ninzo |
|---|---|---|---|
| H3 hexagon grid, built to balance supply and demand by area and to price by area; open-sourced in 2018 | Uber | Published (open source) | Our main grid (`w2`) |
| Matching in short batches instead of nearest-first, so two nearby requests do not both take the same best driver | Uber, others | Published talks; Reported details | Only at city scale, when orders compete (`w5`) |
| Order dispatch and idle-driver repositioning by learned value of a driver at a place and time | DiDi | Published (Xu and others, KDD 2018) | Later, if placement tables stop being enough (`w4`) |
| Heat maps that nudge idle drivers toward demand | Uber, Ola, Rapido | Observed | The relocation suggestion we already send (`internal/relocation`) |

## 4. Food delivery and quick commerce

| Practice | Company | Evidence | Use for Ninzo |
|---|---|---|---|
| Just-in-time assignment: hold the rider until the food is nearly ready | Swiggy | Reported | Our trigger is the zone entry, not food readiness (owner's PDF, page 8) |
| Delivery time = max(assignment delay + first mile, prep time) + last mile | Swiggy | Reported | Our time is (offer + packing + ride to the zone) + the timed leg |
| ETA from gradient-boosted trees, later deep learning | Swiggy | Reported | Averages first, trees once data exists (`w6`) |
| In-house route estimation because map APIs were slow and costly at volume | Swiggy | Reported | Supports self-hosted OSRM (`w1`) |
| Dispatch decided by optimisation over ML-predicted times, and tested in a simulator before release | DoorDash | Published (engineering blog) | The simulator in `w8` |
| Dark stores with a small service radius; an area turned off ("unserviceable") when riders or stock run out | Blinkit, Zepto, Instamart | Observed | Our availability check before payment, from the same coverage numbers |
| Correcting customer pins from where riders actually stop | Zomato, Swiggy | Reported | Record the rider's stop at handover and suggest gate pin fixes (`w3`, `w9`) |
| Batching two nearby orders only if both still meet their limit | Common | Reported | The answer to the second-order question (G19), if yes |

## 5. Routing engines

| Engine | Strength | Reachable area (isochrone) | Many-to-many table | Traffic | Licence, cost | Fit |
|---|---|---|---|---|---|---|
| **OSRM** | Very fast routes and tables (Contraction Hierarchies or MLD) | No | Yes (`/table`) | MLD mode accepts our own speed updates | BSD; we host it | **Chosen** (G3). Cell sets from `/table` |
| Valhalla | Isochrones, time-dependent costs, map matching | Yes | Yes | Yes, with data | MIT; we host it | Good alternative if isochrones become necessary |
| GraphHopper | Custom profiles, isochrones | Yes | Not in the open-source core (the matrix API is a paid product) | Limited | Apache core | Alternative |
| pgRouting | Dijkstra with cutoff inside Postgres | Yes (driving distance) | Yes, slow | No | GPL; available on Supabase (Checked) | Would put the Bengaluru road graph in Supabase. Not recommended on the free plan |
| Google Routes API | Live traffic | No | Yes (Route Matrix) | Yes | Paid per call; terms limit storing results (section 10) | Not for building our tables |
| Mappls APIs | Indian addresses and roads | Yes (reachability) | Yes | Yes | Paid per call; terms apply | Possible geocoding source (`w9`) |
| Ola Maps APIs | Indian maps at lower cost | Check | Check | Check | Paid; terms apply | Watch only |

**Algorithms behind them (never written by us):** Dijkstra with a cutoff, A*, Contraction Hierarchies and MLD come inside the engine. RPHAST, listed in the owner's PDF for one-to-many times, is not in OSRM; OSRM's `/table` does the same job for our sizes.

## 6. Grids and location codes

| Grid | Shape | Who uses it | Fit |
|---|---|---|---|
| **H3** | Hexagons, 16 resolutions, equal neighbour distances | Uber, many logistics firms | **Proposed main grid** (G7). Resolution 10: edges about 66 m, about 120 m across |
| S2 | Squares on a cube, very fast containment | Google, earlier Uber | Equal choice technically; H3's equal neighbours suit "rings" better |
| Geohash | Rectangles from a string prefix | Redis GEO uses it inside | Used indirectly through Redis |
| **DIGIPIN** | About 4 m × 4 m squares, a 10-character code; India Post, launched 27 May 2025, open (Checked) | India Post | Store beside a gate pin as a shareable code; not a computing grid (`w9`) |

## 7. Live position and geofencing

| Tool | Fit |
|---|---|
| **Redis GEO** (`GEOADD`, `GEOSEARCH`) | Latest position per rider, nearby search, cheap. At city scale; the pilot uses one Postgres row per rider (`w3`, G17) |
| **PostGIS** | Exact distance and history. In use |
| Tile38 | A dedicated geofence server with enter and exit events. Only at multi-city scale |
| Kafka, NATS | An event stream for pings. Not before the outbox and Redis stop being enough (decision 0003) |

## 8. Optimisation, forecasting, simulation, viewing

| Need | Tools | Pilot choice |
|---|---|---|
| Covering and placement | Greedy, OR-Tools, PuLP | Greedy, as pure Go functions the simulator can reuse (`w4`, `w8`); OR-Tools at city scale |
| Assignment | Hungarian (`scipy.optimize.linear_sum_assignment`; Go ports exist), min-cost flow | Hungarian for relocation only |
| Demand forecast | Moving averages, Prophet, LightGBM | Averages per cell, weekday and 30-minute band (`w7`) |
| Travel-time percentiles | Rolling percentiles, quantile regression | Rolling p50 and p90 per gate and hour band (`w6`) |
| Simulation | SimPy, a custom event loop | A Go simulator reusing the real rules; Python only for charts (`w8`) |
| Viewing cells | kepler.gl (made by Uber, reads H3), deck.gl | Export cell sets for the owner and ops to view |

## 9. What we copy, and what we change

| Area | Copy | Change for Ninzo |
|---|---|---|
| Positioning | EMS covering models and placement tables | Stock fit matters too: a scooter without the item covers nothing |
| Dispatch | Filter, then rank; preparedness | The rider accepts an offer; one at a time |
| Grid | H3 | The circle stays as the hard outer limit (owner's hybrid) |
| Road times | Self-hosted engine | A scooter profile with heavy signal and U-turn costs, tuned on Bengaluru trips |
| ETA | Rules first, models later | The promise is the timed leg only; the ETA before it is advice |
| Events | Streams of pings and orders | Postgres outbox and Redis until scale needs more |

## 10. Facts checked on 2026-11-11

| Fact | Source |
|---|---|
| Uber's Go H3 binding needs cgo (`CGO_ENABLED=1`); our images build with `CGO_ENABLED=0` | [uber/h3-go](https://github.com/uber/h3-go); `deploy/Dockerfile.service` |
| Pure-Go H3 ports exist but are early or partial: `ziprecruiter/h3-go` (described in search results as the most complete; not confirmed by us, and its API may change), `ThingsIXFoundation/h3-light` (small subset), `dimchansky/h3-go` (work in progress) | [ziprecruiter/h3-go](https://awesome.ecosyste.ms/projects/github.com%2Fziprecruiter%2Fh3-go), [h3-light](https://pkg.go.dev/github.com/ThingsIXFoundation/h3-light), [dimchansky/h3-go](https://github.com/dimchansky/h3-go) |
| Ninzo's Supabase project offers PostGIS 3.3.7 and pgRouting 3.4.1, and no H3 extension | Supabase extension list for project `chyejfgsipcsdxnginei` |
| OSRM's table service allows 100 locations by default; raise it with `--max-table-size` on our own server. Re-check against the docs of the version we pin (`w1` step 1) | [OSRM HTTP docs (v5.5.1)](https://project-osrm.org/docs/v5.5.1/api/), [osrm-talk](https://lists.openstreetmap.org/pipermail/osrm-talk/2015-May/000836.html) |
| OSRM's car profile (master) sets `u_turn_penalty = 20` s and `turn_penalty = 7.5` s; signal costs come from `highway=traffic_signals` nodes. The exact signal penalty must be read from the version we pin | [car.lua](https://github.com/Project-OSRM/osrm-backend/blob/master/profiles/car.lua), [traffic_signal.lua](https://github.com/Project-OSRM/osrm-backend/blob/master/profiles/lib/traffic_signal.lua) |
| Google Maps Platform replaced the 200 USD monthly credit with free monthly usage per SKU from 1 March 2025 (Essentials 10,000, Pro 5,000, Enterprise 1,000) | [Google, March 2025 changes](https://developers.google.com/maps/billing-and-pricing/march-2025) |
| Google's service terms bar using Directions API content with a non-Google map; Routes API caching is limited (for example `place_id`). Read the current terms before any server-side use | [Maps Service Terms](https://cloud.google.com/maps-platform/terms/maps-service-terms) |
| DIGIPIN: India Post, launched 27 May 2025, about 4 m × 4 m cells, 10 characters, open | [The Week](https://www.theweek.in/news/sci-tech/2025/05/31/what-is-digipin-india-new-technology-postal-geo-address-system-department-of-posts.html) |

### Checked on 2026-11-13 (the H3 spike, #193)
- `github.com/ziprecruiter/h3-go` v0.5.0: its README says "early stages of development". It exports point to cell and grid disk; cell centre and outline are not exported.
- `github.com/akhenakh/goh3`: "experimental", H3 v4.4.1 machine-translated to pure Go by ccgo (Apache-2.0). The translated code ships for darwin, freebsd and linux only, with no Windows build.
- `github.com/uber/h3-go`: needs `CGO_ENABLED=1`.
- Official test vectors: Uber's H3 4.1.2 through its Python binding (`h3==4.1.2`), 7,016 cells over Bengaluru and the world.
