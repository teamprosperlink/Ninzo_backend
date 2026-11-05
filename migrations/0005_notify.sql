-- +goose Up
-- A push token belongs to one device, so registering it again moves it to the new owner.
CREATE TABLE devices (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_type text        NOT NULL CHECK (actor_type IN ('customer', 'rider')),
    actor_id   uuid        NOT NULL,
    platform   text        NOT NULL CHECK (platform IN ('android', 'ios')),
    push_token text        NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX devices_actor_idx ON devices (actor_type, actor_id);
ALTER TABLE devices ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE devices;
