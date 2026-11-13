# W4: Rider placement

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Keep free scooters, with the right stock, at the staging nodes that cover the most expected orders within the time budget, and move them when one goes busy. This is the biggest lever on the promise (README, section 2).

## Today
- Staging nodes are legal waiting spots, entered as data. The system assigns a rider a node for the shift (owner, 2026-11-11, #154).
- Relocation (#108): recent paid orders are counted at the node nearest each gate. A node with demand and nobody waiting asks a free rider to move. Straight-line distance. Moves are paid, and a rider may refuse.

## Method (from emergency services, `research.md` section 2)

1. **Coverage matrix:** road time from every node to every demand cell (OSRM table, stored with the road data version).
2. **Expected coverage** of a set of nodes: for each cell, its expected orders × the chance that at least one covering scooter is free. A scooter is busy a share q of the time; a cell covered by k scooters is served with chance 1 − q^k. This is the maximum expected covering model.
3. **Placement table:** for each number of free scooters n = 1 … N, and each time band (weekday, hour), the node set with the highest expected coverage. A greedy pick is enough at pilot size.
4. **Relocation:** when a scooter goes busy or comes free, compare the free scooters' positions with the table row for the new n. Assign free scooters to the target nodes with the least total road time (the Hungarian method: a small square problem). Suggest a move only if the coverage gain is worth it.
5. **Stock-aware coverage** (later): a scooter covers a cell only for the items it holds.

## Options

| Choice | Options | Recommendation |
|---|---|---|
| Where scooters may wait | Fixed legal nodes / any kerb | **Fixed nodes** (legal, safe, known to riders) |
| Placement method | Greedy covering / exact ILP (OR-Tools) / learned values (DiDi-style) | **Greedy** at pilot size; ILP at city scale; learning only with lots of data |
| When to re-plan | Every event / on a timer / both | On each busy or free change, at most once per `relocation_rest_s` per rider |
| Who decides moves | System suggests, rider accepts (today) / forced | **Suggest** (today's rule: no penalty for refusing) |
| Avoiding churn | Move only if the gain is above a threshold | A setting, `relocation_min_gain` (new) |

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | Node catalogue | Ops-entered nodes with approach points and legality notes | The console (other session) already edits places; check the fields | Each pilot node is legal, reachable, and has a snapped road point. Placement plans (#224) choose among the zone's non-test `staging_nodes`, so ops-entered nodes are the candidates; legality and approach points are still ops' to check |
| 2 | Coverage matrix | `w1`, `w2` | A `tools/` or Go job: node × cell road times, stored as `node_cells` | Rebuilt with each road data version |
| 3 | Demand per cell and band | `w7` | Read from `w7`'s counts or forecast | A test data set exists for the simulator |
| 4 | Placement tables | Steps 2 and 3; busy share q (a setting, starting about 0.3 until measured) | Greedy maximum expected covering for n = 1 … N, per band | Stored as `placement_plans`; viewable on a map. The method is built and tested in the simulator (#216, `internal/placement`); storing plans per band is next. **Built (#224):** `placementplan.Build` and `cmd/placement -zone` store plans per weekday, half-hour and n in `placement_plans`; `placementplan.Latest` reads them. Not yet on a map, not rebuilt on a schedule, and read by nothing until relocation v2 |
| 5 | Relocation v2 | Steps 4 and the existing relocation flow | Replace "nearest node to each gate" with the table and the Hungarian assignment; keep the suggestion, pay and rest rules | The simulator shows more orders served within the budget than today's rule, at the same or fewer moves. **Built (#226), off by default:** with `relocation_by_plan` = 1, suggestions follow the zone's placement plan for the weekday, half-hour and free riders (short: the plan wants more than are waiting; donor: the nearest with more than the plan wants), and fall back to today's rule when there is no plan. The suggest, accept, pay and rest flow is unchanged (owner, 2026-11-13: "make sure the flow stays the same"). Donors are chosen by nearest straight line, not the Hungarian method yet, and the simulator has not compared it with today's rule yet |
| 6 | Shift start | Step 4 | A rider starting a shift is given the best open node in the table row | Tested with several riders starting together |
| 7 | Stock-aware coverage | Stock per scooter (exists) | Coverage per item | Shown in the simulator before use |

## First placement result (#216, 2026-11-13)
`go run ./cmd/sim -osrm <test-zone OSRM> -zone <z> -fleet <n> -orders-per-hour <r> -hours 3 -seeds 3 -seed <s> [-place]`.

- **Placed:** greedy maximum expected covering (q = 0.2, one scooter per point) on 6 × 8 grid points snapped to the nearest road within 60 m, which can be any road, arterials included. A point covers a gate within the 80 s sweet spot by road. Each fit uses 150 gates drawn with a seed of its own (1000 + s), and is scored on seeds s to s + 2.
- **Fixed:** the six points hand-picked in #205 (the centre and five around it). They were never chosen to be good, so this is a weak baseline.
- **Otherwise as the fleet comparison in w8:** busy riders, Poisson arrivals, uniform demand, the placeholder profile.

### Four independent fits, 30 orders an hour
Four fits (s = 1, 4, 7, 10) per zone, pooled: 1,034 orders per arm. The figure is in time, of all orders placed; 95% Wilson intervals are in brackets.

| Zone | Rule | 4 fixed | 4 placed | 6 fixed |
|---|---|---|---|---|
| Indiranagar | Road | 8.4% (6.9–10.3) | **18.3%** (16.0–20.8) | 13.8% (11.9–16.1) |
| | Today's | 17.3% (15.1–19.7) | **27.4%** (24.7–30.2) | 24.7% (22.1–27.4) |
| Koramangala | Road | 6.7% (5.3–8.4) | **10.7%** (9.0–12.8) | 8.3% (6.8–10.2) |
| | Today's | 13.5% (11.6–15.8) | 15.9% (13.8–18.2) | 17.7% (15.5–20.1) |

The difference for each fit, in points (placed 4 minus fixed 4; placed 4 minus fixed 6). Each pair shares the same orders:

| Zone | Rule | Placed 4 − fixed 4 | Placed 4 − fixed 6 |
|---|---|---|---|
| Indiranagar | Road | +12.0, +10.2, +7.3, +10.3 | +9.0, +5.4, +0.4, +3.6 |
| | Today's | +8.5, +9.9, +8.1, +13.9 | +3.4, +3.7, −2.2, +6.3 |
| Koramangala | Road | +4.7, +5.5, +0.7, +5.6 | +3.0, +4.4, −1.5, +4.0 |
| | Today's | +3.8, +0.4, +0.4, +5.1 | +0.4, −3.2, −4.0, 0.0 |

The first single-fit sweep at 4 and 6 riders and 30 and 60 an hour is in `docs/testing/reports/216.md`. It points the same way.

### What it shows
With the placeholder profile and uniform demand:
- **Against the same number of fixed riders, placement is at least as good in every fit, and clearly better in three of the four zone and rule pairs.** All 16 per-fit differences are positive, but two of them (Koramangala, today's rule) are +0.4 points, about one order in 270, which is a tie.
  - Road rule: +7 to +12 points in Indiranagar (about twice the in time), and +1 to +6 in Koramangala. The pooled intervals do not overlap in either zone.
  - Today's rule: +8 to +14 in Indiranagar, where the intervals do not overlap; +0.4 to +5 in Koramangala, where they do.
- **Placing 4 against adding 2 more fixed riders depends on the zone and the rule.**
  - Indiranagar, road rule: 4 placed are ahead of 6 fixed on the pooled figure (18.3% against 13.8%), but the intervals just touch (16.0–20.8 against 11.9–16.1). Per fit it is +0.4 to +9, so one of four fits is a tie.
  - Indiranagar on today's rule and Koramangala on the road rule are mixed: three fits ahead, one behind, and the pooled intervals overlap.
  - Koramangala, today's rule: 6 fixed are as good or better.
  - The earlier "4 placed beat 6 fixed in Indiranagar" was one fit. Pooled, it is ahead for the road rule only, and not clearly.
- **The objective favours the road rule.** Placement maximises "road time within the sweet spot", which is exactly the road rule's acceptance test. Today's rule accepts by straight-line reach, so part of the larger road-rule gain is built in. A placement for today's rule would cover by straight-line distance.
- **Still mostly a reach problem.** Even placed, 4 to 6 riders keep only 17 to 38% of the zone's demand within the 80 s sweet spot by road (from the single-fit sweep in the report, placement seed 1001).
- **What these runs are not:**
  - The hold-out is on the same uniform demand, so it tests sampling noise, not a change in where orders come from.
  - Greedy's closeness to the best placement is not measured; it is guaranteed only within (1 − 1/e).
  - q = 0.2 here, while step 4 starts at 0.3; the measured busy shares were 2 to 30%.
- **Not yet real:**
  - The placed points are road-snapped grid points, not checked legal waiting spots (W4 step 1, nodes from the console).
  - Demand is uniform (W7).
  - The profile is a placeholder.
  - Scooters do not move during the day (relocation, W4 step 5).
- Found on the way: OSRM can answer -0.2 m for a point on the destination, and routing refused the whole table. Fixed in #217.
- **Uneven demand (#222, w7):** with synthetic hotspots, placement that sees where demand is beats the fixed points (7 of 8 placements under both rules), and beats placement as if demand were even (all 8). Fitted as if even, it can be worse than the fixed points. Forecast per cell does as well as placing on the demand draws directly; it adds nothing beyond knowing the demand here.

## Path to scale
Exact placement with OR-Tools; plans per zone; learned values of a free scooter at a place and time once there is enough data.

## Risks
| Risk | Mitigation |
|---|---|
| Riders refuse moves | Paid moves (exists); fewer, better moves; measure acceptance |
| Too many moves annoy riders | The gain threshold and the rest time |
| Bad demand data early | Owner or ops can enter expected demand by hand until real orders exist |
| A legal parking spot that is not legal at all hours | Node hours in the catalogue |

## Open questions
- What coverage target counts as good enough for the pilot (for example 90% of expected orders within the sweet spot)?
