# 0004: Supabase Postgres, used only as a database

- Status: Proposed (decision agreed with the owner on 2026-11-04; this write-up awaits owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
The backend needs Postgres with PostGIS as its system of record. Supabase also offers Auth, Storage, Realtime and Edge Functions. Using those would move core logic out of the Go services and tie it to one vendor.

## Options considered
1. Supabase as the full platform: less to build, but auth, realtime and business rules would sit outside the Go code and be harder to test and to move.
2. Supabase as a managed Postgres only: managed database with ordinary Postgres access, and a cleaner path to move later.
3. AWS RDS Multi-AZ from day one: stronger failover, more setup.

## Decision
Option 2. No Supabase Auth, Storage, Realtime or Edge Functions.

Revisit RDS Multi-AZ before the pilot takes real customers, because Supabase automatic failover was found only as a private alpha (finding from 2026-11-04, not re-checked for this record).

## Consequences
- Identity, realtime and storage are built or chosen separately.
- Application code uses standard Postgres, so moving to RDS should mostly be a data migration and a connection change.
- **Pre-pilot gate:** a failover decision must be made before real customers. A database outage during an order breaks the 90 s promise.

## Links
[0005](0005-supabase-setup.md), `docs/planning/project_status.md`.
