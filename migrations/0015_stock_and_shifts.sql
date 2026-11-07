-- +goose Up
-- The zone a rider works in, set when Ninzo approves them.
ALTER TABLE riders ADD COLUMN zone_id uuid REFERENCES zones (id) ON DELETE RESTRICT;

-- Where scooters wait between orders.
CREATE TABLE staging_nodes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    zone_id    uuid             NOT NULL REFERENCES zones (id) ON DELETE RESTRICT,
    name       text             NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    lat        double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng        double precision NOT NULL CHECK (lng BETWEEN -180 AND 180),
    capacity   integer          NOT NULL CHECK (capacity BETWEEN 1 AND 50),
    is_test    boolean          NOT NULL DEFAULT false,
    created_at timestamptz      NOT NULL DEFAULT now(),
    UNIQUE (zone_id, name)
);
ALTER TABLE staging_nodes ENABLE ROW LEVEL SECURITY;

-- Each scooter, where it waits, and who is riding it.
CREATE TABLE scooters (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code             text        NOT NULL UNIQUE CHECK (length(code) BETWEEN 1 AND 40),
    zone_id          uuid        NOT NULL REFERENCES zones (id) ON DELETE RESTRICT,
    node_id          uuid        REFERENCES staging_nodes (id) ON DELETE RESTRICT,
    current_rider_id uuid        UNIQUE REFERENCES riders (id) ON DELETE RESTRICT,
    status           text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'maintenance')),
    is_test          boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE scooters ENABLE ROW LEVEL SECURITY;

-- Labelled batches of one item. Hubs make them from v0.8.0. Until then they are test data.
CREATE TABLE lots (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code        text        NOT NULL UNIQUE CHECK (length(code) BETWEEN 4 AND 64),
    item_id     uuid        NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    qty         integer     NOT NULL CHECK (qty BETWEEN 1 AND 500),
    expires_at  timestamptz NOT NULL,
    status      text        NOT NULL DEFAULT 'ok' CHECK (status IN ('ok', 'quarantined')),
    temp_status text        NOT NULL DEFAULT 'ok' CHECK (temp_status IN ('ok', 'alert')),
    is_test     boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE lots ENABLE ROW LEVEL SECURITY;

-- What each scooter holds. Section 7 of the data map: 0 <= reserved <= on_hand, always.
CREATE TABLE scooter_stock (
    scooter_id uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    lot_id     uuid        NOT NULL UNIQUE REFERENCES lots (id) ON DELETE RESTRICT,
    on_hand    integer     NOT NULL CHECK (on_hand >= 0),
    reserved   integer     NOT NULL DEFAULT 0 CHECK (reserved >= 0 AND reserved <= on_hand),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scooter_id, lot_id)
);
ALTER TABLE scooter_stock ENABLE ROW LEVEL SECURITY;

-- Every stock change, never edited. scooter_stock can always be rebuilt from it.
CREATE TABLE stock_ledger (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scooter_id     uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    lot_id         uuid        NOT NULL REFERENCES lots (id) ON DELETE RESTRICT,
    delta_on_hand  integer     NOT NULL,
    delta_reserved integer     NOT NULL DEFAULT 0,
    reason         text        NOT NULL CHECK (reason IN ('scan', 'reserve', 'release', 'handover', 'unload', 'correction')),
    actor_type     text        NOT NULL,
    actor_id       uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (delta_on_hand <> 0 OR delta_reserved <> 0)
);
CREATE INDEX stock_ledger_scooter_idx ON stock_ledger (scooter_id, created_at);
ALTER TABLE stock_ledger ENABLE ROW LEVEL SECURITY;
CREATE TRIGGER stock_ledger_append_only BEFORE UPDATE OR DELETE ON stock_ledger
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER stock_ledger_no_truncate BEFORE TRUNCATE ON stock_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- Rider shifts. One open shift per rider, and per scooter.
CREATE TABLE shifts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rider_id   uuid        NOT NULL REFERENCES riders (id) ON DELETE RESTRICT,
    node_id    uuid        NOT NULL REFERENCES staging_nodes (id) ON DELETE RESTRICT,
    scooter_id uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz CHECK (ended_at IS NULL OR ended_at >= started_at)
);
CREATE UNIQUE INDEX shifts_one_open_per_rider ON shifts (rider_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX shifts_one_open_per_scooter ON shifts (scooter_id) WHERE ended_at IS NULL;
ALTER TABLE shifts ENABLE ROW LEVEL SECURITY;

-- Each rider's seq, raised by every change the rider can see, so the app knows it is behind.
CREATE TABLE rider_state (
    rider_id   uuid PRIMARY KEY REFERENCES riders (id) ON DELETE RESTRICT,
    seq        bigint      NOT NULL DEFAULT 0 CHECK (seq >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE rider_state ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE rider_state;
DROP TABLE shifts;
DROP TABLE stock_ledger;
DROP TABLE scooter_stock;
DROP TABLE lots;
DROP TABLE scooters;
DROP TABLE staging_nodes;
ALTER TABLE riders DROP COLUMN zone_id;
