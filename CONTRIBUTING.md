# Contributing: repository etiquette

## 1. Hierarchy

| Level | Meaning | GitHub object |
|---|---|---|
| Milestone | A version (`v0.1.0` … `v1.0.0`). Scope is fixed in `docs/planning/milestones.md` | GitHub **Milestone** + git tag on release |
| Feature (= stage) | One shippable capability | **Issue**, label `type:feature` |
| Phase | A step inside a feature | **Sub-issue** of the feature, label `type:phase` |
| Sub-phase | A small step inside a phase | Checklist item in the phase issue |

Gate rule: do not start the next phase until the current phase is tested and validated.

## 2. Workflows

### 2.1 General workflow (single feature)
1. Read the feature's Issue and its spec section.
2. Create a branch and worktree (section 5).
3. Work phase by phase. After each phase: run tests, record evidence.
4. Write or update the test report `docs/testing/reports/<issue>.md` with real command output.
5. Open a PR (section 6). Independent review runs. Claude merges once checks pass and review has no open FAIL. The owner can hold any PR by saying so.
6. Merge to `main`. Update `CHANGELOG.md` and `docs/testing/test_log.md` in the same PR.

### 2.2 Issue-based workflow
1. Every piece of work starts as a GitHub Issue from a template, before any file changes, including docs, config and small fixes. No Issue, no work.
2. Assign it to a Milestone and a priority label (`priority:P1` before `priority:P2`).
3. Phases become sub-issues; sub-phases become checklist items.
4. The PR says `Closes #<issue>`. The Issue closes only when that PR merges.
5. Nothing is closed by hand. Blocked work gets `status:blocked` and a comment saying why.

## 3. Definition of Done (all required)
- [ ] Unit and integration tests pass.
- [ ] Scenario / end-to-end tests for the feature pass.
- [ ] Manual or e2e validation was done, and its evidence is in the test report.
- [ ] Independent AI review of the diff against the spec: no open FAIL.
- [ ] `CHANGELOG.md`, `docs/testing/test_log.md` and affected docs updated.
- [ ] `docs/planning/project_status.md` reflects the new state.

"It should work" or "tests pass" without output is not evidence.

## 4. Branches and commits
- Branch names: `feat/<issue#>-slug`, `fix/<issue#>-slug`, `docs/<issue#>-slug`, `chore/<issue#>-slug`.
- Commits: Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`), one logical change each.
- `main` takes PRs with passing checks only, with no direct pushes or force pushes. GitHub does not enforce this on the free plan, so everyone must follow it by hand.
- `main` deploys to staging. `production`, added near the pilot, takes tagged releases the owner approves. See `docs/decisions/0008-branch-and-release-setup.md`.

## 5. Worktrees
One feature = one worktree.
```
git worktree add ../ninzo-backend-<issue#>-slug -b feat/<issue#>-slug
git worktree remove ../ninzo-backend-<issue#>-slug      # after merge
```
Never work on `main` directly. Never share a worktree between two sessions.

## 6. Pull requests
- Title in Conventional Commit form. Body from the PR template.
- Must link the Issue, list the commands run with their output, and link the test report.
- Squash merge. Delete the branch after merge.
- A PR that changes behavior without a test change is rejected.

## 7. Logs
- Decisions: `docs/decisions/NNNN-title.md`, one per decision, never edited after acceptance (supersede instead).
- Testing: `docs/testing/test_log.md` (index) + `docs/testing/reports/` (evidence).
- Changes: `CHANGELOG.md`, newest version first, grouped by feature or module.
