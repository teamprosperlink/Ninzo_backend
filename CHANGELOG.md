# Changelog

Format: newest first. One section per version; inside it, one entry per feature or module,
each with its Issue number. Entries are added in the same PR that ships the feature.

## [Unreleased]

### Docs (#1)
- Added: `docs/engineering/tech_stack.md` and decisions 0001-0006, awaiting owner review.
- Added: draft customer backend docs in `apps/customer-api/docs/`, awaiting owner review.
- Added: zone timer flow in `docs/engineering/data_flow.md` and decision 0007, replacing the KT clock.
- Added: `CLAUDE.md` rule that every task starts with a GitHub Issue.
- Added: `docs/planning/owner_setup_checklist.md`.

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
