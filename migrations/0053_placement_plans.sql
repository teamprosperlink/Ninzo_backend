-- +goose Up
-- Placement plans (issue 224, geo record W4 step 4). For a zone, for each IST weekday and half-hour
-- band with recorded demand, and for each number of free scooters n, the staging nodes where n
-- scooters cover the most expected demand within the sweet spot by road (maximum expected covering,
-- internal/placement). A build is made from the demand of the last placement_weeks weeks; the latest
-- build of a zone is the current plan. Nothing reads plans yet: relocation v2 will, behind a setting.
-- Plans hold node ids, not cells or customers, so they need no redaction and are append-only.
CREATE TABLE placement_builds (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    zone_id         uuid        NOT NULL REFERENCES zones (id) ON DELETE RESTRICT,
    demand_from     date        NOT NULL,
    demand_to       date        NOT NULL CHECK (demand_to >= demand_from),
    busy_share_pct  integer     NOT NULL CHECK (busy_share_pct BETWEEN 0 AND 90),
    budget_s        integer     NOT NULL CHECK (budget_s BETWEEN 10 AND 600),
    nodes           integer     NOT NULL CHECK (nodes >= 1),
    cells           integer     NOT NULL CHECK (cells >= 1),
    plans           integer     NOT NULL CHECK (plans >= 1),
    built_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX placement_builds_latest_idx ON placement_builds (zone_id, built_at DESC, id DESC);
ALTER TABLE placement_builds ENABLE ROW LEVEL SECURITY;

CREATE TABLE placement_plans (
    build_id  uuid             NOT NULL REFERENCES placement_builds (id) ON DELETE RESTRICT,
    weekday   smallint         NOT NULL CHECK (weekday BETWEEN 0 AND 6), -- 0 is Sunday, IST
    band      smallint         NOT NULL CHECK (band BETWEEN 0 AND 47),
    n         smallint         NOT NULL CHECK (n BETWEEN 1 AND 500),
    -- One entry per scooter: a node appears as many times as scooters wait there.
    node_ids  uuid[]           NOT NULL CHECK (cardinality(node_ids) = n),
    expected  double precision NOT NULL CHECK (expected BETWEEN 0 AND 1),
    covered   double precision NOT NULL CHECK (covered BETWEEN 0 AND 1 AND expected <= covered + 1e-9),
    PRIMARY KEY (build_id, weekday, band, n)
);
ALTER TABLE placement_plans ENABLE ROW LEVEL SECURITY;

CREATE TRIGGER placement_builds_append_only BEFORE UPDATE OR DELETE ON placement_builds
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER placement_builds_no_truncate BEFORE TRUNCATE ON placement_builds
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();
CREATE TRIGGER placement_plans_append_only BEFORE UPDATE OR DELETE ON placement_plans
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER placement_plans_no_truncate BEFORE TRUNCATE ON placement_plans
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- As 0049, 0051 and 0052: a global value for every version, cleared from zone override rows.
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

ALTER TABLE zone_settings ADD COLUMN placement_busy_share_pct integer DEFAULT 30 CHECK (placement_busy_share_pct BETWEEN 0 AND 90);
ALTER TABLE zone_settings ALTER COLUMN placement_busy_share_pct DROP DEFAULT;
ALTER TABLE zone_settings ADD COLUMN placement_weeks integer DEFAULT 4 CHECK (placement_weeks BETWEEN 1 AND 12);
ALTER TABLE zone_settings ALTER COLUMN placement_weeks DROP DEFAULT;

ALTER TABLE zone_settings DISABLE TRIGGER zone_settings_append_only;
UPDATE zone_settings SET placement_busy_share_pct = NULL, placement_weeks = NULL WHERE zone_id IS NOT NULL;
ALTER TABLE zone_settings ENABLE TRIGGER zone_settings_append_only;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN placement_weeks;
ALTER TABLE zone_settings DROP COLUMN placement_busy_share_pct;
DROP TABLE placement_plans;
DROP TABLE placement_builds;
