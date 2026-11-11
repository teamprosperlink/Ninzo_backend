-- +goose Up
-- Console staff sign in with an email address (owner, 2026-11-11). The older ID form stays valid,
-- so users already made keep working. Both are kept lower case by the code.
ALTER TABLE console_users DROP CONSTRAINT console_users_login_id_check;
ALTER TABLE console_users ADD CONSTRAINT console_users_login_id_check
    CHECK (length(login_id) <= 254 AND login_id ~ '^([a-z0-9._-]{3,80}|[a-z0-9._%+-]{1,64}@[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,63})$');

-- +goose Down
ALTER TABLE console_users DROP CONSTRAINT console_users_login_id_check;
ALTER TABLE console_users ADD CONSTRAINT console_users_login_id_check CHECK (login_id ~ '^[a-z0-9._-]{3,80}$');
