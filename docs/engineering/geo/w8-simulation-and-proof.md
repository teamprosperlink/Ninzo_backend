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
| 2 | Simulator v1 with today's rule | It reproduces today's behaviour on a known scenario |
| 3 | Simulator with the new rules | A written comparison, attached to the matching and placement Issues |
| 4 | Road test | The owner arranges a rider; the results recalibrate the profile and budgets |
| 5 | Keep it running | Every geo PR that changes a rule includes a simulator run |

## Open questions
- G24: seed `sweet_spot_budget_s` from OSRM until the road test?
- Who can ride the road test, and when?
