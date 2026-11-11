# Owner notes, 2026-11-11: hybrid grid, pre-mapping and the sweet spot

Input from the owner, pasted into the geo session on 2026-11-11 (Issue #162). Kept verbatim below the line, except that each note's closing "would you like..." question is left out, and so is Note 3's flow diagram, which repeats the one in `90-Second Snack Delivery System backend system and logic.md`. Do not edit; add a new dated file if it changes.

The owner also pasted these, which are already in this folder unchanged: `90-Second Snack Delivery System.md`, `90-Second Snack Delivery System backend system and logic.md` and `Core Technical Architecture & Algorithms.md`. A fourth file, `core_technical_architecture_algorithms.md`, is an earlier, shorter draft of the last one with nothing new, so it is not copied.

How these notes are used: `docs/engineering/geo/README.md`. The flow in force is still decision 0011; these notes give algorithms only.

---

## Note 1: Hybrid of H3 and circles

**Yes — you absolutely can (and often should) use a hybrid approach.**

In fact, for a tight 1-minute experimental system, a **hybrid of H3 + circles** (or H3 + isochrones) is one of the strongest practical designs. Pure geometry or pure irregular polygons both have weaknesses at this scale; combining them gives you the best of both worlds.

### Why Hybrid Works Extremely Well Here

| Component | Role in the Hybrid | Benefit |
|---------|---------------------|--------|
| **H3 (fine resolution)** | Primary spatial index + cell assignment | Extremely fast lookup, uniform neighbours, hierarchical, easy to scale and visualise |
| **Circles (or small fixed radius)** | Hard distance filter / serviceability rule | Simple, predictable, easy to reason about, guarantees maximum straight-line distance |
| **Optional: Isochrones or road network** | Final travel-time validation | Makes it more realistic when needed |

### How a Practical Hybrid Looks

1. **Underlying grid**: Use high-resolution H3 (typically resolution 11–13 for 1-minute scale).
   Every point on the map belongs to exactly one H3 cell.

2. **Store / micro-hub ownership**:
   Each H3 cell is assigned to the nearest micro-hub (or the hub that can theoretically reach it).

3. **Circle (or radius) layer**:
   Around every micro-hub, define a hard circle (e.g. 250–400 metres).
   A cell is only serviceable by that hub if **both**:
   - The cell is assigned to the hub, **and**
   - The centre (or any point) of the cell lies inside the circle.

4. **Optional final check**:
   When an order comes, you can still run a quick estimated travel-time check (using average speed or a simple road graph) to reject edge cases.

### Advantages of This Hybrid for Your 1-Minute Experiment

- **Speed**: H3 cell lookup is extremely fast (O(1) after indexing).
- **Simplicity**: Circles are trivial to implement and debug.
- **Control**: You can easily tighten or loosen the circle radius per hub without redrawing complex polygons.
- **Scalability**: Adding a new micro-hub only requires re-assigning nearby H3 cells + drawing a new circle.
- **Flexibility**: You can later replace the circle with a real isochrone polygon without changing the H3 indexing layer.
- **Visualisation & debugging**: You can plot both the H3 cells and the circles clearly.

### Possible Downsides (and how to handle them)

| Issue | How serious | Mitigation |
|------|-------------|----------|
| Circles ignore real roads | Medium at 1-min scale | Add a secondary travel-time filter, or switch circles to small isochrones later |
| Overlapping circles | Low | Use Voronoi-style assignment or priority rules (nearest hub wins) |
| Gaps between circles | Medium | Slightly increase radius or densify hubs |
| Extra computational layer | Low | Circles are very cheap to check |

### Recommended Hybrid for Your Experiment

**Best starting combination**:
- Fine H3 grid (primary index)
- Hard circle of fixed small radius around each micro-hub
- Nearest-hub assignment of cells
- Optional simple travel-time buffer

This is clean, fast, and easy to iterate on. Once the system works, you can upgrade the circle layer to real travel-time isochrones while keeping the H3 foundation.

---

**Bottom line**:
There is no reason you *cannot* use a hybrid. Hybrid approaches are often superior for experimental tight-time systems precisely because pure methods have limitations. H3 + circles is a very solid and practical combination for what you are trying to build.

---

## Note 2: Pre-mapping strategy

**Yes — you have the right core idea, and it is the correct way to build this.**

### Pre-mapping Strategy (Recommended)

We **pre-map the entire city once** (and refresh periodically) instead of calculating everything live every second.

**How it works:**

1. **Divide the whole city into H3 hexagonal cells** (resolution 9 or 10 is usually good for city streets).
2. **Pre-compute for every possible Gate / delivery pin:**
   - The set of H3 cells that form its **Sweet Spot** (cells from which a scooter can reach the Gate in 80–85 seconds).
   - Larger rings / zones around it (500 m, 1 km, 2 km…).
3. **Pre-compute travel times** between important cells (or use Contraction Hierarchies so live queries stay extremely fast).
4. **Store everything** in Redis / PostGIS so lookups are O(1) or very cheap.

**At runtime the system only does this:**

- Convert rider's current GPS → H3 cell (very fast).
- Check: "Is this cell inside the pre-computed Sweet Spot of the order's Gate?"
- If yes → assign and start timer.
- If no → check the next pre-computed ring, and so on.

This is exactly why H3 + precomputed isochrones / RPHAST / Contraction Hierarchies exist. Live calculation of full isochrones on every order would be too slow and expensive.

**Result:**
Most of the heavy work is done offline or in background jobs. Live path is just cell membership + simple comparison.

### What We Have Covered Well

- Main happy path (Ravi → payment → looking for riders → assignment → 90s timer → 100 m arrival → handover)
- Progressive search when Sweet Spot is empty
- Automatic assignment (no accept/reject)
- Manual placement of riders into Sweet Spots
- Same-grid / multi-order preference logic
- Hub refill when boxes are empty
- Customer & Rider screen states
- Timer start/stop rules

### What Is Still Left / Important to Cover

1. **Edge Cases** (most important next step)
   - No rider found even after full progressive search
   - Rider battery dies or goes offline after assignment
   - Rider is assigned but then leaves the Sweet Spot before moving
   - Multiple riders enter the Sweet Spot at almost the same time
   - Customer cancels while in MATCHING or after ASSIGNED
   - GPS jump / wrong location (rider appears inside Sweet Spot due to bad GPS)
   - Order comes from a brand-new Gate that has no pre-computed Sweet Spot yet
   - Scooter box becomes empty *during* an active delivery
   - Two orders assigned to the same rider in quick succession (sequence optimization)
   - Network failure on rider app during the critical assignment moment
   - Customer moves the Gate pin after placing the order

2. **Final Algorithm Choices** (we need to lock these)
   - Exact method for building Sweet Spots (Dijkstra with cutoff + H3)
   - Matching algorithm (Hungarian vs simple nearest + cost matrix vs bottleneck)
   - How to score "already assigned nearby rider" vs "new rider"
   - How often to refresh pre-computed Sweet Spots (traffic changes)

3. **Operational Pieces**
   - How operations team manually pins riders into Sweet Spots (tooling)
   - Rules for when a scooter is forced to go to Hub
   - What the customer sees and how long we wait in each progressive ring

4. **Smaller but real gaps**
   - Exact buffer time after entering 100 m zone
   - What happens to the 90 s timer display if progressive search takes 20–40 seconds
   - Inventory lock timing (when exactly we reserve the 2 donuts)

---

## Note 3: The sweet spot algorithm

### What is a Sweet Spot?
A **Sweet Spot** is the set of all locations (H3 cells) from which a scooter can reach a specific Gate in **80–85 seconds**.

Only riders currently inside this set are allowed to be assigned immediately.

### How the Sweet Spot is Created (Offline)

This is **not** calculated when a customer places an order.
It is pre-computed in the background.

1. Take one Gate location (latitude, longitude).
2. Run **Dijkstra algorithm** on the road network with a **time cutoff of 80–85 seconds**.
   - Starting point = Gate
   - Cost = travel time
   - Stop exploring when time exceeds 85 seconds
3. Collect all the road points / nodes that can reach the Gate within 85 seconds.
4. Convert every one of those points into **H3 Resolution 10** cell IDs.
5. Remove duplicates → you get a clean **set of H3 cell IDs**.
6. Store this set in Redis / database as the Sweet Spot of that Gate.

**Input:** Gate latitude & longitude; maximum time (80 or 85 seconds); road network (from OpenStreetMap via OSRM/Valhalla).

**Output:** A list/set of H3 cell IDs. Example: `["8a2a1072b59ffff", "8a2a1072b5bffff", "8a2a1072b43ffff", ...]`

**When it runs:** Background job (nightly or when new Gates are added).

### How it is Used at Runtime (Very Fast)

1. Rider sends current GPS location.
2. Convert GPS → H3 cell ID (takes milliseconds).
3. Check: "Does this H3 cell exist in the pre-stored Sweet Spot set of the order's Gate?"
4. If **Yes** → Rider is eligible for immediate assignment.
5. If **No** → Move to progressive search (next rings).

### Quick Summary

| Part | When | Method | Speed |
|---|---|---|---|
| Create Sweet Spot | Offline | Dijkstra + convert to H3 cells | Slow (background) |
| Check if rider in Sweet Spot | Live | H3 cell membership | Extremely fast |
| Progressive Search | Live | Pre-computed larger H3 sets | Extremely fast |
| Assign rider | Live | Filtered + Ranked | Fast |
| Start 90s timer | On assignment | — | Instant |
| Stop timer | At 100m zone | H3 or distance check | Instant |
