# Detailed End-to-End Flow

## 90-Second Snack Delivery System

&nbsp;

### 1\. Actors

| Name | Role |
| :---- | :---- |
| Ravi | Customer |
| David | Delivery rider (scooter with small fridge \+ oven) |
| System | Backend \+ matching engine |
| Hub | Place where scooters refill empty boxes |

&nbsp;

### 2\. Glossary

| Term | Meaning |
| :---- | :---- |
| **Gate** | Exact delivery point chosen by the customer |
| **Sweet Spot** | Area from which a scooter can reach the Gate in 80–85 seconds |
| **Ring / Zone** | Larger area (1–5 km) where riders wait |
| **Arrival Zone** | 100-metre radius around the Gate |
| **Progressive Search** | Expanding search radius when Sweet Spot is empty (500 m → 1 km → 2 km…) |
| **Hub** | Fixed location that prepares and stores snacks/beverages. Scooters go here when their box is empty |

---

### 3\. Order States

`CREATED` → `PAID` → `MATCHING` → `EXPANDING_SEARCH` (optional) → `ASSIGNED` → `EN_ROUTE` → `ARRIVED` → `HANDOVER` → `COMPLETED.`

---

### 4\. Detailed Flow

#### Step 1: Customer Places Order

- Ravi opens the app, adds 2 donuts to cart, taps “Buy Now”, and completes payment.  
- Order state becomes **PAID**.

#### Step 2: Looking for Riders

- Ravi sees the screen: **“Looking for riders”**.  
- Order state \= **MATCHING**.  
- System starts searching for a suitable scooter.

#### Step 3: Matching Logic

**3A. Happy Path – Rider already in Sweet Spot**

- System finds David already inside the Sweet Spot of Ravi’s Gate.  
- System automatically assigns David (no Accept/Reject button).  
- Order state → **ASSIGNED**.  
- 90-second timer starts for Ravi.  
- Ravi’s screen changes to: **“Driver accepted the order – Arriving to your location”** \+ live map \+ countdown.  
- David’s app receives the order and starts navigation.

**3B. Fallback – No Rider in Sweet Spot (Progressive Search)**

- System finds no scooter inside the Sweet Spot.  
- Ravi is shown: **“No rider available in this location right now. We are expanding the search.”**  
- Order state → **EXPANDING\_SEARCH**.  
- System expands search radius step-by-step:  
  1. 500 metres  
  2. 1 km  
  3. 2 km  
  4. … up to the edge of the Ring  
- As soon as a suitable scooter is found → assign it and start the 90-second timer (same as happy path).  
- If still no rider after maximum radius → system can reject or keep waiting (ops decision).

**3C. Multiple Orders from Same / Nearby Grid**

- If David is already delivering another order nearby, the system compares:  
  - Option A: David finishes current order then goes to Ravi.  
  - Option B: Bring a completely new rider from outside into the Sweet Spot.  
- System chooses the faster overall option before assigning.

#### Step 4: Rider Moves

- David rides toward Ravi’s Gate.  
- Order state \= **EN\_ROUTE**.

#### Step 5: Arrival

- When David enters the **100-metre Arrival Zone**:  
  - Timer stops for both Ravi and David.  
  - Ravi’s screen shows: **“Arrived at your location”** or **“Near your gate”**.  
  - David gets a short buffer time to find the exact door/entrance.  
  - Ravi’s map continues with intentional slight lag so the arrival feels faster.  
- Order state → **ARRIVED**.

#### Step 6: Handover & Completion

- David hands over the items and completes OTP/confirmation.  
- Order state → **HANDOVER** → **COMPLETED**.

#### Step 7: Refilling Empty Boxes

- When David’s box becomes empty or low, he is automatically directed to the **nearest Hub**.  
- At the Hub the box is refilled with fresh stock.  
- After refill, David returns to a Sweet Spot or Ring position and becomes available again.

---

### 5\. Flow Diagram

