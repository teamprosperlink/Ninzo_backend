-- +goose Up
-- Restock suggestion settings (decision 0014: settings, not constants). Added with defaults so
-- every stored version has them, then the defaults go, as in 0017.
ALTER TABLE zone_settings
    ADD COLUMN restock_low_qty          integer DEFAULT 3 CHECK (restock_low_qty BETWEEN 0 AND 500),
    ADD COLUMN restock_target_qty       integer DEFAULT 10 CHECK (restock_target_qty BETWEEN 1 AND 500),
    ADD COLUMN restock_suggestion_ttl_s integer DEFAULT 600 CHECK (restock_suggestion_ttl_s BETWEEN 60 AND 3600),
    ADD COLUMN restock_check_s          integer DEFAULT 300 CHECK (restock_check_s BETWEEN 60 AND 3600);
ALTER TABLE zone_settings
    ALTER COLUMN restock_low_qty DROP DEFAULT,
    ALTER COLUMN restock_target_qty DROP DEFAULT,
    ALTER COLUMN restock_suggestion_ttl_s DROP DEFAULT,
    ALTER COLUMN restock_check_s DROP DEFAULT;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN restock_low_qty, DROP COLUMN restock_target_qty, DROP COLUMN restock_suggestion_ttl_s,
    DROP COLUMN restock_check_s;
