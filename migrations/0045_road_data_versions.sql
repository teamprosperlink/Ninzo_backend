-- +goose Up
-- One row per OSRM road data build (issue 190, geo record W1 step 6): what the data was made from,
-- so every stored road time can be traced to the map, profile and engine that made it. Written by
-- cmd/roaddata from the build.json that tools/osrm/build.sh writes; never changed afterwards.
CREATE TABLE road_data_versions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    source             text        NOT NULL CHECK (char_length(source) BETWEEN 1 AND 500),
    source_sha256      text        NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
    clip_box           text        NOT NULL CHECK (clip_box ~ '^-?[0-9.]+,-?[0-9.]+,-?[0-9.]+,-?[0-9.]+$'),
    osm_extract_sha256 text        NOT NULL CHECK (osm_extract_sha256 ~ '^[0-9a-f]{64}$'),
    -- The OpenStreetMap replication timestamp: how old the map is.
    osm_timestamp      timestamptz NOT NULL,
    profile            text        NOT NULL CHECK (profile ~ '^[a-z0-9_-]{1,40}$'),
    profile_version    text        NOT NULL CHECK (profile_version ~ '^[a-z0-9_.-]{1,40}$'),
    profile_sha256     text        NOT NULL CHECK (profile_sha256 ~ '^[0-9a-f]{64}$'),
    osrm_image         text        NOT NULL CHECK (char_length(osrm_image) BETWEEN 1 AND 300),
    algorithm          text        NOT NULL CHECK (algorithm IN ('mld', 'ch')),
    built_at           timestamptz NOT NULL,
    registered_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT road_data_versions_one_build UNIQUE (osm_extract_sha256, profile_sha256, osrm_image, algorithm)
);
ALTER TABLE road_data_versions ENABLE ROW LEVEL SECURITY;
-- A build is a fact: stored road times will point to it, so no one may change or remove it.
CREATE TRIGGER road_data_versions_append_only BEFORE UPDATE OR DELETE ON road_data_versions
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER road_data_versions_no_truncate BEFORE TRUNCATE ON road_data_versions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TABLE road_data_versions;
