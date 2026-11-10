-- +goose Up
-- What a rider earned (contract: Earnings). One row per handed-over order, one incentive per
-- rider per day, and one row per finished relocation. Each row keeps the amount at the rate of the
-- time, so a later rate change never rewrites past pay. day is the zone's local date.
CREATE TABLE earnings (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id      uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('order', 'incentive', 'relocation')),
    order_id      uuid        REFERENCES orders (id) ON DELETE RESTRICT,
    relocation_id uuid        REFERENCES relocations (id) ON DELETE RESTRICT,
    amount_paise  integer     NOT NULL CHECK (amount_paise >= 0),
    paid_seconds  integer     NOT NULL DEFAULT 0 CHECK (paid_seconds >= 0),
    day           date        NOT NULL,
    earned_at     timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'order') = (order_id IS NOT NULL)),
    CHECK ((kind = 'relocation') = (relocation_id IS NOT NULL)),
    CHECK (kind = 'relocation' OR paid_seconds = 0)
);
CREATE UNIQUE INDEX earnings_one_per_order ON earnings (order_id) WHERE kind = 'order';
CREATE UNIQUE INDEX earnings_one_per_relocation ON earnings (relocation_id) WHERE kind = 'relocation';
CREATE UNIQUE INDEX earnings_one_incentive_a_day ON earnings (rider_id, day) WHERE kind = 'incentive';
CREATE INDEX earnings_rider_day_idx ON earnings (rider_id, day);
ALTER TABLE earnings ENABLE ROW LEVEL SECURITY;

-- Test rates until the admin sets them in the console (decision 0014, answer 1), as in 0026.
ALTER TABLE zone_settings
    ADD COLUMN pay_per_order_paise         integer DEFAULT 4000 CHECK (pay_per_order_paise BETWEEN 0 AND 100000),
    ADD COLUMN incentive_orders            integer DEFAULT 10 CHECK (incentive_orders BETWEEN 1 AND 200),
    ADD COLUMN incentive_paise             integer DEFAULT 20000 CHECK (incentive_paise BETWEEN 0 AND 1000000),
    ADD COLUMN relocation_paise_per_minute integer DEFAULT 200 CHECK (relocation_paise_per_minute BETWEEN 0 AND 10000),
    ADD COLUMN relocation_max_paid_s       integer DEFAULT 1200 CHECK (relocation_max_paid_s BETWEEN 60 AND 3600);
ALTER TABLE zone_settings
    ALTER COLUMN pay_per_order_paise DROP DEFAULT,
    ALTER COLUMN incentive_orders DROP DEFAULT,
    ALTER COLUMN incentive_paise DROP DEFAULT,
    ALTER COLUMN relocation_paise_per_minute DROP DEFAULT,
    ALTER COLUMN relocation_max_paid_s DROP DEFAULT;

-- +goose Down
ALTER TABLE zone_settings DROP COLUMN pay_per_order_paise, DROP COLUMN incentive_orders, DROP COLUMN incentive_paise,
    DROP COLUMN relocation_paise_per_minute, DROP COLUMN relocation_max_paid_s;
DROP TABLE earnings;
