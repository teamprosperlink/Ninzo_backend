# 0016: Pilot operating model: check-in, spot assignment, shift windows, zone size, far riders

- Status: Accepted
- Date: 2026-11-14
- Deciders: owner (answers to the geo session's questions, #261)

## Context
Three answers on who places riders disagreed:
- 2026-11-11 (#154): "the system assigns the spot".
- 2026-11-13 (#234, order flow v2): "ops place riders manually from the console, and the system suggests".
- 2026-11-14: "the microhub assigns spots by who came".

The code did none of these: the rider app sends the spot at `startShift`. Shifts were open-ended, and nothing recorded a rider as present at the hub.

The fleet simulator also gave two findings (#255, `docs/engineering/geo/w4-rider-placement.md`):
- 6 riders reaching within 80 s cover only 14–33% of a 2×3 km test zone.
- Starting riders at the placement plan's spots beats moving them around mid-shift.

## Options considered
1. **Rider chooses the spot (today's code).** Simple, but coverage depends on who picks what, and nobody can see or enforce the plan.
2. **Ops place riders manually, and the system suggests (2026-11-13).** Flexible, but slow at every shift start, and a person must act for each rider.
3. **The system assigns at check-in, and staff can override (chosen).** The plan is followed by default, people keep control, and the check-in is a real "present" event.

## Decision
1. **Spot assignment:** the system assigns each rider a spot at check-in, from the current placement plan.
   - Hub staff or ops can override, with a reason logged.
   - The rider app never chooses the spot.
   - Plans for each number of riders are nested, so each check-in takes the next spot of the plan. A late rider takes the next open spot without moving anyone. A rider who leaves is covered by moving the holder of the last spot.
2. **Check-in:** the rider taps check-in, and the hub geofence is verified.
   - Hub staff confirm in the partner app for the first 2–3 weeks.
   - After that, tap plus verified location only.
   - Staff confirmation stays available as an override.
3. **No-show:** at 15 min after the scheduled start, the spot is released and a no-show is recorded. The count is visible to ops; there is no pay penalty in the pilot.
4. **Late join:** allowed until 60 min after the start, into open spots only.
5. **Shift windows:** lunch 11:00–15:00 and dinner 18:30–23:00, IST. These replace open-ended shifts.
   - No new orders after a window's end; live orders finish.
   - The shift ends automatically at the window's end + 30 min.
   - Night close: the scooter goes back to the hub, unsold stock is checked in (the restock flow), and cash-up is done. Orders are prepaid, so cash-up is a reconciliation of stock and handovers, not of cash.
6. **Pilot zone:** about 1 km², preferably inside Koramangala or Indiranagar, so the simulator and what ops know carry over. The geo session picks the polygon and rider count by simulation.
7. **Far riders:**
   - **Matching up to 75 s:** the 90 s timer as today.
   - **Search widened to about 2 min of riding:** the customer sees "about X min", with no hard 90 s timer.
   - **Beyond that:** "no rider nearby", and the order is refused.
8. **OSRM** runs as a Railway service (1–2 GB of memory accepted). The monthly cost goes to the owner after the Bengaluru data is measured.
9. **Cold start:** ops enter known hotspots (colleges, offices, PGs, markets) and surveyed legal waiting spots in the console. Plans use the seeded hotspots until real demand exists.
10. **Moves:**
    - The simulator decides whether a rider with a move in play takes orders. The finished #255 run settles it.
    - Thrashing is fixed by a gain threshold, and by planning on the full shift roster rather than only the free riders. It is not fixed by blocking offers.

## Consequences
- **New backend work, in this order:**
  1. check-in at the hub, with spot assignment from the plan, the staff override, no-show and late join
  2. shift windows, with the automatic end and the night close
  3. gain-threshold relocation, tested in the simulator first
- **Contract changes:**
  - The rider's `startShift` no longer takes a spot from the app.
  - New check-in calls, rider and partner (staff confirmation, override).
  - A customer stage for "about X min" with no timer.
  - Each goes through the contract, the mock and the Shared Doc first.
- **The console session:** #260's "manual placement" becomes the override with a reason. #234's placement line is superseded.
- **Superseded:** the spot answers of 2026-11-11 and 2026-11-13, and open-ended shifts (2026-11-11).
- **Owner and ops:** hotspot and waiting-spot survey; OSRM cost approval.

## Links
#261 (this decision), #255 (simulator), #234 (order flow v2), #154, #260 (console), `docs/engineering/geo/w4-rider-placement.md`.
