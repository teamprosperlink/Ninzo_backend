# Changelog

Format: newest first. One section per version; inside it, one entry per feature or module,
each with its Issue number. Entries are added in the same PR that ships the feature.

## [Unreleased]

### Google sign-in, phone verification and merging (#47)
- Added: `signInWithGoogle`, `requestPhoneOtp` and `verifyPhone` on `customer-api`. Accounts merge only on a phone proven by OTP.
- Added: the `GOOGLE_CLIENT_IDS` setting, required by `customer-api`.
- Evidence: docs/testing/reports/47.md

### SMS setup steps (#45)
- Added: `docs/planning/sms_setup.md`.

### Phone OTP sign-in (#43)
- Added: `requestOtp` and `verifyOtp` on `customer-api` and `rider-api`, with test codes. Real SMS comes later.
- Evidence: docs/testing/reports/43.md

### Rider sign-in is phone and OTP only (#41)
- Removed: rider ID and rider password endpoints and fields from the rider contract and mock.

### Session system (#38)
- Added: `internal/identity` with access and refresh tokens, renewal, the lost-reply retry and ending a chain. `customer-api` and `rider-api` serve `refreshToken` and `signOut`.
- Added: `signOut` to both contracts and the mock.
- Added: migration 0009, the refresh token generation. Not yet applied to Supabase.
- Changed: on any 401 from a signed-in request, apps refresh once and send it again.
- Evidence: docs/testing/reports/38.md

### Idempotency, audit and outbox (#36)
- Added: `internal/platform/idempotency`, `internal/audit`, `internal/platform/outbox` and `internal/platform/sensitive`. No endpoint uses them yet.
- Evidence: docs/testing/reports/36.md

### Questions before v0.3.0 (#34)
- Added: the owner's algorithm documents in `docs/context/algorithms/`, as background, and `docs/planning/v0.3.0_open_questions.md`.

### Service skeleton (#32)
- Added: `apps/customer-api` and `apps/rider-api`, with config, logging, health checks, the database pool and graceful shutdown. No business endpoints yet.
- Changed: `internal/dbtarget` keeps the migration and service logins apart.
- Evidence: docs/testing/reports/32.md

### Sign-in migrations applied to Supabase (#30)
- Changed: migrations 0007 and 0008 are applied to `Ninzo-backend-mumbai`, and verified there.
- Evidence: docs/testing/reports/30.md

### Sign-in contract, mock and tables (#28)
- Added: customer Google sign-in and phone verification. Rider ID, password sign-in and reset, and the `applicant` status.
- Added: migrations for `people`, `person_identities`, `rider_credentials`, the OTP purpose, and the `ninzo_service` database login.
- Changed: phone moved from `customers` and `riders` to `people`.

### Authentication rules (#26)
- Added: `docs/engineering/authentication_rules.md`, recording sign-in rules, roles, situations, problems and solutions, with their reasons.

### Docs (#1)
- Added: `docs/engineering/tech_stack.md` and decisions 0001-0006, awaiting owner review.
- Added: draft customer backend docs in `apps/customer-api/docs/`, awaiting owner review.
- Added: zone timer flow in `docs/engineering/data_flow.md` and decision 0007, replacing the KT clock.
- Added: `CLAUDE.md` rule that every task starts with a GitHub Issue.
- Added: `docs/planning/owner_setup_checklist.md`.

### Migrations applied to Supabase (#22)
- Changed: the 6 migrations are applied to `Ninzo-backend-mumbai`, and verified there.
- Evidence: docs/testing/reports/22.md

### Database foundation (#21)
- Added: migrations for the 10 v0.2.0 tables, a migration revoking Supabase's default grants, `cmd/migrate`, which refuses the wrong target, and database tests in CI.
- Changed: local Redis moved to port 6380. Local Postgres runs under emulation on ARM machines.
- Evidence: docs/testing/reports/21.md

### Endpoint data map (#13)
- Added: `docs/engineering/endpoint_data_map.md`, and a test that keeps it complete.

### Mock server (#8)
- Added: `cmd/mock-server` and `internal/mockserver`: every customer and rider endpoint, all scenarios and live stream replay.
- Added: `cmd/contract-bundle`, `contracts/dist` and `contracts/bruno`.
- Fixed: the rider handover contract now declares a retryable 503.
- Evidence: docs/testing/reports/8.md

### Cancel and refund rule (#15)
- Changed: 5-second cancel window with a full refund, replacing free cancel. No rider is contacted until it closes.
- Evidence: docs/testing/reports/15.md
- Added: `cancel_until` on the order, and `reason_code` and `method` on the refund.
- Changed: payment is charged up front, with no holds. `PaymentStatus` is now `pending`, `paid`, `failed`, `not_paid`, `refunded`.

### Rider contract (#7)
- Added: `contracts/rider/openapi.yaml` and `contracts/rider/stream.asyncapi.yaml`.
- Added: rule tests that keep countdown fields and an arrive endpoint out of the rider contract.
- Evidence: docs/testing/reports/7.md

### Customer contract (#6)
- Added: `contracts/customer/openapi.yaml` and `contracts/customer/stream.asyncapi.yaml`.
- Added: `tests/contracts`, which enforces the contract rules in `go test`.
- Evidence: docs/testing/reports/6.md

### API rules (#5)
- Added: `contracts/shared/common.yaml` and `contracts/README.md`: errors, `server_time`, idempotency, paging, sign-in, live streams, order states, customer stages, timer fields.
- Added: `task contracts:lint` and a CI job that lints contracts.
- Evidence: docs/testing/reports/5.md

### Plan (#3)
- Added: agreed milestones v0.1.0 to v1.0.0 in `docs/planning/milestones.md`, and `app:*` labels.
- Changed: Claude merges PRs once checks pass, with no per-PR owner sign-off.
- Removed: the required QR or code scan on arrival.
- Added: rule that the shared Google Doc is how we communicate with the app developers.

### Config (#1)
- Added: `env.example`, `.gitattributes`, `.editorconfig`, `.dockerignore`, `deploy/compose.yml`, `deploy/Dockerfile.service`, and `db:up` and `db:down` tasks.
- Changed: `.gitignore` now covers keys, service account files and local settings.

<!--
## [v0.1.0] — YYYY-MM-DD
### <feature or module name> (#issue)
- Added / Changed / Fixed / Removed: …
- Evidence: docs/testing/reports/<issue>.md
-->
