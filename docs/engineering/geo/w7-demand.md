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
| 1 | Demand counts | `w2` cells | A table of counts per cell, weekday and 30-minute band, for paid, refused and notify-me | Rebuilt from history; updated as orders come |
| 2 | Forecast v1 | Step 1 | A weighted average of the same weekday and band, recent weeks counting more | Its error is measured each week |
| 3 | Cold start | The owner or ops | Expected demand entered by hand per area and band until real data exists (offices, colleges, events) | Placement has something to plan with on day one |
| 4 | Forecast v2 | Months of data | Gradient-boosted trees with weather and holidays | Only if it beats v1 on the measured error |

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
