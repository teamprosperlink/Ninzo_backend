-- +goose Up
-- How this customer logs in. One method at a time: see decision 0009. No Google customers exist on
-- any shared database yet, so nothing needs filling in.
ALTER TABLE customers ADD COLUMN sign_in_method text NOT NULL DEFAULT 'phone'
    CHECK (sign_in_method IN ('phone', 'google', 'apple'));

-- +goose Down
ALTER TABLE customers DROP COLUMN sign_in_method;
