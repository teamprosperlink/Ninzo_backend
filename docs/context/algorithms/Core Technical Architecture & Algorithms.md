# System Core Architecture & Algorithms

**Let’s lock the core, most controllable, and highest-impact parts first.**

## 1\. H3 Resolution Recommendation

For an **80–85 second** (≈ 1 to 1.5 minute) sweet-spot scale, here is the practical recommendation:

| Resolution | Approx edge length | Best use in our system | Recommendation |
| :---- | :---- | :---- | :---- |
| **10** | \~65 m | Main operational grid for Sweet Spots \+ Rings | **Primary choice** |
| **11** | \~24 m | Fine-grained Arrival Zone (100 m) and precise positioning | Secondary (use only where needed) |
| **12** | \~9 m | Too fine for city-wide pre-computation | Avoid for now |

**Final recommendation:**

* Use **H3 Resolution 10** as the main grid for the entire city (Sweet Spots, Rings, progressive search layers).  
* Optionally use **Resolution 11** only for the final 100-metre Arrival Zone check if you want higher precision.  
* Resolution 12 creates too many cells and makes pre-computation and storage heavier without enough benefit at a 1-minute scale.

This keeps lookups extremely fast and storage reasonable.

---

## 2\. Main Core Algorithms to Lock Now

These are the algorithms that cover the happy path \+ the most common controllable situations:

| Decision | Recommended Algorithm / Method | Why |
| :---- | :---- | :---- |
| **Building Sweet Spots** | Dijkstra with time cutoff (80–85 s) → convert reachable nodes to H3 Res 10 cells | Accurate and controllable. Run offline / periodically. |
| **Fast “Is rider in Sweet Spot?”** | H3 cell membership lookup (pre-computed set) | O(1). This is the heart of real-time performance. |
| **Progressive Search rings** | Pre-computed concentric H3 cell sets (500 m, 1 km, 2 km…) | Extremely fast. No live calculation needed. |
| **Rider Assignment** (most complex part) | **Filtered \+ Ranked Nearest** (or lightweight Hungarian) | See detailed recommendation below. |
| **Multi-order / same-grid preference** | Simple cost comparison: (remaining time of current order \+ travel to new order) vs (new rider full travel time) | Easy to implement and control. |
| **Travel time source** | Pre-computed edge times \+ Contraction Hierarchies (OSRM/Valhalla) | Keeps live ETA fast and stable. |

---

## 3\. Rider Assignment – The Real Complex Task

This is the hardest and most important runtime decision.

**Recommended approach (simple, controllable, and strong enough):**

1. **Hard filter first:** Only consider riders whose current H3 cell is inside the current search layer (Sweet Spot → 500 m → 1 km → …).  
2. **Score the remaining candidates** with a clear cost formula:

```
Cost = Travel time to Gate
     + Battery penalty
     + Current load penalty
     + Existing order penalty (if already assigned)
```

3. **Pick the lowest cost rider.**

**Two implementation options:**

* **Option A (Recommended for start):** Simple sorted ranking (very fast, easy to debug and control).  
* **Option B:** Hungarian algorithm on the small candidate set (optimal when you have 5–15 candidates).

*Start with Option A. Move to Hungarian only if you later see clear quality problems. This keeps the system predictable and easy for the operations team to understand.*

---

## 4\. What We Are Locking Right Now

* **H3 Resolution 10** as primary grid  
* **Pre-computed Sweet Spots** \+ progressive rings  
* **Dijkstra cutoff** for Sweet Spot generation  
* **Filtered \+ Ranked assignment** (with clear cost formula)  
* **Simple comparison** for multi-order cases  
* **Timer starts only** on successful assignment  
* **Timer stops** at 100 m Arrival Zone

These cover the large majority of real orders and are fully controllable.

---

## Next Steps: Edge Cases

We can now move to the most important **edge cases** one by one, using the locked core above.

*(Please list the top 8–10 most important edge cases in priority order so we can decide the behaviour for each.)*

&nbsp;

&nbsp;

\*\*Clear & Practical Explanation\*\*

&nbsp;

\#\#\# 1\. How H3 Works in Our System

&nbsp;

