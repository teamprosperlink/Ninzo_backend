-- +goose Up
-- Console staff (decision 0015): an ID and a password hash, then an authenticator. The TOTP secret is
-- kept encrypted; the key is derived from the service's signing key, never stored here.
CREATE TABLE console_users (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    login_id             text        NOT NULL UNIQUE CHECK (login_id ~ '^[a-z0-9._-]{3,80}$'),
    name                 text        NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    role                 text        NOT NULL CHECK (role IN ('admin', 'ops', 'support', 'hub_manager')),
    hub_id               uuid REFERENCES hubs (id) ON DELETE RESTRICT,
    password_hash        text        NOT NULL,
    must_change_password boolean     NOT NULL DEFAULT true,
    totp_secret          bytea,
    totp_enrolled_at     timestamptz,
    totp_last_step       bigint      NOT NULL DEFAULT 0,
    failed_logins        integer     NOT NULL DEFAULT 0 CHECK (failed_logins >= 0),
    failed_since         timestamptz,
    locked_until         timestamptz,
    active               boolean     NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CHECK ((role = 'hub_manager') = (hub_id IS NOT NULL))
);
ALTER TABLE console_users ENABLE ROW LEVEL SECURITY;

-- The step between the password and the authenticator code. Only its hash is kept.
CREATE TABLE console_challenges (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  text        NOT NULL UNIQUE,
    user_id     uuid        NOT NULL REFERENCES console_users (id) ON DELETE RESTRICT,
    attempts    integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX console_challenges_user_idx ON console_challenges (user_id, created_at DESC);
ALTER TABLE console_challenges ENABLE ROW LEVEL SECURITY;

-- Changes waiting for a second person (decision 0015).
CREATE TABLE approvals (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            text        NOT NULL CHECK (kind IN ('settings', 'kill_switch', 'flag', 'refund')),
    target          text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'failed', 'cancelled', 'lapsed')),
    summary         text        NOT NULL,
    change          jsonb       NOT NULL,
    requested_by    uuid        NOT NULL REFERENCES console_users (id) ON DELETE RESTRICT,
    request_reason  text        NOT NULL CHECK (length(request_reason) BETWEEN 3 AND 500),
    requested_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    decided_by      uuid REFERENCES console_users (id) ON DELETE RESTRICT,
    decided_at      timestamptz,
    decision_reason text CHECK (length(decision_reason) <= 500),
    failure_code    text,
    CHECK ((status = 'pending') = (decided_at IS NULL))
);
-- One change waiting per thing at a time.
CREATE UNIQUE INDEX approvals_one_pending ON approvals (kind, target) WHERE status = 'pending';
CREATE INDEX approvals_list_idx ON approvals (status, requested_at DESC);
ALTER TABLE approvals ENABLE ROW LEVEL SECURITY;

CREATE TABLE feature_flags (
    key         text PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_]{1,60}$'),
    enabled     boolean     NOT NULL DEFAULT false,
    description text        NOT NULL CHECK (length(description) BETWEEN 1 AND 200),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE feature_flags ENABLE ROW LEVEL SECURITY;
INSERT INTO feature_flags (key, enabled, description) VALUES
    ('eta_offers', false, 'Offer a longer ETA at no_scooter, once road times are measured');

-- Console sessions use the same refresh tokens as the apps.
ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_actor_type_check,
    ADD CONSTRAINT refresh_tokens_actor_type_check CHECK (actor_type IN ('customer', 'rider', 'staff', 'console'));

-- +goose Down
UPDATE refresh_tokens SET replaced_by = NULL WHERE actor_type = 'console';
DELETE FROM refresh_tokens WHERE actor_type = 'console';
ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_actor_type_check,
    ADD CONSTRAINT refresh_tokens_actor_type_check CHECK (actor_type IN ('customer', 'rider', 'staff'));
DROP TABLE feature_flags;
DROP TABLE approvals;
DROP TABLE console_challenges;
DROP TABLE console_users;
