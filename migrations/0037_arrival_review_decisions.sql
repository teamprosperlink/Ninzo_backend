-- +goose Up
-- Who settled an arrival review in the console, and why (decision 0015). Null for one that timed
-- out on its own.
ALTER TABLE arrival_reviews
    ADD COLUMN decided_by      uuid REFERENCES console_users (id) ON DELETE RESTRICT,
    ADD COLUMN decision_reason text CHECK (length(decision_reason) BETWEEN 3 AND 500),
    ADD CHECK (decided_by IS NULL OR status IN ('arrived_in_time', 'late'));

-- +goose Down
ALTER TABLE arrival_reviews DROP COLUMN decided_by, DROP COLUMN decision_reason;
