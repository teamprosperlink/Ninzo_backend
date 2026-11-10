-- +goose Up
-- Take the orders lock first. The background pass updates orders every few seconds, and this
-- migration alters orders and touches every row, so taking the lock late can deadlock with it.
-- Taken first, the migration only waits.
LOCK TABLE orders IN ACCESS EXCLUSIVE MODE;

-- Customer reports (data map: support_tickets, ticket_messages). Text is the customer's own words,
-- kept for support.
CREATE TABLE support_tickets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid        NOT NULL REFERENCES customers (id) ON DELETE RESTRICT,
    order_id    uuid        REFERENCES orders (id) ON DELETE RESTRICT,
    type        text        NOT NULL CHECK (type IN ('missing_item', 'cold_food', 'wrong_item', 'payment', 'other')),
    status      text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX support_tickets_customer_idx ON support_tickets (customer_id, created_at DESC);
ALTER TABLE support_tickets ENABLE ROW LEVEL SECURITY;

CREATE TABLE ticket_messages (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id  uuid        NOT NULL REFERENCES support_tickets (id) ON DELETE RESTRICT,
    sender     text        NOT NULL CHECK (sender IN ('customer', 'support')),
    text       text        NOT NULL CHECK (length(text) BETWEEN 1 AND 2000),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_messages_ticket_idx ON ticket_messages (ticket_id, created_at);
ALTER TABLE ticket_messages ENABLE ROW LEVEL SECURITY;

-- "Notify me" requests: one push when the gate has an offer again, within a day.
CREATE TABLE availability_notifications (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid        NOT NULL REFERENCES customers (id) ON DELETE RESTRICT,
    gate_id     uuid        NOT NULL REFERENCES gates (id) ON DELETE RESTRICT,
    item_id     uuid        REFERENCES menu_items (id) ON DELETE RESTRICT,
    expires_at  timestamptz NOT NULL,
    sent_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX availability_notifications_open_idx ON availability_notifications (expires_at) WHERE sent_at IS NULL;
-- One open request per customer, gate and item. A request with no item stands for any item.
CREATE UNIQUE INDEX availability_notifications_one_open ON availability_notifications
    (customer_id, gate_id, coalesce(item_id, '00000000-0000-0000-0000-000000000000'::uuid)) WHERE sent_at IS NULL;
ALTER TABLE availability_notifications ADD COLUMN checked_at timestamptz;
ALTER TABLE availability_notifications ENABLE ROW LEVEL SECURITY;

-- The last stage a push went out for, so each stage change is pushed once.
ALTER TABLE orders ADD COLUMN notified_seq bigint NOT NULL DEFAULT 0, ADD COLUMN notified_stage text;
-- Orders that exist now are taken as already told, so a deploy sends no burst of old stages.
UPDATE orders SET notified_seq = seq;
CREATE INDEX orders_to_notify_idx ON orders (updated_at) WHERE seq > notified_seq;

-- +goose Down
ALTER TABLE orders DROP COLUMN notified_seq, DROP COLUMN notified_stage;
DROP TABLE availability_notifications;
DROP TABLE ticket_messages;
DROP TABLE support_tickets;
