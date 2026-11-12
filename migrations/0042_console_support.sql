-- +goose Up
-- The console's support actions (issue 147): tickets worked and closed, refunds, replacements and
-- credits. Who did each is kept, so the order timeline can say.

-- Take every lock first, in one fixed order, as 0024 does for orders. The background pass and the
-- API write to these tables every few seconds, and altering them one after another can deadlock
-- with a transaction that holds one and wants another. Taken together first, this migration only
-- waits. ticket_messages is altered last, and its parent support_tickets is locked here too.
LOCK TABLE zone_settings, refunds, remedies, support_tickets, ticket_messages IN ACCESS EXCLUSIVE MODE;

-- The refund that needs a second admin: over this amount in paise, cumulative per order. Added
-- with a default so every stored version has it (zone_settings is append-only, so no row can be
-- updated), then the default goes, as in 0034 and 0040.
ALTER TABLE zone_settings ADD COLUMN refund_approval_paise integer DEFAULT 20000 CHECK (refund_approval_paise BETWEEN 0 AND 10000000);
ALTER TABLE zone_settings ALTER COLUMN refund_approval_paise DROP DEFAULT;

-- Who asked for a refund (null when the system made it), and the reason staff gave. reason_code
-- stays the customer's code; a console refund's is support_refund_<n>, as (payment_id, reason_code)
-- is unique and an order can have several.
ALTER TABLE refunds
    ADD COLUMN requested_by uuid REFERENCES console_users (id) ON DELETE RESTRICT,
    ADD COLUMN note         text CHECK (note IS NULL OR char_length(note) BETWEEN 3 AND 500),
    ADD CONSTRAINT refunds_console_note CHECK ((requested_by IS NULL) = (note IS NULL));

-- Who owes a remedy (null when the system made it) and, for a replacement, its lines.
ALTER TABLE remedies
    ADD COLUMN created_by uuid REFERENCES console_users (id) ON DELETE RESTRICT,
    ADD COLUMN items      jsonb,
    ADD CONSTRAINT remedies_items_only_replacement CHECK (items IS NULL OR (type = 'replacement' AND jsonb_typeof(items) = 'array'));

-- A ticket closed from the console keeps when, by whom and why. Statuses stay open and resolved.
ALTER TABLE support_tickets
    ADD COLUMN closed_at    timestamptz,
    ADD COLUMN closed_by    uuid REFERENCES console_users (id) ON DELETE RESTRICT,
    ADD COLUMN close_reason text CHECK (close_reason IS NULL OR char_length(close_reason) BETWEEN 3 AND 500),
    ADD CONSTRAINT support_tickets_closed_together CHECK ((closed_at IS NULL) = (close_reason IS NULL));
CREATE INDEX support_tickets_created_idx ON support_tickets (created_at DESC, id DESC);

-- Who wrote a support reply.
ALTER TABLE ticket_messages ADD COLUMN author_id uuid REFERENCES console_users (id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE ticket_messages DROP COLUMN author_id;
DROP INDEX support_tickets_created_idx;
ALTER TABLE support_tickets DROP CONSTRAINT support_tickets_closed_together,
    DROP COLUMN closed_at, DROP COLUMN closed_by, DROP COLUMN close_reason;
ALTER TABLE remedies DROP CONSTRAINT remedies_items_only_replacement, DROP COLUMN created_by, DROP COLUMN items;
ALTER TABLE refunds DROP CONSTRAINT refunds_console_note, DROP COLUMN requested_by, DROP COLUMN note;
ALTER TABLE zone_settings DROP COLUMN refund_approval_paise;
