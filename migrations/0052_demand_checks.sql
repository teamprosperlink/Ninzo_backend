-- +goose Up
-- Demand (issue 220, geo record W7). One row each time a customer asks what is on offer, at most
-- once per IST day, 30-minute band, blr76 cell, customer and outcome. The app asks again every
-- 30 s, so the key deduplicates. No coordinates and no customer id are stored: who is
-- sha256(customer id || '|' || day). That only hides customers from someone holding this table
-- alone: anyone with database access can recompute it from customers.id and re-link every row
-- (known_downsides, #220). demand.Forget finds a customer's rows the same way, for erasure. Rows older than
-- demand_retention_days are dropped (demand.Prune).
CREATE TABLE demand_checks (
    day     date     NOT NULL,
    band    smallint NOT NULL CHECK (band BETWEEN 0 AND 47),
    cell    text     NOT NULL CHECK (cell ~ '^blr76:-?[0-9]+:-?[0-9]+$'),
    who     bytea    NOT NULL CHECK (octet_length(who) = 32),
    outcome text     NOT NULL CHECK (outcome IN ('offered', 'no_coverage', 'sold_out', 'zone_off', 'out_of_service_area')),
    PRIMARY KEY (day, band, cell, who, outcome)
);
ALTER TABLE demand_checks ENABLE ROW LEVEL SECURITY;

-- As 0049 and 0051: a global value for every version, cleared from zone override rows.
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

ALTER TABLE zone_settings ADD COLUMN demand_retention_days integer DEFAULT 84 CHECK (demand_retention_days BETWEEN 7 AND 365);
ALTER TABLE zone_settings ALTER COLUMN demand_retention_days DROP DEFAULT;

ALTER TABLE zone_settings DISABLE TRIGGER zone_settings_append_only;
UPDATE zone_settings SET demand_retention_days = NULL WHERE zone_id IS NOT NULL;
ALTER TABLE zone_settings ENABLE TRIGGER zone_settings_append_only;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN demand_retention_days;
DROP TABLE demand_checks;
