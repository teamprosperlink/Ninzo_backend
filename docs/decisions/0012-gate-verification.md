# 0012: Gates are verified by the area they are in

- Status: Accepted
- Date: 2026-11-07
- Deciders: Owner

## Context
Orders need a verified gate. Nothing verified gates, so every new gate would have blocked orders.

## Decision
A gate is verified automatically when its pin is inside an area we serve (owner, 2026-11-07). Saving a gate already refuses a pin outside every active zone, so a saved gate is verified at once. An order checks the zone again, so pausing an area stops its gates without changing them.

Pin codes are not used yet. A pin code covers a wider area than a zone, part of which may not be served, and reading one needs an address lookup. Each zone can list served pin codes later, as an extra check.

## Consequences
- Ops do not have to check each gate before a customer can order.
- A pin placed on the wrong building inside a served area still counts. Riders see the entry notes, and support handles a wrong pin.
