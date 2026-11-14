-- +goose Up
-- What the rider app's simple calls need (issue 235, part B2): help articles ops write in the console,
-- feedback riders send, shift plans ops make, and the zone settings the app's settings call reads.
-- Nothing here changes an existing row's meaning.

-- Take every lock first, in one fixed order (alphabetical), as 0042, 0044 and 0056 do: the APIs and
-- the background pass write to these tables every few seconds, and doing the steps one after another
-- can deadlock with a transaction that holds one and wants another. console_users, riders and
-- staging_nodes are only referenced, so they only need to stop changing. zone_settings is altered.
LOCK TABLE console_users, riders, staging_nodes IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;

-- The numbers the rider app works by, console settings (decision 0014). Added with a default so every
-- stored version has them (zone_settings is append-only, so no row can be updated), then the default
-- goes, as in 0040 and 0042. The code keeps the same numbers as its fallback for a row written without them.
ALTER TABLE zone_settings
    ADD COLUMN away_from_spot_m             integer DEFAULT 400   CHECK (away_from_spot_m BETWEEN 50 AND 2000),
    ADD COLUMN scooter_capacity_qty         integer DEFAULT 24    CHECK (scooter_capacity_qty BETWEEN 1 AND 200),
    ADD COLUMN min_phone_battery_pct        integer DEFAULT 20    CHECK (min_phone_battery_pct BETWEEN 0 AND 100),
    ADD COLUMN min_scooter_battery_pct      integer DEFAULT 30    CHECK (min_scooter_battery_pct BETWEEN 0 AND 100),
    ADD COLUMN low_scooter_battery_pct      integer DEFAULT 15    CHECK (low_scooter_battery_pct BETWEEN 0 AND 100),
    ADD COLUMN cancel_pay_cap_paise         integer DEFAULT 4000  CHECK (cancel_pay_cap_paise BETWEEN 0 AND 100000),
    ADD COLUMN break_extension_s            integer DEFAULT 900   CHECK (break_extension_s BETWEEN 60 AND 3600),
    ADD COLUMN not_at_gate_after_s          integer DEFAULT 30    CHECK (not_at_gate_after_s BETWEEN 5 AND 300),
    ADD COLUMN shift_ending_soon_before_s   integer DEFAULT 900   CHECK (shift_ending_soon_before_s BETWEEN 60 AND 7200),
    ADD COLUMN stock_request_ask_before_s   integer DEFAULT 3600  CHECK (stock_request_ask_before_s BETWEEN 300 AND 86400),
    ADD COLUMN stock_request_reminder_before_s integer DEFAULT 1800 CHECK (stock_request_reminder_before_s BETWEEN 60 AND 86400),
    ADD COLUMN verification_within_s        integer DEFAULT 86400 CHECK (verification_within_s BETWEEN 3600 AND 604800);
ALTER TABLE zone_settings
    ALTER COLUMN away_from_spot_m DROP DEFAULT,
    ALTER COLUMN scooter_capacity_qty DROP DEFAULT,
    ALTER COLUMN min_phone_battery_pct DROP DEFAULT,
    ALTER COLUMN min_scooter_battery_pct DROP DEFAULT,
    ALTER COLUMN low_scooter_battery_pct DROP DEFAULT,
    ALTER COLUMN cancel_pay_cap_paise DROP DEFAULT,
    ALTER COLUMN break_extension_s DROP DEFAULT,
    ALTER COLUMN not_at_gate_after_s DROP DEFAULT,
    ALTER COLUMN shift_ending_soon_before_s DROP DEFAULT,
    ALTER COLUMN stock_request_ask_before_s DROP DEFAULT,
    ALTER COLUMN stock_request_reminder_before_s DROP DEFAULT,
    ALTER COLUMN verification_within_s DROP DEFAULT;

-- Help articles. Ops write them in the console and nothing is seeded. audience says who reads one:
-- only riders for now. A rider sees the active ones, in sort order.
CREATE TABLE help_articles (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    audience   text        NOT NULL CHECK (audience IN ('rider')),
    slug       text        NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,59}$'),
    question   text        NOT NULL CHECK (char_length(question) BETWEEN 1 AND 200),
    answer     text        NOT NULL CHECK (char_length(answer) BETWEEN 1 AND 4000),
    sort       integer     NOT NULL DEFAULT 100 CHECK (sort BETWEEN 0 AND 10000),
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid        NOT NULL REFERENCES console_users (id) ON DELETE RESTRICT,
    UNIQUE (audience, slug)
);
CREATE INDEX help_articles_read_idx ON help_articles (audience, sort, id) WHERE active;
ALTER TABLE help_articles ENABLE ROW LEVEL SECURITY;

-- What a rider writes to the team. The text is the rider's own words and is never shown to anyone
-- but the console.
CREATE TABLE rider_feedback (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id   uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    message    text        NOT NULL CHECK (char_length(message) BETWEEN 1 AND 2000),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rider_feedback_newest_idx ON rider_feedback (created_at DESC, id DESC);
CREATE INDEX rider_feedback_rider_idx ON rider_feedback (rider_id);
ALTER TABLE rider_feedback ENABLE ROW LEVEL SECURITY;

-- A day's plan for one rider, made by ops. Shifts stay open-ended: nothing starts, ends or forces a
-- shift by it. plan_date is the day in India time. The reply is the rider's answer to the morning
-- stock request; packing and ready_at are the hub's progress, and a reply closes once packing starts.
CREATE TABLE shift_plans (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id    uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    plan_date   date        NOT NULL,
    node_id     uuid        REFERENCES staging_nodes (id) ON DELETE RESTRICT,
    starts_at   timestamptz NOT NULL,
    ends_at     timestamptz NOT NULL,
    stock_reply text        NOT NULL DEFAULT 'pending' CHECK (stock_reply IN ('pending', 'coming', 'not_today')),
    replied_at  timestamptz,
    packing     text        NOT NULL DEFAULT 'not_started' CHECK (packing IN ('not_started', 'packing', 'ready')),
    ready_at    timestamptz,
    created_by  uuid        NOT NULL REFERENCES console_users (id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (rider_id, plan_date),
    CONSTRAINT shift_plans_span CHECK (ends_at > starts_at AND ends_at <= starts_at + interval '24 hours'),
    CONSTRAINT shift_plans_reply_time CHECK ((stock_reply = 'pending') = (replied_at IS NULL)),
    CONSTRAINT shift_plans_ready_only_after_packing CHECK (packing <> 'not_started' OR ready_at IS NULL)
);
CREATE INDEX shift_plans_newest_idx ON shift_plans (created_at DESC, id DESC);
ALTER TABLE shift_plans ENABLE ROW LEVEL SECURITY;

-- +goose Down
LOCK TABLE console_users, riders, staging_nodes IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE zone_settings IN ACCESS EXCLUSIVE MODE;
DROP TABLE shift_plans;
DROP TABLE rider_feedback;
DROP TABLE help_articles;
ALTER TABLE zone_settings
    DROP COLUMN verification_within_s, DROP COLUMN stock_request_reminder_before_s, DROP COLUMN stock_request_ask_before_s,
    DROP COLUMN shift_ending_soon_before_s, DROP COLUMN not_at_gate_after_s, DROP COLUMN break_extension_s,
    DROP COLUMN cancel_pay_cap_paise, DROP COLUMN low_scooter_battery_pct, DROP COLUMN min_scooter_battery_pct,
    DROP COLUMN min_phone_battery_pct, DROP COLUMN scooter_capacity_qty, DROP COLUMN away_from_spot_m;
