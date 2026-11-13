# W7: Demand

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Know where and when orders come, per cell and time band, including the orders we lost, so placement (`w4`) and the fleet size per hour can be planned.

## Today
Relocation counts recent paid orders at the node nearest each gate, over `relocation_window_s`. Nothing else.

## What counts as demand

| Signal | What it tells us | Stored today? |
|---|---|---|
| Paid orders | Served demand | Yes |
| Refused before payment ("not available right now") | **Lost demand**: we were not there | Check |
| "Notify me" requests | Lost demand, with intent | Yes (#101) |
| Availability checks before sign-in | Interest | **No**: decision 0011 #9 says a location is checked "without saving anything" |

Lost demand matters most: counting only served orders hides the places where we have no scooters.

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | Demand counts | `w2` cells | A table of counts per cell, weekday and 30-minute band, for paid, refused and notify-me | Rebuilt from history; updated as orders come. **Built (#220):** availability checks are kept in `demand_checks` (offered, no_coverage, sold_out, zone_off, out_of_service_area); `demand.Counts` adds paid orders at the gate cell. Notify-me is not counted yet |
| 2 | Forecast v1 | Step 1 | A weighted average of the same weekday and band, recent weeks counting more | Its error is measured each week. **Built (#220):** `History.Forecast` and `History.Backtest` (mean absolute error); not yet run weekly, and not yet fed to placement (W7b) |
| 3 | Cold start | The owner or ops | Expected demand entered by hand per area and band until real data exists (offices, colleges, events) | Placement has something to plan with on day one. The forecast-to-placement chain is tested in the simulator (#222); ops entry still needs the console |
| 4 | Forecast v2 | Months of data | Gradient-boosted trees with weather and holidays | Only if it beats v1 on the measured error |

## Forecast to placement in the simulator (#222, 2026-11-13)
The chain recorded demand → forecast v1 → placement runs end to end on synthetic data:
- **History:** `sim.CellDemand` draws four past weeks of 150 gates each from the scenario's demand. For fit s the seeds are 2000 + 4s to 2003 + 4s, so no two fits share a week. It counts them per `blr76` cell and forecasts the next week with `demand.History.Forecast` (decay 0.8).
- **Placement:** `sim.PlaceOnCells` places on the cell centres with those weights.

`go run ./cmd/sim -osrm <test-zone OSRM> -zone <z> -fleet 4 -orders-per-hour 30 -hours 3 -seeds 3 -seed <s> -hotspots <h> [-place -place-from uniform|draws|forecast]`

Demand has synthetic hotspots that I chose: 60% of gates come from two of them, the rest are uniform.
- Koramangala: `12.9355,77.6245,0.35,200;12.928,77.636,0.25,150`
- Indiranagar: `12.978,77.640,0.4,200;12.968,77.647,0.2,150`

The four arms:
- **Fixed:** the hand-picked points from #205.
- **Uniform:** placed as if demand were even (#216's method), on 150 uniform draws (seed 1000 + s).
- **Draws:** placed on 150 gates drawn from the real hotspot demand (seed 1000 + s), each weighing the same.
- **Forecast:** placed on the forecast per cell.

There are four placements per arm and zone (s = 1, 4, 7, 10). Each is scored under both rules on the same orders, so 8 placements per arm in all, not 16 independent results. Pooled: 1,034 orders per arm. The figure is in time, of all orders placed; 95% Wilson intervals are in brackets.

| Zone | Rule | Fixed | Uniform | Draws | Forecast |
|---|---|---|---|---|---|
| Koramangala | Road | 18.9% (16.6–21.4) | 9.5% (7.8–11.4) | 27.0% (24.4–29.8) | 29.3% (26.6–32.2) |
| | Today's | 26.5% (23.9–29.3) | 14.8% (12.8–17.1) | 31.8% (29.1–34.7) | 33.4% (30.6–36.3) |
| Indiranagar | Road | 16.9% (14.8–19.3) | 18.7% (16.4–21.2) | 36.0% (33.1–38.9) | 36.7% (33.8–39.6) |
| | Today's | 26.2% (23.6–29.0) | 28.0% (25.4–30.9) | 43.1% (40.1–46.2) | 43.3% (40.3–46.4) |

The forecast arm's difference for each placement, in points. Each pair shares the same orders:

| Zone | Rule | − fixed | − uniform | − draws |
|---|---|---|---|---|
| Koramangala | Road | +12.8, +6.6, +8.8, +14.2 | +12.4, +16.5, +19.0, +31.2 | +8.1, 0.0, +1.8, 0.0 |
| | Today's | +10.7, −1.1, +3.3, +15.8 | +8.9, +11.0, +22.3, +31.6 | +4.7, 0.0, +1.9, 0.0 |
| Indiranagar | Road | +20.1, +19.8, +17.9, +21.4 | +19.7, +24.2, +11.7, +16.6 | +5.1, −0.7, −1.1, 0.0 |
| | Today's | +15.0, +16.5, +19.0, +17.8 | +14.1, +21.7, +10.6, +14.6 | +4.7, −0.7, −2.6, 0.0 |

What it shows, on synthetic data:
- **Placing on where demand is beats placing as if it were even.**
  - Forecast beats uniform in all 8 placements under both rules.
  - The pooled intervals do not overlap in any row.
- **It beats the fixed points too.**
  - 7 of 8 forecast placements beat the fixed points under both rules. The eighth (Koramangala, s = 4) is ahead by 6.6 points on the road rule and slightly behind on today's (−1.1).
  - The pooled intervals do not overlap in any row.
  - This is despite the hotspots favouring the fixed points: a hand-picked point sits about 170 m from Koramangala's larger hotspot and about 150 m from Indiranagar's smaller one.
- **The forecast step adds nothing beyond knowing the demand, here.**
  - Forecast against draws is −2.6 to +8.1 per placement; several are 0.0, the same points chosen.
  - The pooled intervals overlap in every row.
  - What matters is that placement sees where demand is. Recorded demand and forecast v1 deliver that in a form the backend can store, but this run cannot tell a good forecast from a perfect one, since the history comes from the same demand it is scored on.
- **Placing as if demand were even can be worse than the fixed points when demand clusters.** In Koramangala the road rule halves (18.9% to 9.5%), and today's rule drops 44%. So placement needs demand data, not just road times.
- **What these runs are not:**
  - The hotspots are mine, not measured.
  - Drift from week to week, which forecast error measures, is not tested.
  - The arms are fitted on different samples (forecast on 600 gates collected into cells, draws and uniform on 150), so a gap of a few points between them is within that difference.
  - As in #216, the objective is the road rule's own test, the profile is a placeholder, and the points are road-snapped grid points, not legal waiting spots.

## Options

| Choice | Options | Recommendation |
|---|---|---|
| Cell size | Res 10 / res 9 | **Res 9** for counting (enough orders per cell), res 10 for placement lookups |
| Time band | 15 / 30 / 60 minutes | **30 minutes** |
| Model | Averages / Prophet / gradient-boosted trees | **Averages** first |

## Path to scale
Per-zone models; events and weather; demand that moves with price or menu changes.

## Risks
| Risk | Mitigation |
|---|---|
| Too few pilot orders to forecast | Hand-entered expectations; coarser cells and bands |
| Lost demand not recorded | Step 1 must record refusals and notify-me |

## Open questions
- May an availability check before sign-in add **one anonymous count to its cell**, with no location, device or person stored? Decision 0011 #9 says nothing is saved, so the answer is no unless the owner says otherwise.
