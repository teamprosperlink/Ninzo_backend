# Decision log

One file per decision: `NNNN-short-title.md`, copied from `0000-template.md`.
Accepted decisions are never edited; to reverse one, write a new decision and mark the old one "Superseded by NNNN".

| ID | Title | Status | Date |
|---|---|---|---|
| [0001](0001-one-repo-separate-services.md) | One repo, one go.mod, each API deployed separately | Proposed | 2026-11-04 |
| [0002](0002-go-for-backends.md) | Go for all backends, Python only offline | Proposed | 2026-11-04 |
| [0003](0003-redis-and-postgres-outbox.md) | Redis and a Postgres outbox instead of Kafka or NATS | Proposed | 2026-11-04 |
| [0004](0004-supabase-postgres-database-only.md) | Supabase Postgres, used only as a database | Proposed | 2026-11-04 |
| [0005](0005-supabase-setup.md) | Supabase setup | Proposed | 2026-11-04 |
| [0006](0006-console-web-separate-repo.md) | Console frontend in a separate console-web repo | Proposed | 2026-11-04 |
| [0007](0007-zone-timer-order-flow.md) | Zone-based order timer | Proposed | 2026-11-04 |
| [0008](0008-branch-and-release-setup.md) | Branch and release setup | Accepted | 2026-11-06 |
| [0009](0009-one-sign-in-method.md) | One sign-in method per customer | Accepted | 2026-11-06 |
| [0010](0010-rate-limits-in-memory.md) | Rate limits counted in memory, for now | Proposed | 2026-11-06 |
| [0011](0011-v030-answers.md) | Answers before v0.3.0 | Accepted | 2026-11-07 |
| [0012](0012-gate-verification.md) | Gates are verified by the area they are in | Accepted | 2026-11-07 |
| [0013](0013-v040-answers.md) | Answers before v0.4.0 | Accepted | 2026-11-09 |
| [0014](0014-v070-to-v0100-answers.md) | Answers for v0.7.0 to v0.10.0: admin-entered rates and hubs, staff sign-in, roles and approvals | Accepted | 2026-11-10 |
