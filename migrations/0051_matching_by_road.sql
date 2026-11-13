-- +goose Up
-- matching_by_road (issue 209, geo record W5): 0 tries the scooters that can take an order in
-- today's order, nearest staging node by straight line; 1 tries them fastest by road from where
-- each rider is. Off by default: which is better is the owner's product call (w8, #205).
--
-- As 0049: added with a default so every global version has a value, then the value the default
-- also writes into zone override rows is cleared at once, so those zones keep following global.
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

ALTER TABLE zone_settings ADD COLUMN matching_by_road integer DEFAULT 0 CHECK (matching_by_road BETWEEN 0 AND 1);
ALTER TABLE zone_settings ALTER COLUMN matching_by_road DROP DEFAULT;

ALTER TABLE zone_settings DISABLE TRIGGER zone_settings_append_only;
UPDATE zone_settings SET matching_by_road = NULL WHERE zone_id IS NOT NULL;
ALTER TABLE zone_settings ENABLE TRIGGER zone_settings_append_only;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN matching_by_road;
