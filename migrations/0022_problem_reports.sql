-- +goose Up
-- What riders report, and what the system raises for support (data map: problem_reports). There
-- is no penalty for a report. A rider's note is free text, kept for support.
CREATE TABLE problem_reports (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id   uuid        REFERENCES riders (id) ON DELETE RESTRICT,
    order_id   uuid        REFERENCES orders (id) ON DELETE RESTRICT,
    type       text        NOT NULL CHECK (type IN ('breakdown', 'accident', 'gate_closed', 'customer_absent', 'unsafe_route', 'signal_lost')),
    source     text        NOT NULL CHECK (source IN ('rider', 'system')),
    note       text        CHECK (length(note) <= 1000),
    outcome    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX problem_reports_order_idx ON problem_reports (order_id) WHERE order_id IS NOT NULL;
-- The system raises a lost signal once per order and rider.
CREATE UNIQUE INDEX problem_reports_one_signal_alert ON problem_reports (order_id, rider_id) WHERE source = 'system' AND type = 'signal_lost';
ALTER TABLE problem_reports ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE problem_reports;
