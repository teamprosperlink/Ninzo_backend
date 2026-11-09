-- +goose Up
-- The background pass raises seq once when the 5-second cancel window closes, and once when the
-- wait deadline passes, so apps watching seq see cancellable change (contracts/README.md).
ALTER TABLE orders
    ADD COLUMN cancel_until_seen  boolean NOT NULL DEFAULT false,
    ADD COLUMN wait_deadline_seen boolean NOT NULL DEFAULT false;
CREATE INDEX orders_paid_clock_idx ON orders (cancel_until, wait_deadline)
    WHERE state IN ('PAYMENT_SECURED', 'OFFERED', 'RIDER_ACCEPTED', 'PACKED');

-- +goose Down
DROP INDEX orders_paid_clock_idx;
ALTER TABLE orders DROP COLUMN cancel_until_seen, DROP COLUMN wait_deadline_seen;
