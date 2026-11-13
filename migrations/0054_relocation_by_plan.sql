-- +goose Up
-- relocation_by_plan (issue 226, geo record W4 step 5): 0 keeps today's relocation rule (a node with
-- recent orders and nobody waiting asks the nearest node with a spare rider); 1 follows the zone's
-- latest placement plan (#224) for the weekday, half-hour and number of free riders, and falls back
-- to today's rule when there is no plan for that moment. The suggest, accept, pay and rest flow is
-- the same either way. Off by default.
--
-- As 0049, 0051, 0052 and 0053: a global value for every version, cleared from zone override rows.
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

ALTER TABLE zone_settings ADD COLUMN relocation_by_plan integer DEFAULT 0 CHECK (relocation_by_plan BETWEEN 0 AND 1);
ALTER TABLE zone_settings ALTER COLUMN relocation_by_plan DROP DEFAULT;

ALTER TABLE zone_settings DISABLE TRIGGER zone_settings_append_only;
UPDATE zone_settings SET relocation_by_plan = NULL WHERE zone_id IS NOT NULL;
ALTER TABLE zone_settings ENABLE TRIGGER zone_settings_append_only;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN relocation_by_plan;
