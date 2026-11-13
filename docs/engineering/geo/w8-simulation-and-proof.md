# W8: Simulation, road test and proof of concept

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Prove each geo rule before it touches a real order: first on a map, then in a simulator, then on the road in Bengaluru. Every later change to a budget, weight or method comes with a simulator result.

## Proof of concept, in three stages

| Stage | What | Needs | Pass when |
|---|---|---|---|
| **PoC 1: on the map** | Build OSRM and the cell sets for the test gates; view them | `w1`, `w2` | The owner and ops agree, by eye and local knowledge, that the tiers look right: no cell across a median in the timer zone, no lake cells |
| **PoC 2: in the simulator** | Run today's rule and the new rules on the same simulated days | `w1`, `w2`, the simulator | The new rules serve more orders within the timer, with a shorter wait before it, at the same fleet size |
| **PoC 3: on the road** | A rider rides real legs in the test zones at different hours | A rider, a scooter and a day, arranged by the owner | OSRM's predicted times are close enough to real ones (for example p90 error under 15 s), or the profile is recalibrated until they are |

## The simulator

| Part | What it models |
|---|---|
| World | OSRM, the test zones, gates, nodes and current settings |
| Orders | Random arrivals per cell and band (from `w7`, or hand-entered) |
| Riders | Shifts, accepting or declining offers, packing time, speed variation, signal waits, breaks |
| Rules under test | Matching (`w5`), placement and relocation (`w4`), zone shapes (`w2`) |
| Measures | Share of timed legs within the timer; wait before the timer (p50, p90); refusals before payment; moves per rider-hour; idle share; **the speed a rule needs**: a rule that only meets the timer above the profile's speed cap fails |

### Options
| Option | For | Against | Verdict |
|---|---|---|---|
| Python in `tools/sim` (SimPy or a custom loop) | Fast to write, good for charts | The rules would be written twice and could drift from the real Go code | For analysis and charts |
| **Go, reusing `internal/matching` and the placement functions** | Tests the real code; no drift | A little more work | **The simulator itself** |

Decision 0002 allows Python only for offline jobs in `tools/`, and allows Go everywhere, so either fits. The rules must be pure functions with no database for the Go simulator to reuse them: that is a design rule for `w4` and `w5`.

## First comparison (#205, 2026-11-13)
Command: `go run ./cmd/sim -osrm <test-zone OSRM> -zone <z> -orders 200 -seed 1 -seeds 5 -sweet-spot <b>`.

The setup:
- The scooter profile `scooter-2`, whose numbers are placeholders until the road test, on the extract of 2026-11-11.
- Six riders at fixed points in each test zone, each free for every order (there is no fleet state yet).
- Gates drawn at random and snapped to the nearest road.
- The timer runs 90 s, from the 350 m zone edge to the 100 m perimeter. The wait and the timed leg are legs of one OSRM route through the rider, the timer start, the timer stop and the gate, with the route's heading at the start and stop. So signal and turn costs count, and the path is the one ridden.
- `today` is only the distance rule of `availability.PickScooter`, without its stock, basket and zone checks.

Koramangala: 5 seeds × 200 orders, 1,000 orders per rule. Counts are pooled; 95% Wilson intervals are in brackets.

| Sweet spot | Rule | Served | In time, of served | **In time, of all orders** | Timed p50 / p90 (seed 1) |
|---|---|---|---|---|---|
| **80 s (now)** | today | 296 | 67.6% (62.0–72.6) | **20.0% (17.6–22.6)** | 68 / 218 s |
| | road | 78 | 100% (95.3–100) | **7.8% (6.3–9.6)** | 23 / 37 s |
| 120 s | today | 628 | 64.5% (60.7–68.1) | 40.5% (37.5–43.6) | 71 / 174 s |
| | road | 213 | 96.2% (92.8–98.1) | 20.5% (18.1–23.1) | 52 / 82 s |
| 180 s | today | 939 | 65.7% (62.6–68.7) | 61.7% (58.6–64.7) | 74 / 156 s |
| | road | 500 | 84.0% (80.5–87.0) | 42.0% (39.0–45.1) | 64 / 101 s |

Indiranagar, seed 1 only. The pooled run was stopped for low memory on the dev PC; run it again when there is room. Its pattern matches Koramangala's:
- At 80 s: today served 95 of 200, 66.3% of them in time; road served 24, all in time.
- At 120 s: today served 174, 60.3% in time; road served 69, 98.6% in time.

What it shows, and what it does not:
- **Today's rule breaks the timer on about a third of the orders it accepts:** 62 to 73% in time, at every budget. The slowest tenth of its timed legs take 150 to 220 s. A straight line does not see one-ways, medians or signals.
- **Today's rule still gets more customers their order in time per order placed,** because it accepts far more orders. At 80 s that is 20% against the road rule's 7.8%. It pays for that with late arrivals, each owed a remedy credit.
- **The road rule rarely makes a promise it cannot keep.** It refuses what it cannot reach in the budget, and what it accepts is in time 96 to 100% up to 120 s.
  - This follows from the rule: it only takes riders whose whole trip fits a budget under the timer. So it is a property of the rule, not a finding about Bengaluru.
  - Past 120 s its in-time share falls (84% at 180 s), because the budget then admits riders whose timed leg can exceed 90 s.
