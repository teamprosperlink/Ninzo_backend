-- +goose Up
-- Two settings for the tiers read from gate cell sets (issue 199, geo record W2 steps 4 and 5):
--   timer_zone_margin_s: a cell is in the timer zone only if its road time to the gate is at least
--     this much under the timer, so normal variation in traffic still fits (placeholder, 15 s).
--   outer_ring_budget_s: the widest road time that still counts as reach for a gate, beyond the
--     sweet spot (placeholder, 300 s).
--
-- Added with a default so every global version has a value (zone_settings is append-only, so no
-- row can be updated afterwards), as 0034 and 0040 did. Unlike them, the value the default also
-- writes into zone override rows is cleared again at once: a zone row with a value overrides the
-- global one, so those zones would otherwise stop following global changes for these settings
-- (and the cross-check against the sweet spot could then refuse a valid global change). The zone
-- rows end exactly as they were; only for that one statement is the append-only trigger paused.

-- The background pass and the APIs read settings every few seconds; take the lock first, as 0042.
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

ALTER TABLE zone_settings ADD COLUMN timer_zone_margin_s integer DEFAULT 15 CHECK (timer_zone_margin_s BETWEEN 0 AND 120);
ALTER TABLE zone_settings ALTER COLUMN timer_zone_margin_s DROP DEFAULT;
ALTER TABLE zone_settings ADD COLUMN outer_ring_budget_s integer DEFAULT 300 CHECK (outer_ring_budget_s BETWEEN 30 AND 3600);
ALTER TABLE zone_settings ALTER COLUMN outer_ring_budget_s DROP DEFAULT;

ALTER TABLE zone_settings DISABLE TRIGGER zone_settings_append_only;
UPDATE zone_settings SET timer_zone_margin_s = NULL, outer_ring_budget_s = NULL WHERE zone_id IS NOT NULL;
ALTER TABLE zone_settings ENABLE TRIGGER zone_settings_append_only;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN outer_ring_budget_s;
ALTER TABLE zone_settings DROP COLUMN timer_zone_margin_s;