```
Ravi pays successfully
        │
        ▼
“Looking for riders” (MATCHING)
        │
        ▼
Is any scooter inside Sweet Spot?
        │
   ┌────┴────┐
  Yes       No
   │         │
   │         ▼
   │    Show message:
   │    “No rider available…
   │    Expanding search”
   │         │
   │         ▼
   │    Progressive Search
   │    500m → 1km → 2km…
   │         │
   │    Found? ──No──→ Reject / Long wait
   │         │
   │        Yes
   │         │
   └─────────┴──────────────┐
                            ▼
                  Assign rider (David)
                  90s Timer STARTS
                  Ravi: “Driver accepted – Arriving…”
                            │
                            ▼
                      David moves (EN_ROUTE)
                            │
                            ▼
               David enters 100m Arrival Zone
                            │
                            ▼
                  Timer STOPS
                  Ravi: “Arrived / Near gate”
                  David gets short buffer
                            │
                            ▼
                      Handover → Completed

(When box empty → David goes to nearest Hub for refill)
```

---

### 6\. Customer Screens (Ravi)

| Situation | Screen Shown |
| :---- | :---- |
| After payment | Looking for riders |
| Rider found in Sweet Spot | Driver accepted – Arriving to your location \+ 90s timer |
| No one in Sweet Spot | No rider available… Expanding search |
| Rider enters 100m zone | Arrived at your location / Near your gate (timer stopped) |
| After handover | Order completed |

---

### 7\. Rider Screens (David)

| Situation | Screen Shown |
| :---- | :---- |
| Waiting in Ring / Sweet Spot | Ready – waiting for orders |
| Order assigned | New order appears automatically \+ navigation starts |
| Moving | Turn-by-turn navigation |
| Entered 100m zone | You have arrived – find the gate (timer stopped) |
| Box empty | Go to nearest Hub for refill |

---

### 8\. Key Rules Summary

- Assignment is fully automatic (no Accept/Reject).  
- Timer starts only when a rider is assigned.  
- Timer stops exactly when the rider enters the 100m Arrival Zone.  
- Riders are manually placed into good Sweet Spots by the operations team.  
- When Sweet Spot is empty, search expands progressively and the customer is informed.  
- For nearby multiple orders, system compares continuing with the current rider vs bringing a new one.  
- Empty boxes are refilled only at Hubs.

\*\*Edge Cases – Prioritized List\*\*

&nbsp;

Here are the most important and realistic edge cases for your system, ranked by how often they can happen and how much damage they can cause if handled poorly.

&nbsp;

We will go through them one by one. For each case I will show:

&nbsp;

\- What happens

\- Suggested default behaviour

\- What Ravi (customer) sees

\- What David (rider) sees

&nbsp;

You can then confirm, change, or add rules.

&nbsp;

\---

&nbsp;

\#\#\# 1\. No rider found even after full Progressive Search

\*\*Situation:\*\* Sweet Spot is empty \+ all expanded rings (500m → 1km → 2km…) also have no suitable rider.

&nbsp;

\*\*Suggested behaviour:\*\*

