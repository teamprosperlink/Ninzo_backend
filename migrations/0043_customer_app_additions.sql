-- +goose Up
-- The customer app's additions (issue 178 part B): the veg flag on menu items, a gate's landmark and
-- type, and the customer's push settings.

-- Take every lock first, in one fixed order, as 0024 and 0042 do. The API reads and writes these
-- tables all day, and altering them one after another can deadlock with a transaction that holds one
-- and wants another. gate_history is locked after its parent gates.
LOCK TABLE menu_items, gates, gate_history IN ACCESS EXCLUSIVE MODE;

-- Set by the admin in the console. An item is not vegetarian until someone says so.
ALTER TABLE menu_items ADD COLUMN is_veg boolean NOT NULL DEFAULT false;

-- A place the rider can spot from the road, and what kind of gate it is. Both are optional. The type
-- is an open list of short lowercase words (main_gate, side_gate, tower, house, office, other).
ALTER TABLE gates
    ADD COLUMN landmark  text CHECK (landmark IS NULL OR char_length(landmark) BETWEEN 1 AND 80),
    ADD COLUMN gate_type text CHECK (gate_type IS NULL OR (char_length(gate_type) <= 30 AND gate_type ~ '^[a-z][a-z_]*$'));
ALTER TABLE gate_history
    ADD COLUMN landmark  text,
    ADD COLUMN gate_type text;

-- A landmark says where someone lives, so erasing a customer clears it with the notes. The guard of
-- 0036 gains that one condition and is otherwise the same.
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
        AND NEW.label = '(removed)' AND NEW.entry_notes IS NULL AND NEW.landmark IS NULL
        AND NEW.gate_type IS NOT DISTINCT FROM OLD.gate_type
        AND NEW.lat = round(OLD.lat::numeric, 2)::double precision AND NEW.lng = round(OLD.lng::numeric, 2)::double precision THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'gate history can only be redacted, once';
END;
$$;
-- +goose StatementEnd

-- What a customer wants pushed. A customer with no row has the defaults: offers and news off,
-- support replies on. order_updates is always on and is not stored.
CREATE TABLE customer_notification_settings (
    customer_id     uuid PRIMARY KEY REFERENCES customers (id) ON DELETE CASCADE,
    offers_and_news boolean     NOT NULL DEFAULT false,
    support_replies boolean     NOT NULL DEFAULT true,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE customer_notification_settings ENABLE ROW LEVEL SECURITY;

-- +goose Down
LOCK TABLE menu_items, gates, gate_history IN ACCESS EXCLUSIVE MODE;
DROP TABLE customer_notification_settings;
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
ALTER TABLE gate_history DROP COLUMN landmark, DROP COLUMN gate_type;
ALTER TABLE gates DROP COLUMN landmark, DROP COLUMN gate_type;
ALTER TABLE menu_items DROP COLUMN is_veg;
