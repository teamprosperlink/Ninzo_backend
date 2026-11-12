# W1: Road data and routing

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Every travel time in the backend comes from real roads in Bengaluru, for a scooter, from a server we run, with a known data version, and a safe fallback when it is down.

## Today
No routing engine. Every distance is a straight line at `straight_line_speed_kmh`. OSRM is chosen (G3).

## Options

| Choice | Options | Recommendation | Why |
|---|---|---|---|
| OSRM mode | **CH** (Contraction Hierarchies): fastest queries, especially large `/table` matrices; a full contraction for any speed change. **MLD** (multi-level Dijkstra): slower queries, and much slower for large tables, but speeds can be updated quickly with `osrm-customize` | **MLD for live routes**, measured against CH for the cell-set builds in step 2; keep CH for the builds if MLD is too slow there | Both use the same `osrm-extract` output, so running or switching between them is cheap. MLD matters once we feed our own measured speeds (`w6`) |
| Map data | Geofabrik's India extract or its southern zone, clipped to Bengaluru with `osmium` | Southern zone, clipped to the Bengaluru box plus a margin | Smaller, faster to build, enough for the city |
| Profile | `car.lua`, `bicycle.lua`, or our own **scooter profile** | Our own, starting from `car.lua` | A scooter uses car roads, but with its own speed caps and much higher costs for signals and U-turns |
| Where it runs | Local Docker (dev), a small test extract (CI), Railway (staging), AWS Mumbai (production, decision pending) | All four, the same image and profile | One build, one behaviour |
| How Go calls it | Plain HTTP to `/route`, `/table`, `/nearest`, later `/match` | A small client in a new `internal/routing` package | No library needed; easy to fake in tests |

## The scooter profile
Starting values are guesses ("Ours") until the road test (`w8`) measures them. Every value is versioned with the profile.

| Rule | Starting idea | Why |
|---|---|---|
| Roads allowed | What cars use, plus `motorcycle=yes` and `moped=yes`; nothing tagged no for motorcycles | Some flyovers and elevated roads restrict two-wheelers: follow the tags, check the pilot area by hand |
| Speed | The car speeds, capped at a city scooter speed; lower on residential and service roads | Scooters filter through traffic but slow in lanes |
| Traffic signals | A large wait per signal, from the road test, not the small car default | A red light in Bengaluru can be 60 to 120 s; one signal can break the timer |
| U-turns | Expensive | Medians force long detours; a U-turn across traffic is slow and unsafe |
| Crossing a main road | Extra cost for a turn across oncoming traffic | Safety, and the real wait |
| One-way streets | Obeyed | Safety and the law |

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | Pin OSRM and build Bengaluru | The OSRM Docker image at a fixed version; the Geofabrik extract; `osmium` | A script in `tools/osrm/` downloads, clips, and runs `osrm-extract`, `osrm-partition`, `osrm-customize` with the profile | One command builds the data from nothing; the build records the extract date and profile version. Built (#183): OSRM v6.0.0, MLD; `task geo:osrm:build` |
| 2 | Scooter profile v1 | The rules above as a Lua profile | `tools/osrm/profiles/scooter.lua`, starting from `car.lua` | Golden routes pass (step 3); memory and build time measured and written here. Built (#183): `tools/osrm/profiles/scooter.lua`, version scooter-2; Bengaluru builds in about 30 s, peak 750 MB, and serves in about 220 MB |
| 3 | Golden routes | 15 to 30 Bengaluru pairs in the test zones with known facts: a one-way street, a median, a signal | A test file of pairs with expected rules (for example "A to B must not take the one-way street", "across the median takes over 60 s") | Tests fail if the profile breaks a rule (proved with a deliberate break). Built (#183): 11 one-way pairs, 10 straight signal crossings, 4 closed streets, the speed cap on 19 routes, a 499-source table; run with `task geo:test:routes` |
| 4 | Local OSRM | The W0 compose project | An `osrm` service on its own port, mounting the built data | `task` command starts it and waits until a route answers. Built (#183): `task geo:osrm:up` on 127.0.0.1:5001 |
| 5 | Go client | `internal/routing`: `Route(from, to)` returns seconds, metres and the line; `Table(sources, dest)` returns seconds; `Nearest(point)` returns the snapped point and how far it moved | Timeouts (about 300 ms), a short circuit-breaker, and a **straight-line fallback** that says so (`source: placeholder`) | Unit tests against a fake OSRM server cover success, timeout, bad reply and fallback; a deliberate break shows they catch it. Built (#180) |
| 6 | Road data version | A table `road_data_versions` (extract date, profile version, OSRM version, built at) | Written by the build script; every stored road time points to it | A stored time can always be traced to the data that made it |
| 7 | Staging | A Railway service with the image and data; `OSRM_URL` on the services that call it | Owner approves the cost (G23) | Staging answers routes; only the services whose PR changed are redeployed |

## Path to scale
- **City:** feed our measured segment speeds into `osrm-customize` (MLD) by hour band; two or more replicas behind the services.
- **Multi-city:** one data set per city or region, chosen by the service area; an AWS deployment next to the backend.

## Risks
| Risk | Mitigation |
|---|---|
| OpenStreetMap gaps in Bengaluru: missing one-way tags, new roads, missing signals | Golden routes; the road test; fix the map in OpenStreetMap itself (open data), then rebuild |
| OSRM down or slow | The fallback answers in a straight line and says so; alerts on fallback rate |
| Memory and cost on Railway | Measured (#183): Bengaluru builds with a peak of about 750 MB and serves in about 220 MB; the data is about 330 MB on disk |
| Licence | OpenStreetMap data is ODbL: credit "© OpenStreetMap contributors" wherever routes or maps from it are shown |

## Measured on the first build (#183, extract of 2026-11-11)
- Build: about 30 s on 8 cores, peak 750 MB (`osrm-extract`); `osrm-routed` serves Bengaluru in about 220 MB.
- On the test zones, against a one-way takes 3.4 to 20 times longer than along it (43 one-way streets checked, 11 kept as golden pairs).
- Signals: crossing a signalised junction straight costs exactly the 30 s wait (10 crossings checked), paid once per junction even where a dual road has a signal node on each side. Longer routes also carry turn costs, so they are not used to check signals. The road test (W8) replaces the 30 s placeholder.
- A `maxspeed` tag can lower a road's speed but never raise it above the scooter table (car.lua alone would use maxspeed x 0.8, which is faster on most tagged roads).
- Closed streets (`motor_vehicle=no`) are routed round (4 checked).
- The fastest segment on the golden routes is 40.2 km/h: the 40 km/h cap holds.
- Not checked by a golden route yet: U-turns, right turns across traffic, the per-road-type ceiling (OSRM's annotations do not name the road type; the 40 km/h cap is checked), and the motorway rule (no motorway in the test zones). Motorways are excluded unless tagged `motorcycle=yes`, which may exclude stretches two-wheelers can legally use, such as the Electronic City flyover: check by hand before any zone near one.

## Open questions
- G23: where OSRM runs for staging, and the cost the owner accepts.
