-- +goose Up
-- PostGIS lives in the extensions schema (decision 0005). Supabase already has that schema.
CREATE SCHEMA IF NOT EXISTS extensions;
-- +goose StatementBegin
DO $$
DECLARE
    found_in text;
BEGIN
    SELECT extnamespace::regnamespace::text INTO found_in FROM pg_extension WHERE extname = 'postgis';
    IF found_in IS NOT NULL AND found_in <> 'extensions' THEN
        RAISE EXCEPTION 'PostGIS is installed in schema %, and must be in extensions', found_in;
    END IF;
END;
$$;
-- +goose StatementEnd
CREATE EXTENSION IF NOT EXISTS postgis SCHEMA extensions;

-- Goose's own table is in public too, so it gets Row Level Security like every other table.
ALTER TABLE goose_db_version ENABLE ROW LEVEL SECURITY;

-- +goose Down
-- Nothing: PostGIS may have been there first, and other things may use it.
