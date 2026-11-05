-- +goose Up
-- Row Level Security does not stop TRUNCATE, and Supabase grants its API roles everything on
-- new public objects by default. Services are the only path to the data (decision 0005), so
-- these roles get nothing, whatever the dashboard's Data API setting says. The roles exist
-- only on Supabase, so elsewhere this does nothing.
-- +goose StatementBegin
DO $$
DECLARE
    api_role text;
BEGIN
    FOREACH api_role IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = api_role) THEN
            EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', api_role);
            EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', api_role);
            EXECUTE format('REVOKE ALL ON ALL ROUTINES IN SCHEMA public FROM %I', api_role);
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %I', api_role);
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %I', api_role);
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON ROUTINES FROM %I', api_role);
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION forbid_change() FROM PUBLIC;

-- +goose Down
-- Nothing: going down must never hand these roles access.
