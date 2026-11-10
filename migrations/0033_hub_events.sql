-- +goose Up
-- The final result of a hub's queued action, by event_id (data map: hub_events), as rider_events
-- does for riders. A repeated event_id returns it with no second effect.
CREATE TABLE hub_events (
    staff_id    uuid        NOT NULL REFERENCES hub_staff (id) ON DELETE RESTRICT,
    event_id    text        NOT NULL CHECK (event_id ~ '^[A-Za-z0-9_-]{8,128}$'),
    type        text        NOT NULL CHECK (type IN ('accept', 'scan', 'unscan', 'shortfall', 'ready', 'quarantine', 'unknown')),
    accepted    boolean     NOT NULL,
    code        text,
    message     text,
    device_time timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (staff_id, event_id),
    CHECK (accepted = (code IS NULL))
);
ALTER TABLE hub_events ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE hub_events;
