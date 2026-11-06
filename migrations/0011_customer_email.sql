-- +goose Up
-- Contact details only. It never signs anyone in, so it is not unique and not verified.
ALTER TABLE customers ADD COLUMN email text CHECK (length(email) BETWEEN 3 AND 254);

-- +goose Down
ALTER TABLE customers DROP COLUMN email;
