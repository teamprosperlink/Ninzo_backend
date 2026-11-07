-- +goose Up
-- Service areas. Their shapes and gates come in the next step.
CREATE TABLE zones (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 80),
    city       text        NOT NULL CHECK (length(city) BETWEEN 1 AND 80),
    status     text        NOT NULL DEFAULT 'paused' CHECK (status IN ('active', 'paused')),
    -- Test data, made before real data exists, so it can be found and replaced.
    is_test    boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE zones ENABLE ROW LEVEL SECURITY;

-- Every value the owner may change from the console. A change is a new version, never an edit, so
-- an order can keep the values it started with. A row with no zone is the global one. A zone's row
-- holds only what it overrides, and its empty columns fall back to the global row.
CREATE TABLE zone_settings (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    zone_id                 uuid REFERENCES zones (id) ON DELETE RESTRICT,
    version                 integer     NOT NULL CHECK (version >= 1),
    timer_length_s          integer CHECK (timer_length_s BETWEEN 30 AND 600),
    zone_radius_m           integer CHECK (zone_radius_m BETWEEN 50 AND 5000),
    arrival_perimeter_m     integer CHECK (arrival_perimeter_m BETWEEN 10 AND 1000),
    min_search_wait_s       integer CHECK (min_search_wait_s BETWEEN 0 AND 600),
    in_zone_wait_s          integer CHECK (in_zone_wait_s BETWEEN 1 AND 600),
    packing_limit_s         integer CHECK (packing_limit_s BETWEEN 10 AND 900),
    max_wait_before_timer_s integer CHECK (max_wait_before_timer_s BETWEEN 30 AND 3600),
    no_scooter_choice_s     integer CHECK (no_scooter_choice_s BETWEEN 10 AND 900),
    cancel_window_s         integer CHECK (cancel_window_s BETWEEN 0 AND 60),
    remedy_credit_paise     integer CHECK (remedy_credit_paise BETWEEN 0 AND 100000),
    signal_release_s        integer CHECK (signal_release_s BETWEEN 5 AND 600),
    reassign_after_s        integer CHECK (reassign_after_s BETWEEN 30 AND 1800),
    sweet_spot_budget_s     integer CHECK (sweet_spot_budget_s BETWEEN 10 AND 600),
    straight_line_speed_kmh integer CHECK (straight_line_speed_kmh BETWEEN 3 AND 60),
    max_search_radius_m     integer CHECK (max_search_radius_m BETWEEN 100 AND 20000),
    reason                  text        NOT NULL CHECK (length(reason) BETWEEN 3 AND 500),
    created_by              text        NOT NULL CHECK (length(created_by) BETWEEN 1 AND 120),
    created_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT zone_settings_one_version UNIQUE NULLS NOT DISTINCT (zone_id, version),
    -- The global row must hold every value an order cannot run without.
    CONSTRAINT zone_settings_global_complete CHECK (zone_id IS NOT NULL OR (
        timer_length_s IS NOT NULL AND zone_radius_m IS NOT NULL AND arrival_perimeter_m IS NOT NULL
        AND max_wait_before_timer_s IS NOT NULL AND cancel_window_s IS NOT NULL AND signal_release_s IS NOT NULL
        AND reassign_after_s IS NOT NULL AND sweet_spot_budget_s IS NOT NULL AND straight_line_speed_kmh IS NOT NULL
        AND max_search_radius_m IS NOT NULL))
);
CREATE INDEX zone_settings_latest_idx ON zone_settings (zone_id, version DESC);
ALTER TABLE zone_settings ENABLE ROW LEVEL SECURITY;
CREATE TRIGGER zone_settings_append_only BEFORE UPDATE OR DELETE ON zone_settings
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
-- Orders will keep the rows they started with, so even the migration login must not wipe them.
CREATE TRIGGER zone_settings_no_truncate BEFORE TRUNCATE ON zone_settings
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- Starting values: decisions 0007 and 0011. The sweet spot budget, speed and search radius are
-- placeholders until the road test in Bengaluru.
INSERT INTO zone_settings (zone_id, version, timer_length_s, zone_radius_m, arrival_perimeter_m,
    max_wait_before_timer_s, cancel_window_s, signal_release_s, reassign_after_s, sweet_spot_budget_s,
    straight_line_speed_kmh, max_search_radius_m, reason, created_by)
VALUES (NULL, 1, 90, 350, 100, 300, 5, 30, 120, 80, 15, 1500,
    'Starting values from decisions 0007 and 0011, before the road test', 'migration 0012');

-- +goose Down
DROP TABLE zone_settings;
DROP TABLE zones;
