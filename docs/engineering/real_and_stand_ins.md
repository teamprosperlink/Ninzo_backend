# What is real, and what stands in for it

The owner's plan (2026-11-09): build every API and the whole pipeline first, with simple stand-ins where something is only an estimate or needs an outside account. Then make each part real. **Money, stock, order states and sign-in rules are real from the start**, because they are hard to fix later.

Update this page in the same PR as any change to a row. The shared Doc points app developers here.

**Status:** Real means production behaviour. Stand-in means it works end to end, with a simple version behind one swappable part. Mock only means only the mock server has it. Not built means nothing answers yet.

## On the real backend (staging)
| Part | Status | What it is today | What makes it real | When |
|---|---|---|---|---|
| Phone sign-in, sessions, refresh, sign-out | Real | As in production | | Done |
| SMS for codes | Stand-in | Test code `123456`, no SMS sent. Production refuses to start in test mode | MSG91, after DLT approval | When credentials arrive |
| Google sign-in | Real | Checked against Google's keys. Tested with a real login on the web, not yet from the Android app | The Android app test on staging | Developer testing now |
| Apple sign-in | Not built | | Apple Developer account and a Mac | Before v1.0.0 |
| One sign-in method, switching, settings change | Real | | | Done |
| Rate limits | Real, in memory | Counted inside each API | Redis, before two replicas of an API | When scaling |
| Profile and devices | Real | Push tokens are stored | Push sending, which needs the worker and FCM | v0.6.0 |
| Settings store | Real | Changed by `task settings` | The admin console | v0.9.0 |
| Zones and service areas | Real, test data | Two rough test boxes in Koramangala and Indiranagar | The owner's real areas | When the owner gives them |
| Gates, location check | Real | Verified by the area they are in (decision 0012) | | Done |
| Menu | Real, test data | Three test items | The owner's real menu, then the console | When the owner gives it |
| Stock, lots, ledger | Real, test data | Lots are made by `task testdata` | Hub APIs make lots | v0.8.0 |
| Rider approval | Stand-in | `task riders`, audited, documents marked checked by hand | Rider application, DigiLocker and the console | v0.3.0 application later, console v0.9.0 |
| Rider shift, nodes, stock scan | Real | A free scooter at the node is the rider's for the shift | | Done |
| **Reach in availability** | Stand-in | Straight-line distance from the scooter's staging node, at a placeholder speed | Rider GPS, then real road times from a routing engine | GPS v0.5.0, roads after the road test |
| H3 map grid | Not built | Not needed at pilot size, where PostGIS is exact | H3 cells, if demand data needs them | After real traffic |
| Matching riders to orders | Not built | | Nearest rider on shift with the whole basket, then ranking | v0.5.0 |
| Orders and holds on one scooter | Real | Built (#78), on staging (#82) | | v0.4.0 |
| Cancel window, retry, gate change and refund rules | Real | Built (#80), on staging (#82). Refunds of the test payment are marked processed at once, as no money moved | Razorpay refunds stay pending until Razorpay says processed | v0.4.0 |
| Background passes: holds end after 10 minutes, seq moves when cancellable changes | Real, in a simple form | A loop every 5 seconds inside the customer API, safe with several copies | A separate worker with scheduled tasks, when load needs it | v0.4.0 |
| Offers: one rider at a time, accept, decline, expiry, Ready, the packing limit, no scooter | Real | Built (#84). The order's own scooter goes first, then the nearest free one holding the whole basket. One order per rider | | v0.5.0 |
| GPS pings, the zone timer, arrival, the missed timer, `rider_moving` | Real | Built (#86). Straight-line distance to the gate, two pings in a row, jumps over 40 m/s and pings over 30 s late dropped | Road distance and location polygons later; the in-zone shorter wait is not used yet | v0.5.0 |
| Handover with the customer's code, partial quantities, stock taken off for good, signal-lost code release | Real | Built (#86). Audit flags are stored on the order; nobody reviews them until the console | Support alerts and remedies in v0.6.0 | v0.5.0 |
| **Payment** | Stand-in | Test payment, marked paid or failed by `POST /test/payments/{order_id}` on staging. Production refuses to start with it | Razorpay and its webhook | Before v1.0.0 |
| Refund money | Being built as a stand-in | The refund is recorded, and no money moves | Razorpay refunds | Before v1.0.0 |
| Background jobs | Being built | Run inside the customer API, one replica | A separate worker | With two replicas, or after the pilot |
| Live streams, timer, handover, offers | Not built | Mock only | | v0.5.0 |
| Failures, remedies, notifications, support tickets | Not built | Mock only | | v0.6.0 |
| Restock, relocation, earnings | Not built | Mock only | | v0.7.0 |
| Partner and hub APIs | Not built | | | v0.8.0 |
| Admin, ops and support APIs | Not built | | | v0.9.0 to v0.10.0 |

## On the mock server
Every customer and rider endpoint in the contracts answers on the mock, with scenarios. Mock replies are samples: they do not check tokens, stock or rules.

## Related
- Accepted downsides: `known_downsides.md`
- Version plan: `docs/planning/milestones.md`
