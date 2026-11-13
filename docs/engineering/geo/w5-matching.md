# W5: Matching

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
For each paid order, offer it to the rider who reaches it fastest by road, without leaving the area uncovered, and explain every choice afterwards.

## Today
- At order creation, `availability.PickScooter` holds the stock on the nearest scooter (straight line from its node) that has the whole basket.
- `offers.go` offers the order to that scooter's rider first, then to the nearest other free scooter with the basket; one rider at a time; `offer_timeout_s`; one order per rider.

Both places choose a scooter, so both must use the same ranking.

## The design: filter, then rank

```
Candidates : riders on shift, free, whose scooter holds the whole basket
Filter     : the rider's tier for this gate (w2, w3): timer zone or sweet spot first,
             then each ring, up to max_search_radius_m
             (a stale position falls back to the node, and is marked)
Rank       : cost = live road time from the rider's exact position to the gate
                    (one OSRM table call for all candidates)
                  + w_cov   × coverage lost if this scooter leaves   (from w4; 0 until then)
                  + w_stock × scarcity of this scooter's stock        (holding the last units nearby)
Battery    : a filter, not a weight: too low to finish and return means not a candidate (G21)
Offer      : the cheapest first; on decline or timeout, the next one (as today)
Record     : the candidates, their costs and the choice, kept with the offer
```

The weights are settings. The filter-then-rank shape is the documents' "Filtered + Ranked" (Option A); the coverage term is the emergency-services "preparedness" idea (`research.md`, section 2).

## Options

| Choice | Options | Recommendation |
|---|---|---|
| Method | Ranked choice / Hungarian on the candidates / batch every 1 to 2 s / ML scores | **Ranked choice** at pilot size. Batching with an assignment solver only when orders often compete (city scale) |
| Two orders at once | Each order picks in turn under row locks (today) / batch | **Oldest order first**, each under row locks (as today) |
| Where the code lives | Inside `availability` and `offers.go` / one new package used by both | **A new package `internal/matching`**, pure functions with no database, used by both and by the simulator |
| Explaining choices | Logs only / stored with each offer | **Stored**: ops trust, debugging and simulator replay all need it |
| Rings shown to the customer | Yes / no | **No** (decision 0011 #8): only "looking for riders" |

### Second order for a busy rider (G19, open)
| Option | Rule | Effect |
|---|---|---|
| A. Never (today) | One order per rider | Simple; idle riders get the next order |
| B. Queue it | Give it to the busy rider only if finishing the first order and then riding to the new gate beats the best free rider, and the new order's timed leg is still safe | More orders served when riders are short |
| C. Batch two orders in one trip | Both gates on one route | Needs packing and timer rules for two orders: a flow change |

Until the owner answers, A stays.

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | `internal/matching` | Nothing new | Move today's choice into pure functions, unchanged in behaviour | Today's tests still pass untouched. Built (#209): `internal/matching` (`ByStraightLine` is today's order) |
| 2 | Road-time ranking | `w1` | One table call per decision; fallback to straight line | The rider nearer by road wins over the one nearer in a straight line (a median test case). Built (#209), behind the setting `matching_by_road` (0, today, by default): one table call from each rider's live position (the node when stale), with a straight-line fallback |
| 3 | Tier filter | `w2`, `w3` | Filter by tier, ring by ring | Tests per tier; a deliberate break is caught. Built (#211): with `matching_by_road` on, reach itself is by road (within `sweet_spot_budget_s`, out to `max_search_radius_m`), for the pick and the menu alike |
| 4 | Record decisions | A column or table for each offer's ranking (migration renumbered before the PR) | Written in the same transaction as the offer | Ops can see why a rider was chosen |
| 5 | Coverage and stock terms | `w4` | Add the terms with weights from settings | The simulator shows a gain before the weights leave zero |
| 6 | Battery filter | Scooter battery data (`battery_pct`, #158) and the owner's answer (G21) | A minimum battery setting | Only after the owner's answer |
| 7 | Second order | The owner's answer (G19) | Option B or C as chosen | Only after the owner's answer |

## Path to scale
Batching every 1 to 2 s with the Hungarian method or min-cost flow; ML travel-time scores; a separate dispatch worker.

## Risks
| Risk | Mitigation |
|---|---|
| OSRM slow during a decision | Timeout and the straight-line fallback; the decision is recorded with its source |
| Gaming: a rider parks just inside the sweet spot to get orders | Fine: that is the behaviour we want; coverage terms stop clustering |
| Weights tuned by feel | Change weights only with a simulator result attached |

## Open questions
- G19: second orders. G21: battery.
