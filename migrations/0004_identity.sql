-- +goose Up
CREATE TABLE customers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone      text        NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    name       text        CHECK (length(name) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE customers ENABLE ROW LEVEL SECURITY;

-- zone_id is added in v0.3.0, when zones exists.
CREATE TABLE riders (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone      text        NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    name       text        NOT NULL CHECK (length(name) >= 1),
    status     text        NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE riders ENABLE ROW LEVEL SECURITY;

CREATE TABLE rider_verifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id   uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    status     text        NOT NULL CHECK (status IN ('pending', 'verified', 'failed', 'expired')),
    method     text        NOT NULL CHECK (method IN ('digilocker', 'manual_upload')),
    reason     text,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rider_verifications_rider_idx ON rider_verifications (rider_id, created_at DESC);
ALTER TABLE rider_verifications ENABLE ROW LEVEL SECURITY;

-- Only a hash is stored. The length check refuses a plain code by mistake.
CREATE TABLE otp_requests (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_type  text        NOT NULL CHECK (actor_type IN ('customer', 'rider')),
    phone       text        NOT NULL CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    code_hash   text        NOT NULL CHECK (length(code_hash) >= 32),
    attempts    integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX otp_requests_phone_idx ON otp_requests (actor_type, phone, created_at DESC);
ALTER TABLE otp_requests ENABLE ROW LEVEL SECURITY;

-- Only a hash is stored. chain_id links each token to the ones it replaced, so reuse can
-- revoke them all. See Sign-in in contracts/README.md.
CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_type  text        NOT NULL CHECK (actor_type IN ('customer', 'rider')),
    actor_id    uuid        NOT NULL,
    chain_id    uuid        NOT NULL,
    token_hash  text        NOT NULL UNIQUE CHECK (length(token_hash) >= 32),
    replaced_by uuid,
    used_at     timestamptz,
    revoked_at  timestamptz,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT refresh_tokens_same_chain_key UNIQUE (id, actor_type, actor_id, chain_id),
    CONSTRAINT refresh_tokens_successor_same_chain FOREIGN KEY (replaced_by, actor_type, actor_id, chain_id)
        REFERENCES refresh_tokens (id, actor_type, actor_id, chain_id) ON DELETE SET NULL (replaced_by),
    CONSTRAINT refresh_tokens_used_when_replaced CHECK (replaced_by IS NULL OR used_at IS NOT NULL)
);
CREATE UNIQUE INDEX refresh_tokens_one_predecessor_idx ON refresh_tokens (replaced_by) WHERE replaced_by IS NOT NULL;
CREATE INDEX refresh_tokens_chain_idx ON refresh_tokens (chain_id);
CREATE INDEX refresh_tokens_actor_idx ON refresh_tokens (actor_type, actor_id);
ALTER TABLE refresh_tokens ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE refresh_tokens;
DROP TABLE otp_requests;
DROP TABLE rider_verifications;
DROP TABLE riders;
DROP TABLE customers;
