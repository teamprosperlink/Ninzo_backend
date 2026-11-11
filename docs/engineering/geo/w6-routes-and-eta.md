# W6: Routes and ETA

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Give the rider a real route to the gate, give the customer an honest wait before the timer, and learn real travel times from our own trips, so the zones and budgets get better with every order.

## Today
- `getRoute` (#156): a straight line from the last position to the gate, `eta_source: placeholder`.
- The customer's order shows `eta_s` and `eta_source`, a placeholder for now. The longer-ETA offer at no scooter (#94) is accepted but never offered, because no time is measured yet.

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | Real route | `w1` | OSRM `/route` from the rider's latest position to the gate's approach point; the line and the time; `eta_source: measured`; the fallback stays `placeholder` | Contract tests pass unchanged; a test proves the fallback |
| 2 | Customer ETA before the timer | `w1`, `w3` | Road time from the rider to the timer zone, plus the expected packing time left | The ETA is `measured` when the rider's position is fresh |
| 3 | Longer-ETA offer | Steps 1 and 2 | Offer it when the best capable scooter is beyond the sweet spot but within `max_wait_before_timer_s` | The existing #94 flow gets real numbers |
| 4 | Record every timed leg | `w3` | For each order: zone entry, perimeter crossing, the path between, the hour | Stored per order, viewable per gate |
| 5 | Learned times | Step 4 | Per gate and hour band: p50 and p90 of the timed leg. Blend with OSRM's guess, giving more weight to our data as trips add up (for example n / (n + 20)) | A report per gate |
| 6 | Zone feedback | Step 5, `w2` | A gate whose p90 timed leg is over the timer minus the margin gets a **proposed** smaller zone, through the console's approval flow (decision 0014) | Proposals appear for review; nothing changes without approval |
| 7 | Measured speeds into OSRM | Map-matched trips (`w3`) | Segment speeds by hour band into `osrm-customize` (MLD) | City scale |

## Options

| Choice | Options | Recommendation |
|---|---|---|
| Rider navigation | Our route drawn in the app / hand off to the Google Maps app for turn-by-turn / both | **Both**: our route for the timed leg and the ETA; a hand-off button for turn-by-turn (an app decision, told through the shared Doc) |
| ETA model | OSRM only / OSRM blended with our trips / ML | **Blend**, then ML (gradient-boosted trees) when data allows |
| Zone changes from data | Automatic / proposed and approved | **Proposed and approved** |

## Licences
- A route built from OpenStreetMap data must credit "© OpenStreetMap contributors" where it is shown.
- Drawing our own line on the Google map in the apps is an overlay of our data. Check Google's current terms before release; never use Google's route data on another map (`research.md`, section 10).

## Path to scale
ETA models per city; traffic-aware speeds by hour; ETA at checkout from the same model.

## Risks
| Risk | Mitigation |
|---|---|
| Too few trips per gate to learn from | Learn per cell band or per area first, then per gate |
| A promised ETA that is wrong | The promise is only the timed leg; the wait before it is shown as an estimate |

## Open questions
- `getRoute` and the order ETA live in `internal/orders` (`route.go`, `orders.go`). Confirm with the owner that this session owns those two files for this work.
