# ninzo-backend

Backend for Ninzo, a 90-second delivery service: one Go repo holding the customer, rider, partner and console APIs, their shared modules, the API contracts and the database migrations. Each API deploys as its own service. The Flutter apps and the console website live in their own repos.

- Stack: Go
- Created: 2026-11-04 from project-kit 0.1.0
- Start here: `CLAUDE.md` (index), `CONTRIBUTING.md` (how we work)

## Run
No services exist yet. Local setup so far:

1. `cp env.example .env`, then fill in values. Never commit `.env`.
2. Start Docker Desktop, then run `task db:up` for local Postgres and Redis.

Accounts and keys the owner must supply: `docs/planning/owner_setup_checklist.md`.

## Test
<!-- Fill in once code exists. -->
