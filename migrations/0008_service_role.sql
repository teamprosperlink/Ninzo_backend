-- +goose Up
-- The login the services use. Owner decision on #25. It can read and write rows, but cannot
-- change tables, truncate, or touch migration history, so a bug or a leaked password can do
-- less harm. postgres stays for migrations only. It bypasses Row Level Security, because
-- services are the only path to the data (decision 0005).
--
-- No password is set here, so it cannot sign in until the owner sets one. The role belongs to
-- the whole server, so it may already exist.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'ninzo_service') THEN
        CREATE ROLE ninzo_service LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
    END IF;
END;
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO ninzo_service;
GRANT USAGE ON SCHEMA extensions TO ninzo_service;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ninzo_service;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ninzo_service;
REVOKE ALL ON goose_db_version FROM ninzo_service;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ninzo_service;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO ninzo_service;

-- +goose Down
-- The role stays, because it belongs to the whole server. It keeps no access here.
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM ninzo_service;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ninzo_service;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM ninzo_service;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM ninzo_service;
REVOKE ALL ON SCHEMA extensions FROM ninzo_service;
REVOKE ALL ON SCHEMA public FROM ninzo_service;
