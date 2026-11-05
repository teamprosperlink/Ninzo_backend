-- +goose Up
-- Stored replies for repeated writes. See Idempotency in contracts/README.md.
-- A row with no response is a request still running, which holds the key until locked_until.
CREATE TABLE idempotency_keys (
    actor_id     uuid        NOT NULL,
    method       text        NOT NULL,
    path         text        NOT NULL,
    key          text        NOT NULL CHECK (length(key) BETWEEN 8 AND 128),
    request_hash text        NOT NULL,
    status       integer     CHECK (status BETWEEN 200 AND 299),
    response     jsonb,
    locked_until timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id, method, path, key),
    CONSTRAINT idempotency_keys_reply_complete CHECK ((status IS NULL) = (response IS NULL)),
    CONSTRAINT idempotency_keys_lock_expires CHECK (status IS NOT NULL OR locked_until IS NOT NULL)
);
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;

-- Events saved in the same transaction as the change that caused them (decision 0003).
CREATE TABLE outbox_events (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic           text        NOT NULL,
    payload         jsonb       NOT NULL,
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_events_due_idx ON outbox_events (next_attempt_at) WHERE delivered_at IS NULL;
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;

-- Due work for cmd/worker. Rows, not memory, so a restart loses nothing.
-- entity_id is null for work with no single record, such as daily partitions.
-- failed_at means retries ran out, and ops must look.
CREATE TABLE scheduled_tasks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,
    entity_id    uuid,
    payload      jsonb       NOT NULL DEFAULT '{}',
    run_at       timestamptz NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until timestamptz,
    last_error   text,
    done_at      timestamptz,
    failed_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- One open task of a kind per order or other record, so scheduling twice has one effect.
CREATE UNIQUE INDEX scheduled_tasks_one_open_idx ON scheduled_tasks (kind, entity_id) NULLS NOT DISTINCT
    WHERE done_at IS NULL AND failed_at IS NULL;
CREATE INDEX scheduled_tasks_due_idx ON scheduled_tasks (run_at) WHERE done_at IS NULL AND failed_at IS NULL;
ALTER TABLE scheduled_tasks ENABLE ROW LEVEL SECURITY;

-- Refuses any change to a row, for append-only tables.
-- +goose StatementBegin
CREATE FUNCTION forbid_change() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION forbid_change();
DROP TABLE scheduled_tasks;
DROP TABLE outbox_events;
DROP TABLE idempotency_keys;
