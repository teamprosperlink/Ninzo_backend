# W2: Map grid and map distribution

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
For every gate, know in advance which cells are in its **timer zone**, its **sweet spot** and its wider **rings**, by road time, so the live path only looks things up. This is the owner's hybrid (H3 grid + circle + road time) and the pre-mapping idea (`docs/context/algorithms/2026-11-11-owner-notes-hybrid-and-premapping.md`).

## Today
No grid. The timer zone is a 350 m circle; reach is a straight line at a set speed.

## The design

```
For each gate:
  1. Snap the gate to its road (OSRM nearest). Ops can set a different approach point.
  2. Candidate cells: H3 res 10 cells around the gate, out to max_search_radius_m.
  3. Road time from each cell to the gate (OSRM table, many cells to one gate).
  4. Store every cell with its road time and the road data version.
  5. Tiers are read at query time from the current settings:
       timer zone : centre inside zone_radius_m AND timed leg to the 100 m perimeter
                    <= timer_length_s minus a safety margin
       sweet spot : road time to the gate <= sweet_spot_budget_s
       rings      : wider time bands, up to max_search_radius_m
```

**Why store times, not tiers:** when the owner changes a budget or the radius in the console, the tiers change at once with no rebuild. Only a new gate or new road data needs a rebuild.

**The timer zone needs more care than the rest.** The timer stops at the 100 m perimeter, not at the gate, and a res-10 cell is about 120 m across. For the cells inside the circle (about 50 to 100 per gate), use full routes, not the table, and measure the time to the point where the route crosses the perimeter. Take the slowest of the cell's centre and its corners, so the zone errs on the safe side.

## Options

### How Go gets H3 (G18, decided 2026-11-13: our own hexagon grid for the pilot)
| Option | For | Against |
|---|---|---|
| A. `uber/h3-go` with cgo | Official, complete | Needs `CGO_ENABLED=1` and a base image with a C library, instead of today's static distroless image |
| B. A pure-Go port (`ziprecruiter/h3-go` is the most complete) | Keeps the static build | Early and partial: must be proved against the official library |
| C. Cells only offline (Python `h3` in `tools/`); live checks by PostGIS on the merged cell outlines | No new Go dependency | Pings get no cell ID live; demand per cell is computed offline |
| D. The `h3` Postgres extension | Cells inside SQL | **Not available on Ninzo's Supabase project** (checked 2026-11-11) |
| E. No H3 for the pilot: a PostGIS hexagon grid (`ST_HexagonGrid`, PostGIS 3.1 and later) or plain geometry | No new dependency; exact; `real_and_stand_ins.md` already says PostGIS is enough at pilot size | No hierarchy and no shared cell IDs; moving to H3 at city scale means rebuilding the cell sets |

**Recommendation:** a short spike first. We need four functions: point to cell, cell to centre, cells within k rings, and the cell outline. Generate test vectors with the official Python library in `tools/`. If option B matches them exactly, use B. If not, choose between C and E for the pilot, and use A only if the image changes anyway. The road-time cell sets, the tiers and every later step work the same with any of these grids.

