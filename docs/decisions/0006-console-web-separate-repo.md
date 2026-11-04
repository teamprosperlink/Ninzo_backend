# 0006: Console frontend in a separate console-web repo

- Status: Proposed (decision agreed with the owner on 2026-11-04; this write-up awaits owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
The ops console is a Next.js web app with a Go backend (ops console KT doc). `ninzo-backend` is a Go repo with one `go.mod`. Who builds the console frontend is still open (status open question 2).

## Options considered
1. Next.js inside `ninzo-backend`: atomic changes, but mixes two toolchains and CI setups in one repo.
2. A separate `console-web` repo: each repo keeps one toolchain, and the frontend depends only on the published contract.

## Decision
Option 2. This repo holds `console-api` and its contract. The frontend goes in `console-web`.

## Consequences
- The console frontend works from `contracts/` and the mock server, the same way as the mobile apps.
- A contract change and its frontend change land as separate PRs in separate repos.

## Links
`docs/engineering/project_structure.md`.
