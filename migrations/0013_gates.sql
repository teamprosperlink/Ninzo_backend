-- +goose Up
-- A zone's service area. Pins outside every active zone are refused.
ALTER TABLE zones ADD COLUMN area extensions.geometry(MultiPolygon, 4326)
    CHECK (area IS NULL OR extensions.ST_IsValid(area));
CREATE INDEX zones_area_idx ON zones USING gist (area);

-- A customer's saved delivery gates. Ops verify a gate before orders can go there.
CREATE TABLE gates (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid             NOT NULL REFERENCES customers (id) ON DELETE RESTRICT,
    zone_id     uuid             NOT NULL REFERENCES zones (id) ON DELETE RESTRICT,
    label       text             NOT NULL CHECK (length(label) BETWEEN 1 AND 80),
    lat         double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng         double precision NOT NULL CHECK (lng BETWEEN -180 AND 180),
    entry_notes text             CHECK (length(entry_notes) <= 500),
    verified    boolean          NOT NULL DEFAULT false,
    created_at  timestamptz      NOT NULL DEFAULT now(),
    updated_at  timestamptz      NOT NULL DEFAULT now()
);
CREATE INDEX gates_customer_idx ON gates (customer_id, created_at DESC);
ALTER TABLE gates ENABLE ROW LEVEL SECURITY;

-- Every version of a gate, from the first. An order points at the version it was delivered to, so a
-- later change, such as ops moving a pin, never rewrites where an order went.
CREATE TABLE gate_history (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    gate_id     uuid             NOT NULL REFERENCES gates (id) ON DELETE RESTRICT,
    version     integer          NOT NULL CHECK (version >= 1),
    label       text             NOT NULL,
    lat         double precision NOT NULL,
    lng         double precision NOT NULL,
    entry_notes text,
    verified    boolean          NOT NULL,
    reason      text             NOT NULL,
    created_at  timestamptz      NOT NULL DEFAULT now(),
    UNIQUE (gate_id, version)
);
ALTER TABLE gate_history ENABLE ROW LEVEL SECURITY;
CREATE TRIGGER gate_history_append_only BEFORE UPDATE OR DELETE ON gate_history
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER gate_history_no_truncate BEFORE TRUNCATE ON gate_history
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TABLE gate_history;
DROP TABLE gates;
DROP INDEX zones_area_idx;
ALTER TABLE zones DROP COLUMN area;
