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
| 1 | Node catalogue | Ops-entered nodes with approach points and legality notes | The console (other session) already edits places; check the fields | Each pilot node is legal, reachable, and has a snapped road point |
| 2 | Coverage matrix | `w1`, `w2` | A `tools/` or Go job: node × cell road times, stored as `node_cells` | Rebuilt with each road data version |
| 3 | Demand per cell and band | `w7` | Read from `w7`'s counts or forecast | A test data set exists for the simulator |
| 4 | Placement tables | Steps 2 and 3; busy share q (a setting, starting about 0.3 until measured) | Greedy maximum expected covering for n = 1 … N, per band | Stored as `placement_plans`; viewable on a map |
| 5 | Relocation v2 | Steps 4 and the existing relocation flow | Replace "nearest node to each gate" with the table and the Hungarian assignment; keep the suggestion, pay and rest rules | The simulator shows more orders served within the budget than today's rule, at the same or fewer moves |
| 6 | Shift start | Step 4 | A rider starting a shift is given the best open node in the table row | Tested with several riders starting together |
| 7 | Stock-aware coverage | Stock per scooter (exists) | Coverage per item | Shown in the simulator before use |

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
