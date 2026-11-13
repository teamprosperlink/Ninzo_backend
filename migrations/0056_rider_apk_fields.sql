-- +goose Up
-- Fields the finished rider app reads and sends (issue 235, part B1): the rider's email and picked
-- area, a hub manager's phone, and on a shift the time the rider went online, the planned end and
-- the Before you start items ticked. Nothing here changes an existing column.

-- Take every lock first, in one fixed order (alphabetical), as 0042 and 0044 do: the APIs and the
-- background pass write to these tables every few seconds, and altering them one after another can
-- deadlock with a transaction that holds one and wants another.
LOCK TABLE hubs, riders, shifts IN ACCESS EXCLUSIVE MODE;

-- Contact only: the email never signs anyone in. The area is a place the rider picked. Its name is
-- kept equal to preferred_area, so the two can never disagree, and the four parts are all there or all null.
ALTER TABLE riders
    ADD COLUMN email     text CHECK (email IS NULL OR char_length(email) BETWEEN 3 AND 254),
    ADD COLUMN area_name text,
    ADD COLUMN area_city text,
    ADD COLUMN area_lat  double precision,
    ADD COLUMN area_lng  double precision,
    ADD CONSTRAINT riders_area_together CHECK (
        (area_name IS NULL) = (area_city IS NULL) AND (area_name IS NULL) = (area_lat IS NULL) AND (area_name IS NULL) = (area_lng IS NULL)),
    ADD CONSTRAINT riders_area_valid CHECK (area_name IS NULL OR (
        char_length(area_name) BETWEEN 1 AND 80 AND char_length(area_city) <= 80
        AND area_lat BETWEEN -90 AND 90 AND area_lng BETWEEN -180 AND 180)),
    ADD CONSTRAINT riders_area_follows_preferred CHECK (area_name IS NULL OR (preferred_area IS NOT NULL AND area_name = preferred_area));

-- The number for Call hub manager. Null until the hub has one. Set by hand until the console takes it.
ALTER TABLE hubs
    ADD COLUMN manager_phone text CHECK (manager_phone IS NULL OR manager_phone ~ '^\+[1-9][0-9]{7,14}$');

-- online_at is set once (goOnline, part B2). planned_end_at is a record only: nothing ends a shift
-- by it. checklist is the codes the rider ticked at startShift, each once.
ALTER TABLE shifts
    ADD COLUMN online_at       timestamptz,
    ADD COLUMN planned_end_at  timestamptz,
    ADD COLUMN checklist       text[] NOT NULL DEFAULT '{}' CHECK (cardinality(checklist) <= 20);

-- +goose Down
LOCK TABLE hubs, riders, shifts IN ACCESS EXCLUSIVE MODE;
ALTER TABLE shifts DROP COLUMN checklist, DROP COLUMN planned_end_at, DROP COLUMN online_at;
ALTER TABLE hubs DROP COLUMN manager_phone;
ALTER TABLE riders DROP CONSTRAINT riders_area_follows_preferred, DROP CONSTRAINT riders_area_valid, DROP CONSTRAINT riders_area_together,
    DROP COLUMN area_lng, DROP COLUMN area_lat, DROP COLUMN area_city, DROP COLUMN area_name, DROP COLUMN email;
