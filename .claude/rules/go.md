---
paths:
  - "**/*.go"
---

- Return errors; do not panic outside `main`.
- Pass `context.Context` first; never store it in a struct.
- Money is integer minor units, never float.
- Store timestamps in UTC; return server time in API responses.
- Write requests carry an idempotency key; test the replay case.
- Table-driven tests; run with `-race` for anything concurrent.
