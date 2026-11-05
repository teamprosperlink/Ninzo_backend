-- +goose Up
-- One person, many roles. Rules and reasons: docs/engineering/authentication_rules.md.
-- The phone is empty until verified, and is moved here from customers and riders. Those
-- tables must be empty when this runs: the NOT NULL columns below make it fail otherwise.
CREATE TABLE people (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone      text        UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    status     text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'blocked')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE people ENABLE ROW LEVEL SECURITY;

-- Provider accounts linked to a person. Email is kept only for display, and never used to merge.
CREATE TABLE person_identities (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id  uuid        NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    provider   text        NOT NULL CHECK (provider IN ('google', 'apple')),
    subject    text        NOT NULL CHECK (length(subject) >= 1),
    email      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT person_identities_one_owner UNIQUE (provider, subject),
    CONSTRAINT person_identities_one_per_provider UNIQUE (person_id, provider)
);
ALTER TABLE person_identities ENABLE ROW LEVEL SECURITY;

ALTER TABLE customers
    DROP COLUMN phone,
    ADD COLUMN person_id uuid NOT NULL UNIQUE REFERENCES people (id) ON DELETE RESTRICT;

-- A new rider is an applicant, so a row made by mistake can never start a shift.
ALTER TABLE riders
    DROP COLUMN phone,
    ADD COLUMN person_id uuid NOT NULL UNIQUE REFERENCES people (id) ON DELETE RESTRICT,
    ADD COLUMN rider_code text UNIQUE CHECK (rider_code ~ '^[A-Z0-9]{4,16}$'),
    ALTER COLUMN name DROP NOT NULL,
    ALTER COLUMN status SET DEFAULT 'applicant',
    ADD CONSTRAINT riders_status_check CHECK (status IN ('applicant', 'active', 'suspended'));

-- Only a hash is stored. The length check refuses a plain password by mistake.
CREATE TABLE rider_credentials (
    rider_id        uuid PRIMARY KEY REFERENCES riders (id) ON DELETE CASCADE,
    password_hash   text        NOT NULL CHECK (length(password_hash) >= 32),
    failed_attempts integer     NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    locked_until    timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE rider_credentials ENABLE ROW LEVEL SECURITY;

-- A code works only for the purpose it was sent for, and verify_phone codes only in the
-- session that asked for them.
ALTER TABLE otp_requests
    ADD COLUMN requested_by uuid,
    ADD COLUMN purpose text NOT NULL CHECK (purpose IN ('sign_in', 'verify_phone', 'password_reset', 'rider_application')),
    ADD CONSTRAINT otp_requests_verify_phone_has_requester CHECK (purpose <> 'verify_phone' OR requested_by IS NOT NULL);

-- +goose Down
ALTER TABLE otp_requests DROP COLUMN purpose, DROP COLUMN requested_by;
DROP TABLE rider_credentials;
ALTER TABLE riders
    DROP CONSTRAINT riders_status_check,
    ALTER COLUMN status SET DEFAULT 'active',
    ALTER COLUMN name SET NOT NULL,
    DROP COLUMN rider_code,
    DROP COLUMN person_id,
    ADD COLUMN phone text NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$');
ALTER TABLE customers
    DROP COLUMN person_id,
    ADD COLUMN phone text NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$');
DROP TABLE person_identities;
DROP TABLE people;
