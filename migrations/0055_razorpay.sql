-- +goose Up
-- Razorpay (issue 237). A payment keeps Razorpay's payment id (pay_…), which a refund needs. A
-- refund keeps Razorpay's refund id (rfnd_…) and its send attempts: refunds are written as pending
-- inside order transactions, and a background pass sends them to Razorpay afterwards. Webhook
-- events are kept by Razorpay's event id, so a repeated delivery has no second effect.
ALTER TABLE payments ADD COLUMN provider_payment_id text UNIQUE CHECK (provider_payment_id ~ '^[A-Za-z0-9_]{1,64}$');

ALTER TABLE refunds ADD COLUMN provider_refund_id text UNIQUE CHECK (provider_refund_id ~ '^[A-Za-z0-9_]{1,64}$');
ALTER TABLE refunds ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0);
ALTER TABLE refunds ADD COLUMN last_error text CHECK (length(last_error) <= 300);
ALTER TABLE refunds ADD COLUMN next_try_at timestamptz;
CREATE INDEX refunds_to_send_idx ON refunds (created_at) WHERE status = 'pending' AND provider_refund_id IS NULL;

CREATE TABLE webhook_events (
    event_id    text        PRIMARY KEY CHECK (length(event_id) BETWEEN 1 AND 100),
    event       text        NOT NULL CHECK (length(event) BETWEEN 1 AND 100),
    received_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE webhook_events ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE webhook_events;
DROP INDEX refunds_to_send_idx;
ALTER TABLE refunds DROP COLUMN next_try_at, DROP COLUMN last_error, DROP COLUMN attempts, DROP COLUMN provider_refund_id;
ALTER TABLE payments DROP COLUMN provider_payment_id;
