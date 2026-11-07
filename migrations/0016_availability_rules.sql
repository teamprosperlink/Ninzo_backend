-- +goose Up
-- When an item can be sold, in India time. An item with no rules can be sold at any time. A rule with
-- no zone applies everywhere.
CREATE TABLE availability_rules (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id      uuid        NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    zone_id      uuid        REFERENCES zones (id) ON DELETE RESTRICT,
    -- Bit 0 is Monday and bit 6 is Sunday.
    weekdays     smallint    NOT NULL DEFAULT 127 CHECK (weekdays BETWEEN 1 AND 127),
    start_minute smallint    NOT NULL CHECK (start_minute BETWEEN 0 AND 1439),
    end_minute   smallint    NOT NULL CHECK (end_minute BETWEEN 1 AND 1440),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (start_minute < end_minute)
);
CREATE INDEX availability_rules_item_idx ON availability_rules (item_id);
ALTER TABLE availability_rules ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE availability_rules;
