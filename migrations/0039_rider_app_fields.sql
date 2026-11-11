-- +goose Up
-- Fields the rider app asked for (issue 158). Orders get a short number people can read out; it is
-- made by the database, so existing orders are numbered too (in the table's own order, which
-- for the test orders on staging is not always the order they were made).
-- Shown as a letter and four digits (see orders.OrderNumber). Relocations keep the demand count
-- behind the suggestion, so the app can say how many orders are waiting. Older rows have none.
ALTER TABLE orders ADD COLUMN order_no bigint GENERATED ALWAYS AS IDENTITY (START WITH 1001);
CREATE UNIQUE INDEX orders_order_no_idx ON orders (order_no);
ALTER TABLE relocations ADD COLUMN orders_waiting integer CHECK (orders_waiting IS NULL OR orders_waiting >= 0);

-- +goose Down
ALTER TABLE relocations DROP COLUMN orders_waiting;
DROP INDEX orders_order_no_idx;
ALTER TABLE orders DROP COLUMN order_no;
