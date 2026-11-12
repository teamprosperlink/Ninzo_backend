-- +goose Up
-- Road-time cell sets per gate version (issue 196, geo record W2 step 3). For each version of a
-- gate (gate_history: a moved pin is a new version), the cells of the pilot grid around it, each
-- with its road time to the gate from one OSRM build (road_data_versions). Tiers (timer zone,
-- sweet spot, rings) are read from these times and the current settings, so a settings change
-- needs no rebuild; new road data or a moved gate makes a new build.
--
-- Builds are facts that orders will point to (G25), so they are append-only, with one exception:
-- erasing a customer redacts their builds (redacted_at, once) and deletes the cells, because the
-- cells around a home give its location away.

CREATE TABLE gate_cell_builds (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    gate_history_id      uuid        NOT NULL REFERENCES gate_history (id) ON DELETE RESTRICT,
    road_data_version_id uuid        NOT NULL REFERENCES road_data_versions (id) ON DELETE RESTRICT,
    grid                 text        NOT NULL CHECK (grid ~ '^[a-z][a-z0-9]{0,15}$'),
    max_radius_m         integer     NOT NULL CHECK (max_radius_m BETWEEN 100 AND 5000),
    cells                integer     NOT NULL CHECK (cells BETWEEN 1 AND 20000),
    built_at             timestamptz NOT NULL DEFAULT now(),
    redacted_at          timestamptz
);
CREATE INDEX gate_cell_builds_latest_idx ON gate_cell_builds (gate_history_id, built_at DESC, id DESC);
ALTER TABLE gate_cell_builds ENABLE ROW LEVEL SECURITY;

CREATE TABLE gate_cells (
    build_id       uuid    NOT NULL REFERENCES gate_cell_builds (id) ON DELETE RESTRICT,
    cell           text    NOT NULL CHECK (cell ~ '^[a-z][a-z0-9]{0,15}:-?[0-9]{1,10}:-?[0-9]{1,10}$'),
    -- Straight-line metres from the cell's centre to the gate.
    centre_metres  real    NOT NULL CHECK (centre_metres >= 0 AND centre_metres <= 6000),
    -- Road time and distance from the cell's centre to the gate; both null when no road joins them.
    road_seconds   real    CHECK (road_seconds >= 0 AND road_seconds < 36000),
    road_metres    real    CHECK (road_metres >= 0 AND road_metres < 200000),
    PRIMARY KEY (build_id, cell),
    CONSTRAINT gate_cells_reach_together CHECK ((road_seconds IS NULL) = (road_metres IS NULL))
);
ALTER TABLE gate_cells ENABLE ROW LEVEL SECURITY;

-- +goose StatementBegin
CREATE FUNCTION gate_cell_builds_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    -- The only change allowed: marking a build redacted, once, with nothing else changed.
    IF TG_OP = 'UPDATE' AND OLD.redacted_at IS NULL AND NEW.redacted_at IS NOT NULL
       AND (NEW.id, NEW.gate_history_id, NEW.road_data_version_id, NEW.grid, NEW.max_radius_m, NEW.cells, NEW.built_at)
           IS NOT DISTINCT FROM
           (OLD.id, OLD.gate_history_id, OLD.road_data_version_id, OLD.grid, OLD.max_radius_m, OLD.cells, OLD.built_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'gate_cell_builds is append-only' USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER gate_cell_builds_append_only BEFORE UPDATE OR DELETE ON gate_cell_builds
    FOR EACH ROW EXECUTE FUNCTION gate_cell_builds_guard();
CREATE TRIGGER gate_cell_builds_no_truncate BEFORE TRUNCATE ON gate_cell_builds
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- +goose StatementBegin
CREATE FUNCTION gate_cells_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    -- Cells are deleted only once their build is redacted (erasure); they are never changed.
    IF TG_OP = 'DELETE' AND EXISTS (
        SELECT 1 FROM public.gate_cell_builds b WHERE b.id = OLD.build_id AND b.redacted_at IS NOT NULL) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'gate_cells is append-only' USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER gate_cells_append_only BEFORE UPDATE OR DELETE ON gate_cells
    FOR EACH ROW EXECUTE FUNCTION gate_cells_guard();
CREATE TRIGGER gate_cells_no_truncate BEFORE TRUNCATE ON gate_cells
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- Nothing new for an erased gate version, and no cells for an erased build or from another grid
-- (the cmd checks too, under a lock; this is the floor).
-- +goose StatementBegin
CREATE FUNCTION gate_cells_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    IF TG_TABLE_NAME = 'gate_cell_builds' THEN
        IF EXISTS (SELECT 1 FROM public.gate_history h WHERE h.id = NEW.gate_history_id AND h.redacted_at IS NOT NULL) THEN
            RAISE EXCEPTION 'gate version % is erased', NEW.gate_history_id USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.redacted_at IS NOT NULL THEN
            RAISE EXCEPTION 'a new build cannot start redacted' USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.gate_cell_builds b
                   WHERE b.id = NEW.build_id AND b.redacted_at IS NULL AND split_part(NEW.cell, ':', 1) = b.grid) THEN
        RAISE EXCEPTION 'cell % does not fit build % (erased, or another grid)', NEW.cell, NEW.build_id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER gate_cell_builds_insert_guard BEFORE INSERT ON gate_cell_builds
    FOR EACH ROW EXECUTE FUNCTION gate_cells_insert_guard();
CREATE TRIGGER gate_cells_insert_guard BEFORE INSERT ON gate_cells
    FOR EACH ROW EXECUTE FUNCTION gate_cells_insert_guard();

-- +goose Down
DROP TABLE gate_cells;
DROP TABLE gate_cell_builds;
DROP FUNCTION gate_cells_guard();
DROP FUNCTION gate_cells_insert_guard();
DROP FUNCTION gate_cell_builds_guard();
