# 0003: Redis and a Postgres outbox instead of Kafka or NATS

- Status: Proposed (decision agreed with the owner on 2026-11-04; this write-up awaits owner review)
- Date: 2026-11-04
- Deciders: Owner

## Context
The architecture doc lists "Kafka / NATS" as the message bus and copies the event-stream idea from Swiggy. The pilot has 4-8 scooters. The backend still needs live scooter state, fan-out to WebSockets, and reliable events such as payment, stock and order changes.

## Options considered
1. Kafka: strong at high volume, but far more to run than the pilot needs.
2. NATS JetStream: lighter than Kafka, but still another stateful system, and it does not make a database change and its event atomic on its own.
3. Redis for live state and pub/sub, plus a Postgres outbox for durable events: the event commits in the same transaction as the change, using stores we already run.

## Decision
Option 3, to start.

- Redis holds live state and fan-out only. It is never the source of truth for stock or orders.
- Durable events are written to an outbox table in the same transaction as the change.
- A worker delivers them by polling ([0005](0005-supabase-setup.md)).

## Consequences
- Delivery is at-least-once, so every consumer must be idempotent.
- Polling adds latency and database load. Time-critical paths, such as the 90 s flow, must not depend on outbox delivery for their correctness or timing.
- If Redis is lost, live state must be rebuilt from new pings and Postgres. No committed data is lost.
- Consumers should depend on an event interface, so a bus can replace the outbox later. Moving would take a new decision, driven by measured load.

## Links
`docs/engineering/tech_stack.md`, `docs/context/BACKEND_PLAN.md` section 2.
