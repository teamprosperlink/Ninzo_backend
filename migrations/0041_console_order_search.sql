-- +goose Up
-- Indexes for the console's order search and timeline (issue 146). The list is newest first with
-- optional zone and state filters, and a search by the last four phone digits of a customer or
-- rider. Small tables here, so plain CREATE INDEX (a brief write lock on orders) is fine.
CREATE INDEX orders_created_idx ON orders (created_at DESC, id DESC);
CREATE INDEX orders_zone_created_idx ON orders (zone_id, created_at DESC, id DESC);
CREATE INDEX orders_rider_created_idx ON orders (rider_id, created_at DESC, id DESC) WHERE rider_id IS NOT NULL;
-- The phone-suffix search. The expression is the same one the query uses.
CREATE INDEX people_phone_last4_idx ON people (right(phone, 4)) WHERE phone IS NOT NULL;
-- The timeline reads every source by order (payments and remedies already have one).
CREATE INDEX offers_order_created_idx ON offers (order_id, created_at);
CREATE INDEX support_tickets_order_idx ON support_tickets (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX arrival_reviews_order_idx ON arrival_reviews (order_id, created_at);
CREATE INDEX refunds_order_idx ON refunds (order_id, created_at);

-- +goose Down
DROP INDEX refunds_order_idx;
DROP INDEX arrival_reviews_order_idx;
DROP INDEX support_tickets_order_idx;
DROP INDEX offers_order_created_idx;
DROP INDEX people_phone_last4_idx;
DROP INDEX orders_rider_created_idx;
DROP INDEX orders_zone_created_idx;
DROP INDEX orders_created_idx;
