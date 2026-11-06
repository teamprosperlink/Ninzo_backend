# 0008: Branch and release setup

- Status: Accepted
- Date: 2026-11-06
- Deciders: Owner

## Context
The owner wants new work kept away from what is deployed, so live users are not disturbed and other developers can work alongside. The owner first suggested three long-lived branches: development, staging and production.

## Options considered
1. Three long-lived branches. With one backend developer, development and staging would hold the same code, every change would merge twice, and long-lived branches drift apart.
2. `main` plus `production`, with one short-lived branch per Issue.

## Decision
Option 2.

- **Feature branches:** one per Issue, deleted after merge. They deploy nowhere, and only CI runs on them.
- **`main`:** reviewed work. It deploys to staging automatically once staging exists.
- **`production`:** created near the pilot. It is updated only from `main`, by a tagged release like `v0.2.0` that the owner approves. Rolling back means redeploying the last good tag.
- **`hotfix/...`:** branches from `production` for urgent fixes, then merges back to `main` too.
- **Environments:** local uses Docker. Staging uses the current Mumbai Supabase project, with test data only. Production gets its own Supabase project and its own secrets, and test OTPs cannot run there.
- **Migrations:** each one must keep the running version working.
- **GitHub plan:** the repo stays on the free plan for now (owner, 2026-11-06).

## Consequences
GitHub cannot enforce branch protection on a private repo on the free plan, so nothing technically stops a direct push to `main`. The rule rests on our process: no direct pushes, and merging only through a PR with passing checks. If several backend developers join, or real users depend on `main`, revisit paying for enforcement and adding a development branch.
