# Test report: <feature> (#<issue>)

- Date:
- Commit SHA: (the branch commit tested; the test log adds the merge commit)
- Branch:
- Environment: (OS, Go version, Postgres and other services with versions, ports)
- Tester / reviewer:

## 1. Scope and acceptance criteria
What was tested, mapped to the acceptance criteria in `project_spec.md` or the Issue. The criteria are written before testing.

| # | Acceptance criterion | Test(s) that prove it |
|---|---|---|

## 2. How to reproduce
The exact commands, from a clean checkout of the commit above, including the environment variables (never secrets) and the services that must be up.
```
<commands>
```

## 3. Commands run and raw output
```
<command>
<paste real output; do not summarise>
```
Long output goes in a file beside this report (`<issue>-<name>.txt`), committed and linked here.

## 4. Scenario results
| Scenario | Expected | Actual | Result |
|---|---|---|---|

## 5. Deliberate breaks
Each break made alone, the test that caught it, and the file restored (checked with `cmp`). Any break not caught at first, and what was added.

| Break | Caught by |
|---|---|

## 6. Manual / end-to-end validation
Steps taken and what was observed.

## 7. Independent review
Reviewer, spec sections checked, findings, status (no open FAIL required).

## 8. Defects found
| ID | Description | Fixed in | Retested |
|---|---|---|---|

## 9. Known limits
Every way the test differs from production, and anything not tested.

## 10. Verdict
PASS / FAIL, and sign-off by the owner (name, date).

Benchmarks (simulation, performance) are recorded in `docs/benchmarks/` by `docs/benchmarks/methodology.md`, and linked from here.
