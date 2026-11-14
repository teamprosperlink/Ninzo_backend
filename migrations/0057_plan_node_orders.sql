-- +goose Up
-- Forecast orders per plan entry (issue 249). For each entry of node_ids, the forecast paid orders
-- in that half-hour of the cells that entry is the first, in pick order, to cover. Relocation by
-- plan reports their sum for the target node as orders_waiting, instead of the node's recent
-- orders (usually 0 for a plan move). Builds made before this have none (null).
-- Plans are append-only rows, but adding a nullable column rewrites nothing and fires no trigger.
ALTER TABLE placement_plans
    ADD COLUMN node_orders double precision[] CHECK (node_orders IS NULL OR (cardinality(node_orders) = n AND 0 <= ALL (node_orders) AND NOT ('NaN' = ANY (node_orders)) AND NOT ('Infinity' = ANY (node_orders))));

-- +goose Down
ALTER TABLE placement_plans DROP COLUMN node_orders;