H3 is \*\*not\*\* calculated live for the whole city on every order.&nbsp;&nbsp;

We \*\*pre-compute\*\* almost everything.

&nbsp;

\#\#\#\# Step-by-step process:

&nbsp;

1\. \*\*Take the city map\*\* (OpenStreetMap data).

2\. \*\*Divide the entire city into hexagonal cells\*\* using H3 at Resolution 10 (our main choice).

3\. \*\*For every possible Gate / delivery location\*\*:

&nbsp;&nbsp;&nbsp;\- Run a routing calculation (Dijkstra with 80–85 second cutoff).

&nbsp;&nbsp;&nbsp;\- Collect all the road points that can reach that Gate within 80–85 seconds.

&nbsp;&nbsp;&nbsp;\- Convert those points into H3 cells.

&nbsp;&nbsp;&nbsp;\- Store the list of H3 cell IDs as the \*\*Sweet Spot\*\* of that Gate.

4\. Also pre-compute larger rings (500 m, 1 km, 2 km…) the same way.

5\. Store everything in Redis or PostGIS.

&nbsp;

\*\*Result at runtime:\*\*

\- Rider sends GPS → convert to one H3 cell ID (very fast).

\- Check if that cell ID exists in the pre-stored Sweet Spot list of the order → Yes/No.

&nbsp;

This is why it stays fast even with thousands of riders and orders.

&nbsp;

\---

&nbsp;

\#\#\# 2\. Input & Output of Each Core Algorithm

&nbsp;

Here is the exact Input → Output for every important piece:

&nbsp;

\---

&nbsp;

\#\#\#\# A. H3 Cell Conversion (Live – very frequent)

&nbsp;

