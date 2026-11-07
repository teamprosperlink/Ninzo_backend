# 0010: Rate limits counted in memory, for now

- Status: Proposed
- Date: 2026-11-06
- Deciders: Claude, for owner review

## Context
`refreshToken` and the sign-in endpoints need no sign-in, and each call can take a connection from a small pool or cost an SMS. The plan was to count in Redis. The owner chose Railway's free plan until full development is done, which gives $1 of credit a month, and Redis would be another service using it all the time. Nothing else in the Go code uses Redis yet.

## Options considered
1. Count in Redis. Correct across replicas, but it costs a service now, and adds a new way to fail.
2. Count in Postgres. Survives restarts, but it spends the database connections these limits exist to protect.
3. Count in memory, behind one small interface. Free and simple. Counts reset on restart, and each replica counts alone.

## Decision
Option 3 while each API runs one replica. `ratelimit.Limiter` has two methods, so a Redis store can replace it without touching the rules.

- **Rules live in one place:** `internal/identity/limits.go`.
- **Per address, generous:** many mobile users in India share one address.
- **Per phone and address:** 5 codes an hour, so a stranger cannot lock out a phone's owner from another address. This was the recommended answer to the lockout question, and the owner did not overrule it.
- **Per phone, all addresses:** raised from 5 to 20 codes an hour. This stays in Postgres, as the ceiling on SMS cost for one number.
- **If the limiter fails, requests go through:** sign-in matters more, and the Postgres limits still hold.
- **Finding the caller:** `CLIENT_IP_HEADER` names the proxy's header. On Railway that is `X-Real-IP`. Railway's docs name it, and its staff say the edge overwrites it, though not as a guarantee. Confirmed on staging on 2026-11-07: a forged `X-Real-IP` was overwritten, and the limit held. The API warns once if the header is missing.
- **If the table fills up:** new keys share one larger bucket per rule, so the limit stays on. Real new users can be refused while an attack lasts, which is listed in the known downsides.

## Consequences
See [Known downsides](../engineering/known_downsides.md). **Before running two replicas of an API, move to Redis**, or each replica allows the full limit.
