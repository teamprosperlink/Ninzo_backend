# Tech stack: ninzo-backend

Version: 0.1.0 · Updated: 2026-11-04

Status values:
- **Agreed:** the owner agreed it on 2026-11-04. See the linked decision.
- **KT:** stated as decided in the KT docs. Not yet separately reviewed with the owner.
- **Proposed:** a recommendation. Not agreed.

## Backend

| Area | Choice | Status | Source |
|---|---|---|---|
| Language | Go. Installed locally: 1.27.3 | Agreed | [0002](../decisions/0002-go-for-backends.md) |
| Repo shape | One repo, one `go.mod`, each API deployed as its own service | Agreed | [0001](../decisions/0001-one-repo-separate-services.md) |
| Router | chi | KT | Ops console doc, backend section |
| Database access | pgx and sqlc | KT | Ops console doc, backend section |
| Task runner | Task (`Taskfile.yml`) | Agreed by repo setup | `Taskfile.yml` |
| Offline jobs | Python, only in `tools/` | Agreed | [0002](../decisions/0002-go-for-backends.md) |

The ops doc says chi comes from "the backend handover". That handover is not in `docs/context/`, so the ops doc is the only source we have.

## Data and messaging

| Area | Choice | Status | Source |
|---|---|---|---|
| System of record | Supabase Postgres, used only as a database | Agreed | [0004](../decisions/0004-supabase-postgres-database-only.md) |
| Supabase setup | Mumbai, session pooler, small pool per service, Data API off or granted nothing | Agreed | [0005](../decisions/0005-supabase-setup.md) |
| Spatial | PostGIS in the `extensions` schema; H3 cells | PostGIS agreed; H3 from the architecture doc | [0005](../decisions/0005-supabase-setup.md), architecture doc |
| Live state and fan-out | Redis. Never the source of truth for stock | Agreed | [0003](../decisions/0003-redis-and-postgres-outbox.md) |
| Events | Postgres outbox, delivered by polling | Agreed | [0003](../decisions/0003-redis-and-postgres-outbox.md), [0005](../decisions/0005-supabase-setup.md) |
| Schema changes | SQL files in `migrations/` only | Agreed | [0005](../decisions/0005-supabase-setup.md) |
| Local and test database | Docker Postgres (`deploy/compose.yml`) | Agreed | [0005](../decisions/0005-supabase-setup.md) |
| Migration tool | Not chosen. Proposed: goose | Proposed | None |

## External services

| Need | Choice | Status | Note |
|---|---|---|---|
| Payments | Razorpay, webhook-driven | KT | Charged up front, with no holds: owner decision, 2026-11-04 |
| Push | Firebase Cloud Messaging | KT | |
| SMS and OTP | SMS gateway, not chosen. Test OTP until DLT is done | KT | Customer doc scope |
| Routing engine | OSRM or Valhalla with an OpenStreetMap extract | Proposed | Needs the pilot city: status open question 6 |
| Masked calls | Telephony provider, not chosen | Open | Customer doc lists masked call; no endpoint yet |

## Contracts and testing

| Need | Choice | Status | Source |
|---|---|---|---|
| HTTP contract | OpenAPI, one per app, in `contracts/` | Repo layout | `project_structure.md` |
| Live events contract | AsyncAPI in `contracts/` | Repo layout | `project_structure.md` |
| Mock server | Prism, with `X-Mock-Scenario` | KT | Customer doc |
| Contract tests | Schemathesis | KT | Customer doc |
| Manual API testing | Bruno collection | KT | Customer doc, `project_structure.md` |
| Integration tests against a real database | Docker Postgres | Agreed | [0005](../decisions/0005-supabase-setup.md) |

## Operations

| Need | Choice | Status | Note |
|---|---|---|---|
| Backend and Redis hosting | Proposed: AWS Mumbai | Proposed | Status open question 1 |
| Observability | OpenTelemetry, Prometheus, Grafana; Sentry for errors | KT | Architecture doc; Sentry from the earlier plan |
| Mock server hosting | Railway, trial plan | Live | Not the production host |
| CI | GitHub Actions (`.github/workflows/ci.yml`) | Skeleton, never run | Needs the GitHub repo and `go.mod` |

## Not used
- Supabase Auth, Storage, Realtime and Edge Functions ([0004](../decisions/0004-supabase-postgres-database-only.md)).
- Kafka and NATS, to start ([0003](../decisions/0003-redis-and-postgres-outbox.md)).
- FastAPI, unless a live forecast model is needed later ([0002](../decisions/0002-go-for-backends.md)).
- The console frontend does not live in this repo ([0006](../decisions/0006-console-web-separate-repo.md)).
