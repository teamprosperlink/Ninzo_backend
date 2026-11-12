-- +goose Up
-- Live rider data (issue 207, geo record W3 steps 1 and 2).
--
-- Each new ping keeps the cell of the pilot grid it falls in (internal/hexgrid, grid blr76), or
-- none for a point beyond the grid's reach. Older pings keep none: location_pings is append-only.
ALTER TABLE location_pings ADD COLUMN cell text CHECK (cell ~ '^[a-z][a-z0-9]{0,15}:-?[0-9]{1,10}:-?[0-9]{1,10}$');

-- Where each rider is now: one row per rider, written in the same transaction as the ping, and
-- only by a ping newer by device time, so a late or out-of-order ping never moves a rider back.
CREATE TABLE rider_positions (
    rider_id    uuid PRIMARY KEY REFERENCES riders (id) ON DELETE RESTRICT,
    lat         double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng         double precision NOT NULL CHECK (lng BETWEEN -180 AND 180),
    cell        text CHECK (cell ~ '^[a-z][a-z0-9]{0,15}:-?[0-9]{1,10}:-?[0-9]{1,10}$'),
    speed_mps   double precision NOT NULL CHECK (speed_mps >= 0),
    accuracy_m  double precision NOT NULL CHECK (accuracy_m >= 0),
    device_time timestamptz      NOT NULL,
    received_at timestamptz      NOT NULL
);
ALTER TABLE rider_positions ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE rider_positions;
ALTER TABLE location_pings DROP COLUMN cell;
