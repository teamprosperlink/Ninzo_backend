# 0013: Answers before v0.4.0

- Status: Accepted
- Date: 2026-11-09
- Deciders: Owner

## Decision
The owner accepted each recommendation, and the plan: build every API and the whole pipeline first, then make each part real (see `docs/engineering/real_and_stand_ins.md`).

| # | Question | Answer |
|---|---|---|
| 1 | How long stock is held for an unpaid order | 10 minutes, as a setting |
| 2 | How app developers trigger a test payment | A staging-only endpoint marks a payment paid or failed, as Razorpay's webhook will. Production refuses it |
| 3 | Where background jobs run | Inside the customer API, while it has one replica |
| 4 | H3 and the other algorithms | After the APIs and pipeline. Estimates stay simple, behind one swappable part each. Money, stock, order states and sign-in are real from the start |

## Consequences
- `real_and_stand_ins.md` lists what is real, what is a stand-in and what is mock only, and is updated with every change.
- Razorpay, real SMS, Apple sign-in, rider GPS, road times and H3 come later, without changing the order flow.
