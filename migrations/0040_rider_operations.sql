-- +goose Up
-- The rider app's operations (issues 157 and 159): the area a rider asks for, breaks inside a shift,
-- the phone a shift was started on, the notification list, hand counts of stock, the reason a
-- document was rejected, and when the rider said they were waiting at the gate.

ALTER TABLE riders ADD COLUMN preferred_area text CHECK (preferred_area IS NULL OR char_length(preferred_area) <= 80);

-- A phone removed with removeRiderDevice leaves the shift with no device, never a dangling id.
-- returned_items is what stood on the scooter when the shift ended (null while it is open, and for
-- shifts that ended before this column), so a summary shows it however the next shift loads it.
ALTER TABLE shifts
    ADD COLUMN device_id        uuid REFERENCES devices (id) ON DELETE SET NULL,
    ADD COLUMN break_started_at timestamptz,
    ADD COLUMN break_ends_at    timestamptz,
    ADD COLUMN returned_items   jsonb,
    ADD CONSTRAINT shifts_break_together CHECK ((break_started_at IS NULL) = (break_ends_at IS NULL)),
    ADD CONSTRAINT shifts_break_order CHECK (break_ends_at IS NULL OR break_ends_at > break_started_at),
    ADD CONSTRAINT shifts_break_only_while_open CHECK (ended_at IS NULL OR break_started_at IS NULL);

-- Every break of every shift, so break time adds up. One break is open at a time per shift.
CREATE TABLE shift_breaks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    shift_id   uuid        NOT NULL REFERENCES shifts (id) ON DELETE RESTRICT,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz,
    ended_by   text CHECK (ended_by IN ('rider', 'timeout', 'shift_end')),
    CHECK ((ended_at IS NULL) = (ended_by IS NULL)),
    CHECK (ended_at IS NULL OR ended_at >= started_at)
);
CREATE UNIQUE INDEX shift_breaks_one_open ON shift_breaks (shift_id) WHERE ended_at IS NULL;
CREATE INDEX shift_breaks_shift_idx ON shift_breaks (shift_id, started_at);
ALTER TABLE shift_breaks ENABLE ROW LEVEL SECURITY;

-- The longest break, a console setting (decision 0014). Added with a default so every stored version
-- has it (zone_settings is append-only, so no row can be updated), then the default goes, as in 0034.
ALTER TABLE zone_settings ADD COLUMN max_break_s integer DEFAULT 1800 CHECK (max_break_s BETWEEN 300 AND 7200);
ALTER TABLE zone_settings ALTER COLUMN max_break_s DROP DEFAULT;

-- Set when the rider says they are waiting at the gate; the first time is kept.
ALTER TABLE orders ADD COLUMN rider_waiting_since timestamptz;

-- What was pushed to a rider, kept 30 days. It never holds a customer's phone or address.
CREATE TABLE rider_notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id   uuid        NOT NULL REFERENCES riders (id) ON DELETE CASCADE,
    kind       text        NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 40),
    title      text        NOT NULL,
    body       text        NOT NULL,
    order_id   uuid        REFERENCES orders (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rider_notifications_rider_idx ON rider_notifications (rider_id, created_at DESC, id DESC);
CREATE INDEX rider_notifications_created_idx ON rider_notifications (created_at);
ALTER TABLE rider_notifications ENABLE ROW LEVEL SECURITY;

-- A hand count when the scanner fails: what the rider counted against what the server expected on
-- the scooter. It does not change the stock; ops read the mismatches.
CREATE TABLE stock_counts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id   uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    shift_id   uuid        NOT NULL REFERENCES shifts (id) ON DELETE RESTRICT,
    scooter_id uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    reason     text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 200),
    mismatches integer     NOT NULL CHECK (mismatches >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stock_counts_rider_idx ON stock_counts (rider_id, created_at DESC);
ALTER TABLE stock_counts ENABLE ROW LEVEL SECURITY;

CREATE TABLE stock_count_lines (
    stock_count_id uuid    NOT NULL REFERENCES stock_counts (id) ON DELETE RESTRICT,
    item_id        uuid    NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    expected       integer NOT NULL CHECK (expected >= 0),
    counted        integer NOT NULL CHECK (counted >= 0),
    PRIMARY KEY (stock_count_id, item_id)
);
ALTER TABLE stock_count_lines ENABLE ROW LEVEL SECURITY;

-- The status of each document of a verification, and why one was rejected. Empty until ops record
-- them.
CREATE TABLE verification_documents (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    verification_id uuid        NOT NULL REFERENCES rider_verifications (id) ON DELETE RESTRICT,
    type            text        NOT NULL CHECK (char_length(type) BETWEEN 1 AND 40),
    status          text        NOT NULL CHECK (status IN ('pending', 'verified', 'rejected')),
    reason          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (verification_id, type),
    CHECK (status = 'rejected' OR reason IS NULL)
);
ALTER TABLE verification_documents ENABLE ROW LEVEL SECURITY;

-- For shift history and the shift summary (#159 review).
CREATE INDEX shifts_rider_started_idx ON shifts (rider_id, started_at DESC, id DESC);
CREATE INDEX orders_rider_handed_over_idx ON orders (rider_id, handed_over_at) WHERE handed_over_at IS NOT NULL;

-- +goose Down
DROP INDEX orders_rider_handed_over_idx;
DROP INDEX shifts_rider_started_idx;
DROP TABLE verification_documents;
DROP TABLE stock_count_lines;
DROP TABLE stock_counts;
DROP TABLE rider_notifications;
ALTER TABLE orders DROP COLUMN rider_waiting_since;
ALTER TABLE zone_settings DROP COLUMN max_break_s;
DROP TABLE shift_breaks;
ALTER TABLE shifts
    DROP CONSTRAINT shifts_break_only_while_open,
    DROP CONSTRAINT shifts_break_order,
    DROP CONSTRAINT shifts_break_together,
    DROP COLUMN returned_items,
    DROP COLUMN break_ends_at,
    DROP COLUMN break_started_at,
    DROP COLUMN device_id;
ALTER TABLE riders DROP COLUMN preferred_area;
