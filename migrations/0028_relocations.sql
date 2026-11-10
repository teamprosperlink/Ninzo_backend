-- +goose Up
-- Suggested moves between staging nodes (contract: RelocationSuggestion, acceptRelocation). Moving
-- is paid time from accepted_at to arrived_at. Ignoring or refusing has no penalty.
CREATE TABLE relocations (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id         uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    shift_id         uuid        NOT NULL REFERENCES shifts (id) ON DELETE RESTRICT,
    from_node_id     uuid        NOT NULL REFERENCES staging_nodes (id) ON DELETE RESTRICT,
    to_node_id       uuid        NOT NULL REFERENCES staging_nodes (id) ON DELETE RESTRICT,
    reason           text        NOT NULL CHECK (length(reason) BETWEEN 1 AND 200),
    state            text        NOT NULL DEFAULT 'suggested' CHECK (state IN ('suggested', 'accepted', 'arrived', 'expired', 'cancelled')),
    expires_at       timestamptz NOT NULL,
    accepted_at      timestamptz,
    arrived_at       timestamptz,
    state_changed_at timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (from_node_id <> to_node_id),
    CHECK ((state IN ('accepted', 'arrived')) <= (accepted_at IS NOT NULL)),
    CHECK ((state = 'arrived') = (arrived_at IS NOT NULL))
);
-- One move in play per rider.
CREATE UNIQUE INDEX relocations_one_open_per_rider ON relocations (rider_id) WHERE state IN ('suggested', 'accepted');
CREATE INDEX relocations_open_idx ON relocations (state, expires_at) WHERE state IN ('suggested', 'accepted');
CREATE INDEX relocations_rider_idx ON relocations (rider_id, state_changed_at DESC);
ALTER TABLE relocations ENABLE ROW LEVEL SECURITY;

-- The simple rule's settings (decision 0014: settings, not constants), as in 0026.
ALTER TABLE zone_settings
    ADD COLUMN relocation_window_s  integer DEFAULT 1800 CHECK (relocation_window_s BETWEEN 300 AND 7200),
    ADD COLUMN relocation_min_orders integer DEFAULT 2 CHECK (relocation_min_orders BETWEEN 1 AND 50),
    ADD COLUMN relocation_ttl_s     integer DEFAULT 300 CHECK (relocation_ttl_s BETWEEN 60 AND 1800),
    ADD COLUMN relocation_rest_s    integer DEFAULT 900 CHECK (relocation_rest_s BETWEEN 60 AND 7200),
    ADD COLUMN relocation_arrive_m  integer DEFAULT 100 CHECK (relocation_arrive_m BETWEEN 20 AND 500);
ALTER TABLE zone_settings
    ALTER COLUMN relocation_window_s DROP DEFAULT,
    ALTER COLUMN relocation_min_orders DROP DEFAULT,
    ALTER COLUMN relocation_ttl_s DROP DEFAULT,
    ALTER COLUMN relocation_rest_s DROP DEFAULT,
    ALTER COLUMN relocation_arrive_m DROP DEFAULT;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN relocation_window_s, DROP COLUMN relocation_min_orders, DROP COLUMN relocation_ttl_s,
    DROP COLUMN relocation_rest_s, DROP COLUMN relocation_arrive_m;
DROP TABLE relocations;