\- After maximum radius is reached → mark order as \`UNFULFILLABLE\`

\- Automatically cancel or move to long-wait queue (ops decision)

\- Refund or keep payment on hold

&nbsp;

\*\*Ravi sees:\*\* “Sorry, no riders available in your area right now. Please try again in a few minutes.”

&nbsp;

\---

&nbsp;

\#\#\# 2\. Rider goes offline / battery dies / app crashes after assignment

\*\*Situation:\*\* David was assigned, timer started, then his phone dies or he goes offline.

&nbsp;

\*\*Suggested behaviour:\*\*

\- Detect missing location updates (e.g. no ping for 15–20 seconds)

\- Automatically cancel the assignment

\- Put the order back into \`MATCHING\` / Progressive Search

\- Notify Ravi that a new rider is being found

&nbsp;

\*\*Ravi sees:\*\* “Rider disconnected. Finding another rider for you…” \+ timer pauses or resets carefully

&nbsp;

\---

&nbsp;

\#\#\# 3\. Rider leaves the Sweet Spot after assignment (but before moving)

\*\*Situation:\*\* David was inside Sweet Spot → got assigned → then moves out of the zone (or GPS jumps).

&nbsp;

\*\*Suggested behaviour:\*\*

\- Soft warning first

\- If he stays outside for more than X seconds → cancel assignment and re-match

&nbsp;

\---

&nbsp;

\#\#\# 4\. Multiple riders enter the Sweet Spot at almost the same time

\*\*Situation:\*\* 3–4 scooters are eligible at the exact same moment.

&nbsp;

\*\*Suggested behaviour:\*\*

\- Use the ranking algorithm (travel time \+ battery \+ load)

\- Assign only one

\- Others remain available for next orders

&nbsp;

\---

&nbsp;

\#\#\# 5\. Customer cancels

\*\*Two sub-cases:\*\*

&nbsp;

\*\*A. During MATCHING / EXPANDING\_SEARCH\*\*&nbsp;&nbsp;

→ Easy cancel. Just stop searching.

&nbsp;

\*\*B. After ASSIGNED (timer already running)\*\*&nbsp;&nbsp;

→ Need clear policy:

\- Free cancel within first 10–15 seconds?

\- Or charge a small fee?

\- Release the stock reservation immediately

&nbsp;

\---

&nbsp;

\#\#\# 6\. Bad GPS / Fake entry into Sweet Spot

\*\*Situation:\*\* Rider’s GPS jumps and suddenly shows inside Sweet Spot even though he is far.

&nbsp;

\*\*Suggested behaviour:\*\*

\- Require 2 consecutive good location pings inside the Sweet Spot (hysteresis)

\- Or check recent speed \+ heading

\- Reject sudden impossible jumps

&nbsp;

\---

&nbsp;

\#\#\# 7\. Brand new Gate with no pre-computed Sweet Spot

\*\*Situation:\*\* Customer drops a pin in a completely new location that was never processed.

&nbsp;

\*\*Suggested behaviour:\*\*

\- Fallback: calculate a temporary Sweet Spot on the fly (slower)

\- Or reject with message “Delivery not available at this exact pin yet”

\- Queue it for background pre-computation

&nbsp;

\---

&nbsp;

\#\#\# 8\. Scooter box becomes empty during an active delivery

\*\*Situation:\*\* David is already on the way, but system detects stock issue (or he reports it).

&nbsp;

\*\*Suggested behaviour:\*\*

\- Cancel current order

\- Send David to nearest Hub

\- Re-assign a new rider to Ravi

&nbsp;

\---

&nbsp;

\#\#\# 9\. Two close orders assigned to the same rider (sequence problem)

\*\*Situation:\*\* David is delivering Order A. Order B comes from a nearby location.

&nbsp;

\*\*Suggested behaviour:\*\*

\- System already compares “finish A then do B” vs “new rider”

\- If continuing is better → add Order B to David’s queue

\- Show David the correct sequence

&nbsp;

\---

&nbsp;

\#\#\# 10\. Network failure on rider app at the moment of assignment

\*\*Situation:\*\* System assigns David, but the push/notification fails.

&nbsp;

\*\*Suggested behaviour:\*\*

\- Retry notification 2–3 times

\- If still no acknowledgement within 8–10 seconds → cancel and re-assign

&nbsp;

\---

&nbsp;

\*\*Which edge case do you want to decide first?\*\*

&nbsp;

I recommend we start with these three (they are the highest impact):

&nbsp;

1\. No rider found after full search&nbsp;&nbsp;

2\. Rider goes offline after assignment&nbsp;&nbsp;

3\. Customer cancels after timer has started&nbsp;&nbsp;

&nbsp;

Just say the number (or the name) and we will lock the exact behaviour.

&nbsp;