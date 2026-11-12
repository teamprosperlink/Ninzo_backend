-- +goose Up
-- Hub operations in the console (issue 149): when a restock request counts as stuck.

-- Take the lock first, as 0042 does, so this migration only waits for the background passes and
-- the API that write to zone_settings every few seconds instead of deadlocking with them.
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

-- A request in confirmed, hub_accepted or packing that has waited longer than this is stuck (decision
-- 0014: settings, not constants). 10 minutes: long enough that a hub mid-pack is not flagged, short
-- of restock_accept_s (15 minutes), after which an unaccepted request expires on its own, so ops
-- sees it first. Added with a default so every stored version has it (zone_settings is append-only,
-- so no row can be updated), then the default goes, as in 0034, 0040 and 0042.
ALTER TABLE zone_settings ADD COLUMN restock_stuck_s integer DEFAULT 600 CHECK (restock_stuck_s BETWEEN 60 AND 7200);
ALTER TABLE zone_settings ALTER COLUMN restock_stuck_s DROP DEFAULT;

-- +goose Down
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

ALTER TABLE zone_settings DROP COLUMN restock_stuck_s;
