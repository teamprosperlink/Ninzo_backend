-- +goose Up
-- Append-only. Written in the same transaction as the change it records.
CREATE TABLE audit_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_type  text        NOT NULL,
    actor_id    uuid,
    action      text        NOT NULL,
    entity_type text        NOT NULL,
    entity_id   text        NOT NULL,
    before      jsonb,
    after       jsonb,
    reason      text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_entity_idx ON audit_events (entity_type, entity_id, created_at);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, created_at);
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;

CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TABLE audit_events;