- **The real choice for the owner** is a product one: accept more orders and break the timer on a third of them, or refuse the ones that cannot be met. A third way is the road rule with a budget near 120 s plus more riders or better placement (W4). That raises orders served without breaking the promise, and the simulator needs fleet state to measure it.
- **This does not yet say how many riders are needed.** Riders are free for every order, there is no placement (W4) and no demand (W7). Served counts are therefore an upper bound.
- **For G24:** these numbers do not justify a budget on their own. They show that `sweet_spot_budget_s` decides the trade-off, and that the road test must replace the placeholder signal and speed numbers before any budget is set.

## Fleet comparison (#214, 2026-11-13)
Command: `go run ./cmd/sim -osrm <test-zone OSRM> -zone <z> -fleet <n> -orders-per-hour <r> -hours 3 -seeds 3`.

The setup:
- Riders get busy: they pack for 60 s, ride to the gate, hand over in 60 s, then ride back to their node.
- Orders arrive as a Poisson stream. Gates are the first gates of the same draw at every rate, so the rates are compared on paired orders.
- The other assumptions are as in the first comparison.

A refusal is **busy** when the rule would have taken a rider had they been free, and **reach** when nobody is within the rule's reach, free or not. Counts are pooled over 3 seeds × 3 hours; 95% Wilson intervals are in brackets.

| Zone | Riders | Orders an hour | Rule | Orders | Served | Refused: busy | Refused: reach | In time, of all orders | Riders busy |
|---|---|---|---|---|---|---|---|---|---|
| Koramangala | 4 | 10 | today | 81 | 15 | 0 | 66 | 9.9% (5.1–18.3) | 5% |
| | | | road | 81 | 3 | 0 | 78 | 3.7% (1.3–10.3) | 0% |
| | 4 | 60 | today | 493 | 70 | 24 | 399 | 10.1% (7.8–13.1) | 20% |
| | | | road | 493 | 27 | 2 | 464 | 5.5% (3.8–7.9) | 5% |
| | 6 | 60 | today | 493 | 105 | 39 | 349 | 13.6% (10.8–16.9) | 22% |
| | | | road | 493 | 37 | 2 | 454 | 7.5% (5.5–10.2) | 5% |
| Indiranagar | 4 | 60 | today | 493 | 115 | 62 | 316 | 16.0% (13.1–19.5) | 30% |
| | | | road | 493 | 33 | 7 | 453 | 6.7% (4.8–9.3) | 7% |
| | 6 | 30 | today | 234 | 83 | 25 | 126 | 22.2% (17.4–28.0) | 14% |
| | | | road | 234 | 23 | 2 | 209 | 9.8% (6.6–14.3) | 3% |
| | 6 | 60 | today | 493 | 155 | 75 | 263 | 21.9% (18.5–25.8) | 27% |
| | | | road | 493 | 51 | 9 | 433 | 10.3% (8.0–13.3) | 7% |

The full sweep (4 and 6 riders, at 10, 30 and 60 orders an hour, in both zones) is in `docs/testing/reports/214.md`.

What it shows, with the placeholder profile and six fixed waiting points:
- **Reach is the larger cause of refusals, and a busy fleet is a real second.** For today's rule, reach is 78 to 100% of refusals, and busy refusals grow with demand: from 0 to 2 of 81 orders at 10 an hour, to 24 to 75 of 493 at 60. For the road rule, which reaches less far, reach is over 97%.
- **Riders are busy 0 to 30% of the time,** with a ride that runs past the end counted in full (under about 3 points high).
- **More riders help a little.** Going from 4 to 6 riders raises in time per order by about 2 to 9 points for today's rule and 1 to 4 for the road rule, and cuts reach refusals (more nodes to be near). Several of these differences sit inside overlapping intervals.
- **Hypothesis for W4, not yet measured:** placing the waiting points by demand and road time (covering models) cuts reach refusals more than adding riders at fixed points. The simulator can test this once placement exists.
- **Not a sizing answer:** the waiting points are fixed, demand is uniform rather than from W7, and the profile is a placeholder.

## The road test (decision 0011 #4)

| Item | Plan |
|---|---|
| Where | The test zones in Koramangala and Indiranagar, or the owner's real pilot area once known |
| What | 5 to 10 gates; from each, 5 to 10 start points spread over the sweet spot and the timer zone |
| When | Three bands: morning peak, midday, evening peak |
| How | The rider app's normal pings (or a GPS logger app); the start and stop marked by the rider |
| Output | Real timed-leg times versus OSRM's; the signal and U-turn costs recalibrated; the first `sweet_spot_budget_s` and timer zone margin |

## Steps

| # | Step | Done when |
|---|---|---|
| 1 | PoC 1 | The owner has seen the cell sets on a map |
| 2 | Simulator v1 with today's rule | It reproduces today's behaviour on a known scenario. Built (#205): `internal/sim`, `cmd/sim`; the first comparison is above |
| 3 | Simulator with the new rules | A written comparison, attached to the matching and placement Issues. Built (#205, #214): the first comparison and the fleet comparison above |
| 4 | Road test | The owner arranges a rider; the results recalibrate the profile and budgets |
| 5 | Keep it running | Every geo PR that changes a rule includes a simulator run |

## Open questions
- G24: seed `sweet_spot_budget_s` from OSRM until the road test?
- Who can ride the road test, and when?
