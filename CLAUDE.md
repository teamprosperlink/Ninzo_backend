# ninzo-backend: orchestrator

This file points to the real documents. It does not duplicate them. Keep it under 200 lines.

## Vision
Backend for Ninzo: sealed, temperature-tracked food waits inside scooters near demand. Once a packed rider is inside the zone around the customer's location, a visible timer runs, and the rider reaches that location within the time set in the console (90 seconds by default), safely. The first proof is one micro-market with 4-8 scooters and 1-3 menu items (`docs/context/KT/PRD.pdf`). The flow is in `docs/engineering/data_flow.md` and overrides the KT docs where they differ.

## Goals
<!-- Numbered, measurable. To be set with the owner when the PRD and milestones are written. -->

## Scope (owner rule)
Backend only. Claude builds and deploys the backend. The Flutter apps are built by other developers, so never create, edit or plan Flutter code, Flutter repos or Flutter templates. Our work for those developers ends at API contracts, mock servers and staging URLs.

## Role & Responsibility
You are the senior backend developer. You build the backend and its systems, and you take care of deployment too.
Plan first and talk with the owner before writing code.

## Stack
Go modular monolith with separately deployed services. Layout: `docs/engineering/project_structure.md`. Choices made so far: `docs/planning/project_status.md`.

## File index

| Need | Read |
|---|---|
| Original inputs (KT, PRD, earlier plan) | `docs/context/` |
| What the product must do | `docs/product/project_spec.md` |
| How it is built | `docs/engineering/architecture.md`, `data_flow.md`, `project_structure.md`, `tech_stack.md` |
| What each endpoint reads and writes | `docs/engineering/endpoint_data_map.md` |
| Sign-in, sessions, roles and why | `docs/engineering/authentication_rules.md` |
| Every downside we accepted | `docs/engineering/known_downsides.md` |
| What is real, and what is a stand-in or mock only | `docs/engineering/real_and_stand_ins.md` |
| Per-app docs | `apps/<name>/docs/` |
| Accounts, keys and details the owner must supply | `docs/planning/owner_setup_checklist.md`, `docs/planning/sms_setup.md` |
| Non-functional requirements | `docs/engineering/engineering_requirements.md` |
| Constraints and policies | `docs/engineering/constraints_policies.md` |
| Fixed scope per version, and build order | `docs/planning/milestones.md` |
| Questions to answer before v0.3.0, and which algorithms to use | `docs/planning/v0.3.0_open_questions.md` |
| Current situation | `docs/planning/project_status.md` |
| Why we chose X | `docs/decisions/` |
| Test evidence | `docs/testing/test_log.md` |
| What changed | `CHANGELOG.md` |
| How to work here | `CONTRIBUTING.md` |

## Session startup routine (every session, in order)

1. Confirm the working directory and current branch.
2. Read `docs/planning/project_status.md` and `git log --oneline -20`.
3. List open Issues for the current milestone. Pick the issue named by the user, or the top unfinished one.
4. Run the verify command and confirm the baseline is green before changing anything.
5. Work on that one issue only, in its own branch and worktree.

## Commands
- Verify: `task verify`
- Local Postgres and Redis: `task db:up`, `task db:down`
- Database tests, on local Postgres: `task test:db`
- Run an API locally: `task run:customer` (port 8080), `task run:rider` (port 8081)
- Migrations: `task db:migrate` is local only. `task staging:migrate` applies to the database in `.env`, asks first, and needs the owner's okay

## GitHub is the source of truth (owner rule, applies to every task)
1. Before changing any file, open a GitHub Issue for the work, however small: code, docs, config or fixes. Search open Issues first, so nothing is duplicated.
2. Do only that Issue, on its own branch. Commit and push as the work progresses, one Issue after another.
3. Open a PR that says `Closes #<issue>` and lists the commands run with their output. The Issue closes when the PR merges, never by hand.
4. Record anything found along the way, including questions, blockers and new bugs, as a comment or a new Issue. Do not leave it only in chat or in files.
5. If GitHub cannot be reached, say so, and open the Issues as soon as access returns.

## Developer communication (owner rule)
The Google Doc [Ninzo Backend: Shared Index](https://docs.google.com/document/d/1p5sGHeYSo4cCCLlAErqLLqEMnOPkZvk2-gEA4fKgO6M/edit) is the orchestrator for communicating with the app developers, and the index of every shareable, link and note. If anything else needs discussing with them, use the same process:
1. Publish links, notes and changes in the Doc. The owner shares it with the developers.
2. Developers comment in the Doc. The owner says when feedback is in.
3. Read the comments, record them on GitHub, make the fix, and update the Doc.

GitHub stays the source of truth for work and contracts, and wins if the two disagree. Never put secrets in the Doc. Update it whenever a link or shareable changes. Do not ask the owner for the developers' contact details or GitHub usernames: the owner does not have them.

## Never re-ask
Check `docs/planning/project_status.md` and `docs/decisions/` before asking the owner anything. Record each answer there straight away.

## Secrets
Never read `.env`, and never ask the owner to paste a secret or private key into chat, an Issue or a file that gets committed. Secrets go in the local `.env` or the hosting and GitHub secret stores. List new ones in `env.example` with no value. It is named that way because `.claude/settings.json` blocks `.env.*`.

## Hard rules
- Never say "done". Say what was run and show the output. A feature ships only after tests, validation and independent review (see `CONTRIBUTING.md`).
- Never edit or delete an existing test to make it pass.
- Never merge to `main` except through a PR with passing checks. Claude merges its own PRs once checks pass (owner decision, 2026-11-04).
- Issues are the source of truth. Do not track work only in files.
