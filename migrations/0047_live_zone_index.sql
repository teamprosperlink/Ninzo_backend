-- +goose Up
-- The console's live map (issue 150) reads the orders in progress in one zone every few seconds.
-- No index covers that: orders_dispatch_idx and orders_timer_idx hold only some of these states
-- and are not by zone, and orders_zone_created_idx walks the zone's whole history. This one holds
-- only the orders in progress, so it stays a handful of rows however many orders there have been.
-- The list of states is the one in internal/console/live.go (liveOrderStates); the planner uses
-- the index only when the query's list is the same or smaller.
--
-- Plain CREATE INDEX, as 0041 and 0040 do (goose runs a migration in a transaction, where
-- CONCURRENTLY is not allowed). The lock is taken first so the build waits its turn at once,
-- instead of upgrading a lock part way and deadlocking with the order writers.
LOCK TABLE orders IN SHARE MODE;
CREATE INDEX orders_live_zone_idx ON orders (zone_id, created_at, id)
    WHERE state IN ('PAYMENT_SECURED', 'OFFERED', 'NO_SCOOTER', 'RIDER_ACCEPTED', 'PACKED', 'TIMER_RUNNING', 'ARRIVED');

-- +goose Down
DROP INDEX orders_live_zone_idx;
