-- +goose Up
-- A zone's service area. Pins outside every active zone are refused.
ALTER TABLE zones ADD COLUMN area extensions.geometry(MultiPolygon, 4326)
    CHECK (area IS NULL OR (extensions.ST_IsValid(area) AND NOT extensions.ST_IsEmpty(area)));
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
    redacted_at timestamptz,
    UNIQUE (gate_id, version)
);
ALTER TABLE gate_history ENABLE ROW LEVEL SECURITY;

-- History is never rewritten, with one exception: when a customer's data is erased, the label and
-- notes go and the pin is blurred to about 1 km, once. That is all this allows.
-- +goose StatementBegin
CREATE FUNCTION gate_history_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.redacted_at IS NULL AND NEW.redacted_at IS NOT NULL
        AND NEW.id = OLD.id AND NEW.gate_id = OLD.gate_id AND NEW.version = OLD.version
        AND NEW.verified = OLD.verified AND NEW.reason = OLD.reason AND NEW.created_at = OLD.created_at
        AND NEW.label = '(removed)' AND NEW.entry_notes IS NULL
        AND NEW.lat = round(OLD.lat::numeric, 2)::double precision AND NEW.lng = round(OLD.lng::numeric, 2)::double precision THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'gate history can only be redacted, once';
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION gate_history_guard() FROM PUBLIC;
CREATE TRIGGER gate_history_append_only BEFORE UPDATE OR DELETE ON gate_history
    FOR EACH ROW EXECUTE FUNCTION gate_history_guard();
CREATE TRIGGER gate_history_no_truncate BEFORE TRUNCATE ON gate_history
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TABLE gate_history;
DROP FUNCTION gate_history_guard();
DROP TABLE gates;
DROP INDEX zones_area_idx;
ALTER TABLE zones DROP COLUMN area;
