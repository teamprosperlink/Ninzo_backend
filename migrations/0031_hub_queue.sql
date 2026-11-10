-- +goose Up
-- The hub's live channel follows hub_state.seq, raised by every change to one of the hub's
-- requests, as rider_state does for riders. Each request has its own seq too, and who accepted it.
CREATE TABLE hub_state (
    hub_id     uuid PRIMARY KEY REFERENCES hubs (id) ON DELETE RESTRICT,
    seq        bigint      NOT NULL DEFAULT 1 CHECK (seq >= 1),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE hub_state ENABLE ROW LEVEL SECURITY;

ALTER TABLE restock_requests
    ADD COLUMN seq         bigint NOT NULL DEFAULT 1 CHECK (seq >= 1),
    ADD COLUMN accepted_by uuid REFERENCES hub_staff (id) ON DELETE RESTRICT;
CREATE INDEX restock_requests_hub_idx ON restock_requests (hub_id, state);

-- +goose Down
DROP INDEX restock_requests_hub_idx;
ALTER TABLE restock_requests DROP COLUMN accepted_by, DROP COLUMN seq;
DROP TABLE hub_state;
