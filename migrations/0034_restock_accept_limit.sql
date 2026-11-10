-- +goose Up
-- A confirmed request no hub accepts within this time expires, so a scooter is never stuck with a
-- request at a hub with no staff (decision 0014: settings, not constants). As in 0026.
ALTER TABLE zone_settings ADD COLUMN restock_accept_s integer DEFAULT 900 CHECK (restock_accept_s BETWEEN 120 AND 7200);
ALTER TABLE zone_settings ALTER COLUMN restock_accept_s DROP DEFAULT;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN restock_accept_s;
