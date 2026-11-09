-- +goose Up
-- Remedies: what Ninzo owes a customer for a missed timer (a credit) or missing food (a refund).
-- One of each type per order. amount_paise is null while the console has not set the credit.
CREATE TABLE remedies (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id     uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    type         text        NOT NULL CHECK (type IN ('credit', 'refund', 'replacement')),
    reason       text        NOT NULL,
    amount_paise integer     CHECK (amount_paise > 0),
    status       text        NOT NULL DEFAULT 'owed' CHECK (status IN ('owed', 'applied', 'cancelled')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (order_id, type)
);
ALTER TABLE remedies ENABLE ROW LEVEL SECURITY;

-- A late ping whose device time falls inside the timer, at the gate, may mean the rider arrived in
-- time with no signal. The remedy waits for a decision, at most 10 minutes (contracts/README.md,
-- Signal lost at the gate). One open review per order.
CREATE TABLE arrival_reviews (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    uuid             NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    lat         double precision NOT NULL,
    lng         double precision NOT NULL,
    device_time timestamptz      NOT NULL,
    due_at      timestamptz      NOT NULL,
    status      text             NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'arrived_in_time', 'late', 'timed_out')),
    decided_at  timestamptz,
    created_at  timestamptz      NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX arrival_reviews_one_open ON arrival_reviews (order_id) WHERE status = 'open';
ALTER TABLE arrival_reviews ENABLE ROW LEVEL SECURITY;

-- A longer ETA offered at no_scooter, from a scooter that will be free later. Made only when the
-- time is measured (contract: EtaOffer), so nothing makes one until road times exist.
CREATE TABLE eta_offers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    scooter_id  uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    eta_s       integer     NOT NULL CHECK (eta_s > 0),
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX eta_offers_order_idx ON eta_offers (order_id, created_at DESC);
ALTER TABLE eta_offers ENABLE ROW LEVEL SECURITY;

-- After a longer ETA is accepted, the search starts again: offers made before it no longer count
-- as tried.
ALTER TABLE orders ADD COLUMN search_from timestamptz;

-- The last ping that arrived on time. A flush of queued pings says the phone is back, but not
-- where the rider was, so a credit counts only on-time pings.
ALTER TABLE orders ADD COLUMN last_live_ping_at timestamptz;

-- +goose Down
ALTER TABLE orders DROP COLUMN search_from, DROP COLUMN last_live_ping_at;
DROP TABLE eta_offers;
DROP TABLE arrival_reviews;
DROP TABLE remedies;
