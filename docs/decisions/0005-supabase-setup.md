# 0005: Supabase setup

- Status: Proposed (decision agreed with the owner on 2026-11-04; this write-up awaits owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
[0004](0004-supabase-postgres-database-only.md) uses Supabase as a plain database. Several separately deployed services and a worker will connect to it, and the data must stay in India for a pilot in India.

## Decision

| Setting | Choice |
|---|---|
| Region | Mumbai |
| Project | `Ninzo-backend-mumbai`, ref `chyejfgsipcsdxnginei`. `cmd/migrate` refuses any other |
| Connections | Session pooler, with a small connection pool per service |
| Outbox delivery | Polling |
| Data API | Off, or granted nothing. Migration 0006 revokes the grants, so this does not depend on a dashboard setting |
| PostGIS | Installed in the `extensions` schema |
| Schema changes | Through `migrations/` only, never from the dashboard |
| Local work and tests | Docker Postgres |

## Options considered
Each setting was agreed as listed. Alternatives were not recorded at the time, so none are listed here.

## Consequences
- Pool sizes must add up to less than the plan's connection limit across all services and the worker. Check this whenever a service is added.
- Outbox polling needs no long-lived replication or `LISTEN` connection.
- Supabase has PostGIS 3.3.7, and local and CI have 3.5. Use only functions that exist in 3.3.
- Migrations and queries must reference PostGIS through the `extensions` schema, or set `search_path` to include it.
- The local Docker image must include PostGIS and match Supabase's major Postgres version, or tests may pass locally and fail in production.
- With the Data API off, the only path to the data is through our services.

## Links
[0003](0003-redis-and-postgres-outbox.md), [0004](0004-supabase-postgres-database-only.md).
