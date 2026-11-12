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
| 1 | Pin OSRM and build Bengaluru | The OSRM Docker image at a fixed version; the Geofabrik extract; `osmium` | A script in `tools/osrm/` downloads, clips, and runs `osrm-extract`, `osrm-partition`, `osrm-customize` with the profile | One command builds the data from nothing; the build records the extract date and profile version |
| 2 | Scooter profile v1 | The rules above as a Lua profile | `tools/osrm/profiles/scooter.lua`, starting from `car.lua` | Golden routes pass (step 3); memory and build time measured and written here |
| 3 | Golden routes | 15 to 30 Bengaluru pairs in the test zones with known facts: a one-way street, a median, a signal | A test file of pairs with expected rules (for example "A to B must not take the one-way street", "across the median takes over 60 s") | Tests fail if the profile breaks a rule (proved with a deliberate break) |
| 4 | Local OSRM | The W0 compose project | An `osrm` service on its own port, mounting the built data | `task` command starts it; a health check answers |
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
| Memory and cost on Railway | Measure in step 2 before asking the owner for a figure |
| Licence | OpenStreetMap data is ODbL: credit "© OpenStreetMap contributors" wherever routes or maps from it are shown |

## Open questions
- G23: where OSRM runs for staging, and the cost the owner accepts.
