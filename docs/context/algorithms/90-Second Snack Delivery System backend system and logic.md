# 90-Second Snack Delivery System

## Clean, Self-Contained Specification

*(Anyone can understand this without prior context)*

# 1\. Actors

| Name | Role |
| :---- | :---- |
| Ravi | Customer |
| David | Delivery rider (scooter) |
| System | Backend \+ matching engine |

# 2\. Core Definitions

| Term | Meaning |
| :---- | :---- |
| **Gate** | Exact delivery point chosen by the customer (building entrance / specific location pin) |
| **Sweet Spot** | Geographic area around the Gate from which a scooter can reach the Gate in 80–85 seconds under normal conditions. Only scooters inside a Gate’s Sweet Spot are eligible for assignment. |
| **Ring** | Larger waiting zone (roughly 1–5 km radius / polygon) where riders stay ready with pre-loaded stock. Multiple Sweet Spots exist inside the Ring. |
| **Stocked Scooter** | Scooter carrying a small refrigerator \+ oven. Contains ready-to-deliver snacks, sweets, and both hot & cold beverages. No kitchen or picking step after order. |
| **Assignment** | Automatic matching of one order to one eligible scooter. Rider has **no Accept / Reject** button. |
| **90-Second Timer** | Customer-facing countdown that starts the moment a rider is assigned and stops when the rider enters the 100-metre Arrival Zone. |
| **Arrival Zone** | 100-metre radius around the Gate. Entering this zone stops the timer for both customer and rider. |
| **Looking for Riders** | Customer screen shown after successful payment until a rider is assigned. |

# 3\. High-Level Order States (System)

```
CREATED → PAID → MATCHING → ASSIGNED → EN_ROUTE → ARRIVED → HANDOVER → COMPLETED
```

| State | Meaning |
| :---- | :---- |
| CREATED | Order placed, payment not yet confirmed |
| PAID | Payment successful → enters MATCHING |
| MATCHING | System searching for a scooter already inside the Sweet Spot |
| ASSIGNED | Scooter found and locked → 90s timer starts |
| EN\_ROUTE | Rider is moving toward the Gate |
| ARRIVED | Rider entered 100 m Arrival Zone → timer stops |
| HANDOVER | Rider hands over items \+ OTP / confirmation |
| COMPLETED | Order finished |

# 4\. End-to-End Flow (Ravi & David Scenario)

Step 1 – Customer places order  
Ravi opens the app → adds 2 donuts to cart → taps “Buy Now” → completes payment successfully.  
Step 2 – Post-payment screen  
Ravi sees screen: **“Looking for riders”**. Order state \= MATCHING.

## Step 3 – System matching

* System looks only at scooters that are **already inside the Sweet Spot** of Ravi’s Gate.  
* Among those, it picks the nearest eligible scooter (David).  
* Assignment is automatic. David has no Accept/Reject option.

## Step 4 – Assignment happens

* Order state changes to ASSIGNED.  
* 90-second timer starts for Ravi.  
* Ravi’s screen changes to: **“Driver accepted the order – Arriving to your location”** \+ live map \+ countdown.  
* David’s app receives the order and starts navigation.

Step 5 – Rider moves  
David rides from his position inside the Sweet Spot toward Ravi’s Gate. Order state \= EN\_ROUTE.  
Step 6 – Arrival Zone entered  
When David’s location enters the 100-metre Arrival Zone around the Gate:

* Timer stops for both Ravi and David.  
* Ravi’s screen shows: **“Arrived at your location”** or **“Near your gate”**.  
* David gets a short buffer time to find the exact door / entrance.  
* Map on Ravi’s side continues (with intentional slight lag / slower updates) so the arrival feels even faster.

Step 7 – Handover  
David hands over the 2 donuts. OTP or confirmation is completed. Order state → HANDOVER → COMPLETED.

# 5\. Customer App Views (Ravi)

| Screen / State | What Ravi Sees |
| :---- | :---- |
| Cart → Payment | Normal checkout |
| After successful payment | **“Looking for riders”** (spinner / searching animation) |
| Rider assigned | **“Driver accepted – Arriving to your location”** \+ 90s countdown \+ live map |
| Rider enters 100 m zone | **“Arrived at your location / Near your gate”** \+ timer stopped \+ map still visible (slightly lagged) |
| Handover complete | Order completed screen |

# 6\. Rider App Views (David)

| Screen / State | What David Sees |
| :---- | :---- |
| Waiting in Ring | Map of his current zone \+ “Ready – waiting for orders” |
| Order assigned | New order card appears automatically (no Accept button) \+ navigation starts |
| En route | Turn-by-turn navigation to Gate |
| Entered 100 m zone | “You have arrived – find the gate” \+ timer stopped \+ buffer time |
| Handover | Confirm quantity \+ OTP screen |

# 7\. Timer Rules (Exact)

* Timer **starts** only when the system successfully assigns a rider (state \= ASSIGNED).  
* Timer **stops** the moment the rider’s GPS enters the 100-metre Arrival Zone.  
* Both customer and rider see the same remaining time.  
* After timer stops, customer map continues with mild artificial lag for better perception.

# 8\. Key Business Rules

* Riders carry ready stock (fridge \+ oven). No preparation after order.  
* Assignment is fully automatic — nearest eligible scooter inside the Sweet Spot wins.  
* Only scooters already inside the Sweet Spot can be assigned (this guarantees the 80–85 second travel budget).  
* Riders wait inside the larger Ring and move into Sweet Spots as needed.  
* Customer never sees “Accept/Reject” on the rider side.

# 9\. Simple Visual Flow Summary

```
Ravi pays
    ↓
“Looking for riders” (MATCHING)
    ↓
System finds David already inside Sweet Spot → Auto-assigns
    ↓
90s Timer starts for Ravi
Ravi screen → “Driver accepted – Arriving…”
David starts moving
    ↓
David enters 100 m Arrival Zone
    ↓
Timer stops
Ravi screen → “Arrived at your location”
David gets short buffer to find door
    ↓
Handover → Completed
```

*This document is complete and self-contained. Any person or AI reading only this text can fully understand the intended customer experience, rider experience, states, and timing rules.*