\- \*\*Library\*\*: \`h3\` (Uber’s official library – available in Python, JavaScript, Go, etc.)

\- \*\*Input\*\*: Latitude \+ Longitude (e.g. 12.9716, 77.5946)

\- \*\*Output\*\*: One H3 cell ID (e.g. \`8a2a1072b59ffff\`)

\- \*\*When\*\*: Every time a rider sends GPS location

&nbsp;

\---

&nbsp;

\#\#\#\# B. Building Sweet Spot (Offline / Background job)

&nbsp;

\- \*\*Algorithm\*\*: Dijkstra with time cutoff

\- \*\*Library\*\*: OSRM or Valhalla

\- \*\*Input\*\*:

&nbsp;&nbsp;\- Gate location (lat, lon)

&nbsp;&nbsp;\- Maximum time (80 or 85 seconds)

&nbsp;&nbsp;\- Road graph

\- \*\*Output\*\*: List of H3 cell IDs that form the Sweet Spot

\- \*\*When\*\*: Nightly or whenever new Gates are added / traffic patterns change significantly

&nbsp;

\---

&nbsp;

\#\#\#\# C. Progressive Rings (Offline)

&nbsp;

\- \*\*Method\*\*: Same as Sweet Spot, just with larger time cutoffs (or distance)

\- \*\*Input\*\*: Gate location \+ larger time/distance (500 m, 1 km, 2 km…)

\- \*\*Output\*\*: Separate lists of H3 cells for each ring

\- \*\*When\*\*: Same background job as Sweet Spot

&nbsp;

\---

&nbsp;

\#\#\#\# D. “Is rider inside Sweet Spot?” (Live)

&nbsp;

\- \*\*Method\*\*: Simple set membership

\- \*\*Input\*\*:

&nbsp;&nbsp;\- Rider’s current H3 cell ID

&nbsp;&nbsp;\- Pre-computed Sweet Spot cell list of the order

\- \*\*Output\*\*: \`True\` or \`False\`

\- \*\*When\*\*: Continuously while order is in MATCHING state

&nbsp;

\---

&nbsp;

\#\#\#\# E. Rider Assignment (Live – most important)

&nbsp;

\*\*Recommended method\*\*: Filtered \+ Ranked (simple & controllable)

&nbsp;

\- \*\*Input\*\*:

&nbsp;&nbsp;\- List of all currently available riders

&nbsp;&nbsp;\- Each rider’s current H3 cell

&nbsp;&nbsp;\- Order’s Gate location

&nbsp;&nbsp;\- Pre-computed Sweet Spot \+ rings of that Gate

&nbsp;&nbsp;\- Extra data: battery %, current load, whether rider already has an order

\- \*\*Process\*\*:

&nbsp;&nbsp;1\. Keep only riders whose cell is inside current search layer (Sweet Spot → 500m → 1km…)

&nbsp;&nbsp;2\. Calculate a cost for each remaining rider

&nbsp;&nbsp;3\. Sort by lowest cost

\- \*\*Output\*\*: The single best rider (or empty if none found)

\- \*\*When\*\*: Every 1–2 seconds while order is in MATCHING / EXPANDING\_SEARCH

&nbsp;

\*\*Cost formula example\*\*:

\`\`\`

Cost \= Travel time to Gate&nbsp;

&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;\+ (100 \- battery%) \* 0.5

&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;\+ number of items already carrying \* 10

&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;\+ penalty if already has an active order

\`\`\`

&nbsp;

\---

&nbsp;

\#\#\#\# F. Multi-order Preference (Live)

&nbsp;

\- \*\*Input\*\*:

&nbsp;&nbsp;\- Current rider’s remaining time to finish existing order

&nbsp;&nbsp;\- Distance/time from existing delivery location → new Gate

&nbsp;&nbsp;\- Time for a brand-new rider to reach the new Gate

\- \*\*Output\*\*: Decision — “Continue with current rider” or “Assign new rider”

\- \*\*When\*\*: When a new order arrives near an already busy rider

&nbsp;

\---

&nbsp;

\#\#\#\# G. Travel Time between two points (Live or cached)

&nbsp;

\- \*\*Library\*\*: OSRM / Valhalla (with Contraction Hierarchies)

\- \*\*Input\*\*: Origin lat/lon \+ Destination lat/lon

\- \*\*Output\*\*: Estimated travel time in seconds

\- \*\*When\*\*: During assignment scoring and multi-order comparison

&nbsp;

\---

&nbsp;

\#\#\# Summary – What Runs When

&nbsp;

\| Task                        \| When it runs          \| Speed      \|

\|----------------------------\|-----------------------\|------------\|

\| Divide city into H3 cells  \| Once (or rarely)      \| Slow (offline) \|

\| Build Sweet Spots & Rings  \| Background job        \| Slow (offline) \|

\| Convert GPS → H3 cell      \| Every rider location  \| Extremely fast \|

\| Check if in Sweet Spot     \| Continuous            \| Extremely fast \|

\| Assign best rider          \| Every 1–2 seconds     \| Fast       \|

\| Calculate live travel time \| During assignment     \| Fast (thanks to CH) \|

&nbsp;

\---

&nbsp;

\*\*Would you like me to also show a small realistic example\*\* (with fake coordinates) of how a Sweet Spot is created and how a rider is checked against it?&nbsp;&nbsp;

Or shall we now move to the edge cases list?

\*\*Yes — you are thinking correctly.\*\*

&nbsp;

Even if multiple scooters are inside the same Sweet Spot, we \*\*do not\*\* assign just because they are geographically close.&nbsp;&nbsp;

We still rank them by \*\*actual travel time\*\* on the real road network.

&nbsp;

\#\#\# Who does the filtering inside the Sweet Spot?

&nbsp;

1\. \*\*First filter (very fast)\*\*&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;Only riders whose current H3 cell is inside the pre-computed Sweet Spot are considered.

&nbsp;

2\. \*\*Second filter / ranking (the important one)\*\*&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;For every remaining rider, the system asks the routing engine:&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\*\*“What is the real travel time from this rider’s exact GPS location to the Gate right now?”\*\*

&nbsp;

3\. The rider with the \*\*lowest travel time\*\* (plus battery \+ load penalties) gets the order.

&nbsp;

\*\*Why this works well:\*\*

\- The routing engine (OSRM or Valhalla) already knows the real roads.

\- Bumps, steep slopes, sharp turns, one-ways, traffic signals, etc. naturally increase the travel time if the map data reflects them.

\- You can later add extra penalties (e.g. “avoid this steep road”) if you collect that data.

&nbsp;

So Sweet Spot \= “eligible”.&nbsp;&nbsp;

Actual travel time \= “who is truly fastest”.

&nbsp;

\---

&nbsp;

\#\#\# Recommended Libraries & How to Start

&nbsp;

\| Purpose                    \| Recommended Library / Tool          \| Why \|

\|---------------------------\|-------------------------------------\|-----\|

\| Hexagonal grid (H3)       \| \*\*Uber h3-py\*\*                      \| Official, very fast, easy \|

\| Building Sweet Spots      \| \*\*Valhalla\*\* (best) or OSRM         \| Valhalla has excellent built-in isochrone \|

\| Live travel time          \| Valhalla or OSRM                    \| Millisecond responses after preprocessing \|

\| Rider Assignment          \| Simple ranking \*\*or\*\* Google OR-Tools \| Start simple, upgrade later \|

\| Map matching (GPS snap)   \| Valhalla or OSRM                    \| Important for accurate position \|

&nbsp;

\---

&nbsp;

\#\#\# Exact Input → Output for the Critical Parts

&nbsp;

\*\*1. Create Sweet Spot (Offline)\*\*

\- \*\*Input\*\*: Gate lat/lon \+ max time (85 seconds)

\- \*\*Tool\*\*: Valhalla \`/isochrone\` or OSRM \+ custom script

\- \*\*Output\*\*: List of H3 Res 10 cell IDs

&nbsp;

\*\*2. Check if rider is eligible (Live)\*\*

\- \*\*Input\*\*: Rider current lat/lon

\- \*\*Tool\*\*: \`h3.latlng\_to\_cell(lat, lon, 10)\`

\- \*\*Output\*\*: One H3 cell ID → check if it exists in the Sweet Spot set

&nbsp;

\*\*3. Rank riders inside Sweet Spot (Live)\*\*

\- \*\*Input\*\*: List of eligible riders \+ Gate location

\- \*\*Tool\*\*: Routing engine table/matrix or individual route calls

\- \*\*Output\*\*: Sorted list by travel time (lowest first)

&nbsp;

\---

&nbsp;

\#\#\# Best Places to Download Code & Learn

&nbsp;

1\. \*\*Uber H3 (Python)\*\*&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- GitHub: \[https\://github.com/uber/h3-py\](https\://github.com/uber/h3-py)&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- Official docs \+ notebooks

&nbsp;

2\. \*\*Valhalla Isochrone\*\* (recommended for Sweet Spots)&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- Docs: \[https\://valhalla.github.io/valhalla/api/isochrone/\](https\://valhalla.github.io/valhalla/api/isochrone/)&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- Very clean API for time-based reachable areas

&nbsp;

3\. \*\*OSRM\*\*&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- GitHub: \[https\://github.com/Project-OSRM/osrm-backend\](https\://github.com/Project-OSRM/osrm-backend)&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- Extremely fast for live routing

&nbsp;

4\. \*\*Google OR-Tools (Assignment)\*\*&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;\- Official examples: \[https\://developers.google.com/optimization/assignment\](https\://developers.google.com/optimization/assignment)

&nbsp;

5\. \*\*Simple starting point\*\*&nbsp;&nbsp;

&nbsp;&nbsp;&nbsp;Many people first use:

&nbsp;&nbsp;&nbsp;\- \`h3-py\` for cells

&nbsp;&nbsp;&nbsp;\- Valhalla for isochrones \+ routing

&nbsp;&nbsp;&nbsp;\- Plain Python sorting for assignment (very easy to control)

&nbsp;

\---

&nbsp;

\#\#\# Recommended Starting Path (Practical)

&nbsp;

1\. Install \`h3\` and play with converting lat/lon → cell.

2\. Set up Valhalla (or use a public demo first) and generate one isochrone for a Gate.

3\. Convert that isochrone polygon into H3 cells.

4\. Write a small function that takes rider locations and returns the fastest one inside the Sweet Spot.

5\. Only after this works, add progressive rings and multi-order logic.

&nbsp;

Would you like me to give you a \*\*minimal working code structure\*\* (pseudocode \+ real library calls) for:

\- Creating a Sweet Spot

\- Checking riders

\- Ranking them by travel time

&nbsp;

Or shall we move to the edge cases list now?

&nbsp;