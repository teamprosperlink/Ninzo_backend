# 0001: One repo, one go.mod, each API deployed separately

- Status: Proposed (decision agreed with the owner on 2026-11-04; this write-up awaits owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
The architecture KT doc says "microservices". The pilot is one micro-market with 4-8 scooters and 1-3 menu items (PRD). Four APIs are planned: customer, rider, partner and console. Earlier work split the backend into separate folders (`customer-api`, `rider-api`, `partner-api`, `core`, `contracts`, `console`).

## Options considered
1. Separate repos per service, as in the earlier folders: strong isolation, but shared modules and contract changes must be versioned and released across repos.
2. Single deployed monolith: simplest to run, but one deploy and one failure domain for every app.
3. One repo and one `go.mod`, with each API deployed as its own service: atomic changes across modules and contracts, with separate deployment. Boundaries depend on enforced rules rather than repo walls.

## Decision
Option 3. Shared modules live in `internal/`, thin APIs in `apps/`, and background binaries in `cmd/`.

## Consequences
- A change to a contract, a module and an app can land in one PR.
- Module boundaries are a code rule, so they need enforcing: `docs/engineering/project_structure.md` sets the rules, and an automated import check is still to be added.
- All services share one dependency set, so an upgrade affects all of them at once.
- A module can later move to its own service through its public surface.

## Links
`docs/engineering/project_structure.md`, `docs/context/BACKEND_PLAN.md` section 2.
