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
    ELSE
        -- A non-superuser, like postgres on Supabase, cannot remove powers from a role, so a
        -- role that has too many stops the migration below.
        ALTER ROLE ninzo_service LOGIN BYPASSRLS;
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

-- GRANT only warns when it lacks the right, and a role that already existed could differ, so
-- check the result and stop if it is wrong. Migration history stays out of the role's reach.
-- +goose StatementBegin
DO $$
DECLARE
    history_sequence text := pg_get_serial_sequence('goose_db_version', 'id');
BEGIN
    IF history_sequence IS NOT NULL THEN
        EXECUTE format('REVOKE ALL ON SEQUENCE %s FROM ninzo_service', history_sequence);
    END IF;
    IF NOT has_schema_privilege('ninzo_service', 'public', 'USAGE') THEN
        RAISE EXCEPTION 'ninzo_service has no USAGE on schema public';
    END IF;
    IF NOT has_schema_privilege('ninzo_service', 'extensions', 'USAGE') THEN
        RAISE EXCEPTION 'ninzo_service has no USAGE on schema extensions';
    END IF;
    IF NOT (SELECT rolbypassrls FROM pg_roles WHERE rolname = 'ninzo_service') THEN
        RAISE EXCEPTION 'ninzo_service cannot bypass Row Level Security';
    END IF;
    IF (SELECT rolsuper OR rolcreatedb OR rolcreaterole FROM pg_roles WHERE rolname = 'ninzo_service') THEN
        RAISE EXCEPTION 'ninzo_service has more power than it should';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- The role stays, because it belongs to the whole server. It keeps no access here.
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM ninzo_service;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ninzo_service;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM ninzo_service;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM ninzo_service;
REVOKE ALL ON SCHEMA extensions FROM ninzo_service;
REVOKE ALL ON SCHEMA public FROM ninzo_service;
