# 0011: Answers before v0.3.0

- Status: Accepted, except where marked
- Date: 2026-11-07
- Deciders: Owner

## Context
The owner shared five algorithm documents on 2026-11-05, which differ from the flow decided on 2026-11-04. `docs/planning/v0.3.0_open_questions.md` listed the questions, with a recommendation for each. These are the owner's answers.

## Decision
| # | Question | Answer |
|---|---|---|
| 1 | Which flow wins | **The 2026-11-04 flow stays:** the rider accepts an offer, packs while stopped, the GPS timer starts inside the zone, the rider sees a calm label, a 5-second cancel after payment, payment up front, GPS at 100 m with OTP as proof. The documents are used for algorithms only, where they agree with this |
| 2 | A delayed customer map | **No.** The map stays live |
| 3 | A rider losing signal | Keep today's rule: after 30 seconds without a ping, the handover OTP is released. Reassign the order only after 2 minutes without a ping. Both are console settings |
| 4 | The sweet spot's time budget | **A console setting.** Until the console exists, it is a stored setting with a starting value, changed by an audited script. The first value comes from a road test in Bengaluru |
| 5 | The pilot city | **Bengaluru** |
| 6 | A second order for a busy rider | **Open:** the answer "yes" could mean either way. Asked again |
| 7 | Refilling | The backend suggests the nearest hub, and the rider confirms |
| 8 | No rider after the widest search | Refused before payment, with "not available right now" |
| 9 | Checking a location before giving a phone | Yes, without saving anything |
| 10 | Menu | Test items for now. The owner gives the real ones later. Menu, stock and prices are changed through the console later, so they live in tables, never in code |
| 11 | Gates | Test pins in Bengaluru for now, replaced by real ones later |
| 12 | Maps and routing | **Hybrid:** an open-source routing engine (Valhalla or OSRM, to be chosen) for travel times, and Google Maps for showing the map in the apps |

## Consequences
- The contracts and mock stay as they are, because the flow did not change.
- Every number the owner may want to change is a stored setting, not a constant in code. Until the console (`v0.9.0`), settings change only through an audited script.
- Test menu items and test gates are marked as test data, so they can be found and replaced.