**Spike result (#193, 2026-11-13).** Official vectors came from Uber's H3 4.1.2.
- Option B, `ziprecruiter/h3-go` v0.5.0, does not export cell centre or outline.
- `akhenakh/goh3` (H3 4.4.1 machine-translated to pure Go) is experimental and has no Windows build, so it cannot compile on the development PC. Running it was blocked by the local permission check on unvetted code, so its accuracy is unmeasured.
- Options A (cgo) and D (Postgres extension) were already ruled out.

**Decision:** our own pointy-topped hexagon grid in pure Go, `internal/hexgrid`. It uses a local flat projection around each city's origin. The Bengaluru grid `blr76` has 76 m edges, the same average area as H3 resolution 10, and neighbour spacing is within 0.12% on the real Earth over the city.

We lose global H3 ids and the parent and child levels, which the pilot does not use. Moving to H3 at city scale costs a rebuild of the cell sets, which happens with every road data version anyway. Revisit when a pure-Go H3 builds on all three platforms and matches the official vectors.

### Resolution
| Resolution | Average edge | Across (about) | Area (about) | Use |
|---|---|---|---|---|
| 9 | 174 m | 300 to 350 m | 0.105 km² | Demand at city scale (`w7`) |
| **10** | **66 m** | **115 to 130 m** | **0.015 km²** | **The main grid (G7)** |
| 11 | 25 m | 45 to 50 m | 0.002 km² | Not used. The 100 m perimeter stays an exact distance (G8): a cell about 50 m across would blur the stop line by about ±25 m |

### Rings: by distance or by time?
The documents use distance (500 m, 1 km, 2 km). **Proposed: by time**, capped by `max_search_radius_m`, because time is what the customer waits. Distance rings stay available as a setting if ops prefer them.

### Where cell sets are kept
| Option | Recommendation |
|---|---|
| Postgres table `gate_cells` (gate, cell, road seconds, snap distance, road data version, built at) | **Yes**: the source of truth, joined with gates and settings |
| Also a merged outline per tier (PostGIS polygon) | **Yes**: for the console map and for option C |
| Redis sets | At city scale, as a cache |

### Per gate, or per destination cell?
- **Per gate** (pilot): exact, and gates are few.
- **Per destination cell** (city): compute once for every cell in a service area as a destination; a new gate then uses its cell's set at once. Roughly 66 cells per km², so a 5 km² area means about 330 destinations, each with a few thousand source cells: easy for OSRM.

## Pre-mapping and refresh

| Trigger | What is rebuilt |
|---|---|
| A gate is created or moved | That gate, in the background, in about a second. Until then the straight-line fallback answers (principle 3) |
| New road data or a new profile | Every gate, as one job, with the new version |
| Nightly | Any gate whose set is missing or older than the current road data |
| A setting changes | Nothing: tiers are read from the settings at query time. A running order keeps what it started with (G25) |

A brand-new pin (edge case 7) is never refused for lack of a set; it is mapped on creation.

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | H3 spike (G18). Done (#193): see the result above | Test vectors from the official library | Compare options B and C on the four functions | A written result here, and the choice recorded in the decision log |
| 2 | `internal/hexgrid` (was `internal/h3cells`). Built (#193) | The chosen option | Point to cell, centre, disk, outline; nothing else | Its property tests pass: a point is in its own cell and nearest its centre, disks are complete, ids round-trip, and the real-Earth spacing is within 0.5% over the city (#193) |
| 3 | `gate_cells` and its build | W1 client; a migration (renumbered to the next free number before the PR) | A job builds a gate's cells; a command rebuilds all | A test gate across a median from some cells excludes them from the timer zone; the timer zone is always inside the circle; a deliberate break is caught. Built (#196): `gate_cell_builds`, `gate_cells`, `internal/gatecells`, `cmd/gatecells build|build-all`; erasure redacts builds |
| 4 | Tiers from settings | A function that, given a gate and a cell, answers the tier from the stored time and current settings | A pure function plus a query | Changing a budget in a test changes the tier with no rebuild |
| 5 | Settings | `timer_zone_margin_s` (new); ring budgets (new); existing `zone_radius_m`, `sweet_spot_budget_s`, `max_search_radius_m` | Through `geo.ChangeSettings`, so every zone stays valid | Each is a console setting with limits |
| 6 | Use the timer zone (G20) | The owner's answer | Behind a setting: `circle` (today) or `road`. **Each order keeps the zone it started with** (G25): the road data version and the cells in force when its offer was accepted, as it already keeps its timer length and radius (decision 0007) | Both modes pass the timer tests; a test proves a rebuild during a running order does not move its timer |
| 7 | Export for viewing | Cell sets as GeoJSON or a kepler.gl file | A command in `tools/` | The owner and ops can see each gate's tiers on a map |

## Measured on the first build (#196)
On the test-zone data with profile `scooter-2`, a gate on 80 Feet Road, Koramangala (12.93545, 77.61439), with a 1,500 m search radius:

| Road time to the gate | Cells | Area |
|---|---|---|
| 60 s | 2 | 0.03 km² |
| 80 s (today's `sweet_spot_budget_s`) | 4 | 0.06 km² |
| 120 s | 12 | 0.18 km² |
| 180 s | 24 | 0.36 km² |
| 300 s | 165 | 2.48 km² |

All 472 cells reach the gate. With the placeholder scooter numbers (30 s per signal, city speeds of 18 to 40 km/h), an 80 s sweet spot is small: a waiting rider would have to be within about 200 m of the gate. This is evidence for G24 (seed the budget from OSRM) and for the road test (W8), which replaces the placeholders.

## Path to scale
Per destination cell instead of per gate; Redis as a cache; hour bands (peak and off-peak sets) once measured speeds exist.

## Risks
| Risk | Mitigation |
|---|---|
| A cell centre falls in a building or a lake | Record the snap distance; drop cells whose snap is too far; use corners as well as the centre |
| The gate snaps to the wrong road (main road instead of the service lane) | An approach point ops can set per gate |
| OSRM's times are wrong for Bengaluru | The road test (`w8`), then learned times (`w6`) |
| A zone too tight leaves few riders eligible | The simulator shows the trade-off before the setting changes |

## Open questions
- G18 (engineering): the H3 option, after the spike.
- G20 (owner): build the road-time timer zone now?
