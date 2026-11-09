-- +goose Up
-- How long a rider has to answer an offer. Added with a default, so every stored version has it,
-- then the default goes (as for payment_hold_s in 0017).
ALTER TABLE zone_settings ADD COLUMN offer_timeout_s integer DEFAULT 30 CHECK (offer_timeout_s BETWEEN 10 AND 300);
ALTER TABLE zone_settings ALTER COLUMN offer_timeout_s DROP DEFAULT;

-- What the rider who accepted fixes on the order (contracts/README.md, Timer): a later console
-- change never alters it. min_wait_until is counted from payment.
ALTER TABLE orders
    ADD COLUMN rider_id            uuid REFERENCES riders (id) ON DELETE RESTRICT,
    ADD COLUMN min_wait_until      timestamptz,
    ADD COLUMN min_wait_until_seen boolean NOT NULL DEFAULT false,
    ADD COLUMN rider_accepted_at   timestamptz,
    ADD COLUMN packed_at           timestamptz,
    ADD COLUMN timer_length_s      integer CHECK (timer_length_s > 0),
    ADD COLUMN zone_radius_m       integer CHECK (zone_radius_m > 0),
    ADD COLUMN arrival_perimeter_m integer CHECK (arrival_perimeter_m > 0);
CREATE INDEX orders_dispatch_idx ON orders (state, cancel_until) WHERE state IN ('PAYMENT_SECURED', 'NO_SCOOTER', 'RIDER_ACCEPTED');

-- Offers (data map section 1.1). One rider's view of an order. A live offer is one not yet
-- released: open, or accepted and still the rider's to deliver. An order has at most one live
-- offer, and a rider at most one (decision 0011, one order per rider).
CREATE TABLE offers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id          uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    rider_id          uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    scooter_id        uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    state             text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'accepted', 'declined', 'expired', 'taken', 'cancelled')),
    fencing_token     text        NOT NULL UNIQUE,
    expires_at        timestamptz NOT NULL,
    rider_accepted_at timestamptz,
    ended_reason      text,
    final_state       text,
    released_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (state <> 'open' OR released_at IS NULL),
    CHECK (state IN ('open', 'accepted') OR released_at IS NOT NULL),
    CHECK ((state = 'accepted') = (rider_accepted_at IS NOT NULL))
);
CREATE UNIQUE INDEX offers_one_live_per_order ON offers (order_id) WHERE released_at IS NULL;
CREATE UNIQUE INDEX offers_one_live_per_rider ON offers (rider_id) WHERE released_at IS NULL;
CREATE INDEX offers_rider_idx ON offers (rider_id, updated_at DESC);
CREATE INDEX offers_open_idx ON offers (expires_at) WHERE state = 'open';
ALTER TABLE offers ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE offers;
DROP INDEX orders_dispatch_idx;
ALTER TABLE orders DROP COLUMN rider_id, DROP COLUMN min_wait_until, DROP COLUMN min_wait_until_seen, DROP COLUMN rider_accepted_at,
    DROP COLUMN packed_at, DROP COLUMN timer_length_s, DROP COLUMN zone_radius_m, DROP COLUMN arrival_perimeter_m;
ALTER TABLE zone_settings DROP COLUMN offer_timeout_s;
