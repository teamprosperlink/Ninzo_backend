-- +goose Up
-- The final result of a rider's queued action, by event_id (data map: rider_events). A repeated
-- event_id returns it with no second effect. Results that may change on a retry are not kept.
CREATE TABLE rider_events (
    rider_id    uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    event_id    text        NOT NULL CHECK (event_id ~ '^[A-Za-z0-9_-]{8,128}$'),
    type        text        NOT NULL,
    accepted    boolean     NOT NULL,
    code        text,
    message     text,
    device_time timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (rider_id, event_id),
    CHECK (accepted = (code IS NULL))
);
ALTER TABLE rider_events ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE rider_events;
