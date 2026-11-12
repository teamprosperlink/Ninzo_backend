# W3: Live rider data

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Know, at any moment, where each rider is, how fresh that is, and which cell and tier that puts them in for any open order, cheaply enough to read every second. Keep the full history for learning.

## Today
- Pings arrive by `POST` and in the offline batch, with the same rules.
- Each ping must carry `accuracy_m`, and pings worse than 100 m are refused (`internal/orders/tracking.go`).
- Cleaning already in place: two pings in a row for zone and perimeter events, and jumps over 40 m/s dropped. Pings over 30 s late do not move the timer but are kept for late-arrival review.
- Every ping is stored in Postgres (`location_pings`), kept in full through testing (owner, 2026-11-05).
- Live streams read the saved state about once a second and send the rider's position every 3 s.
- No cell on pings, no separate latest-position store, and Redis is not used by any code yet.

## Options

### Where the latest position lives
| Option | For | Against | Verdict |
|---|---|---|---|
| Read the newest ping from `location_pings` | Nothing new; an index lookup (`location_pings_rider_idx`) | The newest ping may be one that was not applied (late or rejected), so each reader must filter | Today; fine for now |
| **A `rider_positions` row per rider in Postgres, updated on each ping** | Simple, no new service, exact | Many updates at scale | **Pilot** (8 riders) |
| Redis GEO plus a hash per rider | Fast, nearby search built in, pub/sub for streams | A new service on staging and production | **City scale** |
| Tile38 | Geofence events built in | Another service to run | Multi-city only |

G17 in the README follows this: Postgres for the pilot, Redis when the number of streams or riders needs it.

### Cleaning GPS
| Method | Use |
|---|---|
| Two pings in a row (hysteresis) | In place |
| Impossible-jump filter | In place (40 m/s) |
| Accuracy filter | In place: `accuracy_m` is required, and pings worse than 100 m are refused. Tighten the limit for zone events if the road test shows the need |
| Kalman smoothing | Later, for display and speed |
| Map matching (OSRM `/match`, a hidden Markov model) | Later: for learning travel times (`w6`), not for the timer |

## Steps

| # | Step | What we need | How | Done when |
|---|---|---|---|---|
| 1 | Cell on each ping | `internal/hexgrid` (`w2`, #193) | Add the grid cell (`blr76`, #193) to each stored ping (new column, migration renumbered before the PR) | Each new ping has its cell; old pings are filled by a one-off job |
| 2 | Latest position | `rider_positions` (rider, point, cell, speed, heading, accuracy, ping time, received time) | Updated in the same transaction as the ping | Reads take one row; tests cover out-of-order and late pings |
| 3 | Tier lookup | `w2` step 4 | Given a rider and a gate, answer the tier from the rider's cell | Used by `w5`, with a test |
| 4 | Read function for the console live map | Agreement with the console session (#150) | A Go function in a package this session owns; the console session builds its endpoint on it. Any change on their side goes through an Issue | #150 can show riders without reading pings itself |
| 5 | Handover stop point | The rider's position at handover | Store it with the order | Feeds gate pin fixes (`w9`) and the learned approach point (`w2`) |
| 6 | Ping rate hint (optional) | Agreement with the app developers through the shared Doc | Faster pings near and inside the zone, slower while waiting, to save battery | Only if the developers want it: it is a contract change |

## Path to scale
Redis GEO for positions, and push to streams (Redis pub/sub or Postgres `LISTEN/NOTIFY`) instead of polling; ping history moved to cheaper storage after the testing period; Tile38 or a stream platform only at multi-city scale.

## Risks
| Risk | Mitigation |
|---|---|
| Urban canyons give 10 to 30 m errors | The perimeter is 100 m; two pings in a row; the accuracy filter |
| A rider's phone sends late or batched pings | The existing 30 s rule; the offline batch rules |
| Write load from full ping history | Fine at pilot size; retention is decided after testing (owner, 2026-11-05) |

## Open questions
- None of its own. G17 in the README covers where latest positions live.
