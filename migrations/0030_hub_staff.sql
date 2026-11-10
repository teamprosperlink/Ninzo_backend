-- +goose Up
-- Hub staff and hub leads (decision 0014: phone and OTP). Each belongs to one hub. A person may also
-- be a rider or a customer: those are separate profiles on the same person.
CREATE TABLE hub_staff (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id  uuid        NOT NULL UNIQUE REFERENCES people (id) ON DELETE RESTRICT,
    hub_id     uuid        NOT NULL REFERENCES hubs (id) ON DELETE RESTRICT,
    role       text        NOT NULL CHECK (role IN ('hub_staff', 'hub_lead')),
    status     text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    name       text        CHECK (length(name) BETWEEN 1 AND 80),
    is_test    boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX hub_staff_hub_idx ON hub_staff (hub_id);
ALTER TABLE hub_staff ENABLE ROW LEVEL SECURITY;

-- Staff sign in, hold sessions and register devices like the other apps' users.
ALTER TABLE otp_requests DROP CONSTRAINT otp_requests_actor_type_check,
    ADD CONSTRAINT otp_requests_actor_type_check CHECK (actor_type IN ('customer', 'rider', 'staff'));
ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_actor_type_check,
    ADD CONSTRAINT refresh_tokens_actor_type_check CHECK (actor_type IN ('customer', 'rider', 'staff'));
ALTER TABLE devices DROP CONSTRAINT devices_actor_type_check,
    ADD CONSTRAINT devices_actor_type_check CHECK (actor_type IN ('customer', 'rider', 'staff'));

-- +goose Down
DELETE FROM devices WHERE actor_type = 'staff';
UPDATE refresh_tokens SET replaced_by = NULL WHERE actor_type = 'staff';
DELETE FROM refresh_tokens WHERE actor_type = 'staff';
DELETE FROM otp_requests WHERE actor_type = 'staff';
ALTER TABLE devices DROP CONSTRAINT devices_actor_type_check,
    ADD CONSTRAINT devices_actor_type_check CHECK (actor_type IN ('customer', 'rider'));
ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_actor_type_check,
    ADD CONSTRAINT refresh_tokens_actor_type_check CHECK (actor_type IN ('customer', 'rider'));
ALTER TABLE otp_requests DROP CONSTRAINT otp_requests_actor_type_check,
    ADD CONSTRAINT otp_requests_actor_type_check CHECK (actor_type IN ('customer', 'rider'));
DROP TABLE hub_staff;
