-- +goose Up
-- Who made each gate version from the console (decision 0015). Null for a version the customer
-- made. Console users are never deleted, only deactivated, so the reference always holds.
ALTER TABLE gate_history ADD COLUMN changed_by uuid REFERENCES console_users (id) ON DELETE RESTRICT;

-- The guard from 0013, now also keeping who made a version: a redaction cannot rewrite it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION gate_history_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.redacted_at IS NULL AND NEW.redacted_at IS NOT NULL
        AND NEW.id = OLD.id AND NEW.gate_id = OLD.gate_id AND NEW.version = OLD.version
        AND NEW.verified = OLD.verified AND NEW.reason = OLD.reason AND NEW.created_at = OLD.created_at
        AND NEW.changed_by IS NOT DISTINCT FROM OLD.changed_by
        AND NEW.label = '(removed)' AND NEW.entry_notes IS NULL
        AND NEW.lat = round(OLD.lat::numeric, 2)::double precision AND NEW.lng = round(OLD.lng::numeric, 2)::double precision THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'gate history can only be redacted, once';
END;
$$;
-- +goose StatementEnd

-- The time a price was set, not when its transaction began: a change that waited on another then
-- sorts after it, so the newest price is always the one in force.
ALTER TABLE price_history ALTER COLUMN created_at SET DEFAULT clock_timestamp();

-- +goose Down
ALTER TABLE price_history ALTER COLUMN created_at SET DEFAULT now();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION gate_history_guard() RETURNS trigger
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
ALTER TABLE gate_history DROP COLUMN changed_by;
