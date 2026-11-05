# Project structure: ninzo-backend

Version: 0.1.0 · Updated: 2026-11-04

One repo, one `go.mod`, several separately deployed services.

```
ninzo-backend/
├─ CLAUDE.md  README.md  CONTRIBUTING.md  CHANGELOG.md  Taskfile.yml
├─ .github/        issue + PR templates, CODEOWNERS, labels, workflows/ci.yml
├─ .claude/        settings, rules, example hooks
├─ docs/           system-wide docs: context, product, engineering, planning, decisions, testing
├─ contracts/      OpenAPI per app, AsyncAPI for live events, mock scenarios, Bruno collection
├─ migrations/     the only place for schema changes; applied files are never edited
├─ internal/       shared modules and platform code
├─ apps/           one folder per API: customer-api, rider-api, partner-api, console-api
├─ cmd/            background and support binaries: worker, tracker (later), rider-sim
├─ tools/          offline Python jobs: staging node placement, simulation
├─ tests/          end-to-end and scenario tests (unit tests sit next to the code)
└─ deploy/         compose, infrastructure, one Dockerfile shared by every service
```

Folders under `internal/`, `apps/` and `cmd/` are created when first needed, never in advance.

## Shared modules (`internal/`)
Planned: `platform`, `identity`, `geo`, `catalog`, `inventory`, `orders`, `payments`, `dispatch`, `tracking`, `restock`, `ops`, `support`, `audit`, `notify`, `realtime`.

```
internal/<module>/
├─ <module>.go     public surface: the only part other code may call
├─ module.go       wiring
├─ CHANGELOG.md
├─ domain/         entities, rules, state machines; no I/O
├─ app/            use cases
├─ store/          SQL and sqlc output; the only place with SQL for this module
└─ events/         events published and consumed; only if the module uses events
```

`platform` holds technical code only (config, database, logging, telemetry, idempotency, middleware), with no business rules.

## Apps (`apps/`)
Apps stay thin: handlers, request and response types, and wiring. No SQL and no business rules.

```
apps/<name>/
├─ main.go         wiring only
├─ CLAUDE.md  CHANGELOG.md
├─ docs/           PRD, project_spec, EDD, architecture, milestones and stages
└─ internal/       one folder per API area
```

## Rules
- A module imports another module only through that module's public surface.
- No SQL against another module's tables.
- No general `utils` or `common` package.
- One app never imports another app.
- `main.go` contains no business logic.
- Shared facts live in the system docs; app docs link to them and cover only what is specific to that app.
- Tests live next to the code (`*_test.go`); scenario tests live in `tests/`.
- Run `task verify` before every commit.

## Naming
Packages: short, lowercase, singular where natural. Files: `snake_case.go`. Migrations: `NNNN_description.sql`.
