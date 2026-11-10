-- +goose Up
-- A request moved to another hub after a shortfall points at the request it follows.
ALTER TABLE restock_requests ADD COLUMN follow_of uuid REFERENCES restock_requests (id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX restock_requests_follow_of ON restock_requests (follow_of) WHERE follow_of IS NOT NULL;

-- +goose Down
DROP INDEX restock_requests_follow_of;
ALTER TABLE restock_requests DROP COLUMN follow_of;
