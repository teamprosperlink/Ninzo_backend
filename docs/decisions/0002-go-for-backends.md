# 0002: Go for all backends, Python only offline

- Status: Proposed (decision agreed with the owner on 2026-11-04; this write-up awaits owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
The ops console KT doc already fixes Go with chi, pgx and sqlc for its backend. The architecture doc also calls for optimisation and simulation work (OR-Tools, SimPy), which sits outside the request path.

## Options considered
1. Go everywhere, including optimisation: one language, but weaker libraries for this offline work.
2. Go for services and Python for offline jobs: each uses the better tool, and Python stays out of the request path.
3. Python (FastAPI) services for forecasting: only useful if a model must answer live requests, and no such need exists today.

## Decision
Option 2. All backends are Go. Python is used only for offline jobs in `tools/`, such as staging node placement and simulation. FastAPI is added only if a live forecast model is needed later, which would take a new decision.

## Consequences
- One language to build, deploy and review for all live services.
- Offline job output reaches the backend as data, for example sweet-spot cells and node placement, and not through live calls.
- Python tooling needs its own setup and tests in `tools/`.

## Links
`docs/engineering/tech_stack.md`, `docs/context/KT/` (ops console, architecture).
