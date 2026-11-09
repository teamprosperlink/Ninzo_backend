-- +goose Up
-- The zone timer and handover (decision 0007, contracts/README.md Timer). Timer start and stop are
-- written once, by conditional updates. handover_otp is kept readable, because the customer must
-- see it, and is cleared once the order is handed over or has ended (data map section 1.1).
ALTER TABLE orders
    ADD COLUMN timer_started_at timestamptz,
    ADD COLUMN timer_stopped_at timestamptz,
    ADD COLUMN timer_missed     boolean     NOT NULL DEFAULT false,
    ADD COLUMN arrived_at       timestamptz,
    ADD COLUMN arrival_source   text CHECK (arrival_source IN ('gps', 'handover')),
    ADD COLUMN handover_otp     text CHECK (handover_otp ~ '^[0-9]{4}$'),
    ADD COLUMN otp_attempts     integer     NOT NULL DEFAULT 0 CHECK (otp_attempts >= 0),
    ADD COLUMN otp_released_at  timestamptz,
    ADD COLUMN handed_over_at   timestamptz,
    ADD COLUMN audit_flag       text,
    ADD COLUMN zone_hits        integer     NOT NULL DEFAULT 0 CHECK (zone_hits >= 0),
    ADD COLUMN perimeter_hits   integer     NOT NULL DEFAULT 0 CHECK (perimeter_hits >= 0),
    ADD COLUMN last_ping_at     timestamptz,
    ADD COLUMN last_device_time timestamptz,
    ADD CHECK (timer_stopped_at IS NULL OR timer_started_at IS NOT NULL);
CREATE INDEX orders_timer_idx ON orders (timer_started_at) WHERE state = 'TIMER_RUNNING';

-- What the rider handed over. Below qty is a partial handover.
ALTER TABLE order_items ADD COLUMN qty_handed integer CHECK (qty_handed >= 0 AND qty_handed <= qty);

-- Every ping, kept as evidence and never changed (data map: location_pings). applied says whether
-- it moved the order's zone counts. Daily partitions come later, see known downsides.
CREATE TABLE location_pings (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rider_id    uuid             NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    order_id    uuid             REFERENCES orders (id) ON DELETE RESTRICT,
    lat         double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng         double precision NOT NULL CHECK (lng BETWEEN -180 AND 180),
    speed_mps   double precision NOT NULL CHECK (speed_mps >= 0),
    accuracy_m  double precision NOT NULL CHECK (accuracy_m >= 0),
    device_time timestamptz      NOT NULL,
    received_at timestamptz      NOT NULL DEFAULT now(),
    applied     boolean          NOT NULL,
    note        text
);
CREATE INDEX location_pings_rider_idx ON location_pings (rider_id, received_at DESC);
CREATE INDEX location_pings_order_idx ON location_pings (order_id, received_at) WHERE order_id IS NOT NULL;
ALTER TABLE location_pings ENABLE ROW LEVEL SECURITY;
CREATE TRIGGER location_pings_append_only BEFORE UPDATE OR DELETE ON location_pings
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER location_pings_no_truncate BEFORE TRUNCATE ON location_pings
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TABLE location_pings;
ALTER TABLE order_items DROP COLUMN qty_handed;
DROP INDEX orders_timer_idx;
ALTER TABLE orders DROP COLUMN timer_started_at, DROP COLUMN timer_stopped_at, DROP COLUMN timer_missed, DROP COLUMN arrived_at,
    DROP COLUMN arrival_source, DROP COLUMN handover_otp, DROP COLUMN otp_attempts, DROP COLUMN otp_released_at, DROP COLUMN handed_over_at,
    DROP COLUMN audit_flag, DROP COLUMN zone_hits, DROP COLUMN perimeter_hits, DROP COLUMN last_ping_at, DROP COLUMN last_device_time;
