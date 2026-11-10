-- +goose Up
-- Restock sites (data map: hubs). Entered by the admin in the console; test data until then
-- (decision 0014).
CREATE TABLE hubs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    zone_id    uuid             NOT NULL REFERENCES zones (id) ON DELETE RESTRICT,
    name       text             NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    lat        double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng        double precision NOT NULL CHECK (lng BETWEEN -180 AND 180),
    status     text             NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused')),
    is_test    boolean          NOT NULL DEFAULT false,
    created_at timestamptz      NOT NULL DEFAULT now(),
    updated_at timestamptz      NOT NULL DEFAULT now(),
    UNIQUE (zone_id, name)
);
ALTER TABLE hubs ENABLE ROW LEVEL SECURITY;

-- A lot waiting at a hub. Once scanned onto a scooter it is in scooter_stock instead.
ALTER TABLE lots ADD COLUMN hub_id uuid REFERENCES hubs (id) ON DELETE RESTRICT;
CREATE INDEX lots_hub_idx ON lots (hub_id, item_id, expires_at) WHERE hub_id IS NOT NULL;

-- Restock requests and their states (contract: RestockRequest). A suggestion is a request in
-- state suggested. A scooter has at most one open request, and at most one suggestion.
CREATE TABLE restock_requests (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id         uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    scooter_id       uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    hub_id           uuid        NOT NULL REFERENCES hubs (id) ON DELETE RESTRICT,
    source           text        NOT NULL CHECK (source IN ('suggested', 'manual')),
    state            text        NOT NULL CHECK (state IN ('suggested', 'confirmed', 'hub_accepted', 'packing', 'ready', 'picked_up',
        'received', 'closed', 'expired', 'cancelled', 'partial')),
    slot_at          timestamptz,
    arrive_by        timestamptz,
    expires_at       timestamptz,
    reason           text CHECK (length(reason) <= 200),
    state_changed_at timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX restock_one_open_per_scooter ON restock_requests (scooter_id)
    WHERE state IN ('confirmed', 'hub_accepted', 'packing', 'ready', 'picked_up', 'partial');
CREATE UNIQUE INDEX restock_one_suggestion_per_scooter ON restock_requests (scooter_id) WHERE state = 'suggested';
CREATE INDEX restock_requests_rider_idx ON restock_requests (rider_id, created_at DESC);
CREATE INDEX restock_requests_moving_idx ON restock_requests (state, state_changed_at) WHERE state IN ('confirmed', 'hub_accepted', 'packing');
ALTER TABLE restock_requests ENABLE ROW LEVEL SECURITY;

-- Items asked for, and what the hub packed. filled_qty below qty is a short fill.
CREATE TABLE restock_lines (
    request_id uuid    NOT NULL REFERENCES restock_requests (id) ON DELETE RESTRICT,
    item_id    uuid    NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    qty        integer NOT NULL CHECK (qty BETWEEN 1 AND 500),
    filled_qty integer CHECK (filled_qty >= 0),
    PRIMARY KEY (request_id, item_id)
);
ALTER TABLE restock_lines ENABLE ROW LEVEL SECURITY;

-- The lots packed into the cassette. A lot is in one cassette at a time; a cancelled or expired
-- request releases its lots, so they can be packed again.
CREATE TABLE restock_lots (
    request_id  uuid        NOT NULL REFERENCES restock_requests (id) ON DELETE RESTRICT,
    lot_id      uuid        NOT NULL REFERENCES lots (id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now(),
    released_at timestamptz,
    PRIMARY KEY (request_id, lot_id)
);
CREATE UNIQUE INDEX restock_lots_one_cassette ON restock_lots (lot_id) WHERE released_at IS NULL;
ALTER TABLE restock_lots ENABLE ROW LEVEL SECURITY;

-- What a hub could not fill, and what it offers instead. One per request.
CREATE TABLE shortfalls (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id         uuid        NOT NULL UNIQUE REFERENCES restock_requests (id) ON DELETE RESTRICT,
    action             text        NOT NULL CHECK (action IN ('partial', 'substitute', 'other_hub')),
    substitute_item_id uuid        REFERENCES menu_items (id) ON DELETE RESTRICT,
    other_hub_id       uuid        REFERENCES hubs (id) ON DELETE RESTRICT,
    status             text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'accepted', 'refused')),
    created_at         timestamptz NOT NULL DEFAULT now(),
    decided_at         timestamptz,
    CHECK ((action = 'substitute') = (substitute_item_id IS NOT NULL)),
    CHECK ((action = 'other_hub') = (other_hub_id IS NOT NULL))
);
ALTER TABLE shortfalls ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE shortfalls;
DROP TABLE restock_lots;
DROP TABLE restock_lines;
DROP TABLE restock_requests;
DROP INDEX lots_hub_idx;
ALTER TABLE lots DROP COLUMN hub_id;
DROP TABLE hubs;
