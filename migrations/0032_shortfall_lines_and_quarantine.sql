-- +goose Up
-- A shortfall lists every short line (contract: HubShortfall). One shortfall per request, as before.
CREATE TABLE shortfall_lines (
    request_id  uuid    NOT NULL REFERENCES shortfalls (request_id) ON DELETE RESTRICT,
    item_id     uuid    NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    qty_missing integer NOT NULL CHECK (qty_missing BETWEEN 1 AND 500),
    PRIMARY KEY (request_id, item_id)
);
ALTER TABLE shortfall_lines ENABLE ROW LEVEL SECURITY;
-- Shortfalls the simulator recorded before: their lines are what was filled below what was asked.
INSERT INTO shortfall_lines (request_id, item_id, qty_missing)
SELECT s.request_id, w.item_id, least(w.qty - coalesce(w.filled_qty, 0), 500)
FROM shortfalls s JOIN restock_lines w ON w.request_id = s.request_id
WHERE coalesce(w.filled_qty, 0) < w.qty;

ALTER TABLE shortfalls
    ADD COLUMN reason      text CHECK (length(reason) <= 200),
    ADD COLUMN recorded_by uuid REFERENCES hub_staff (id) ON DELETE RESTRICT;

-- When the cassette was sealed, for its label.
ALTER TABLE restock_requests ADD COLUMN sealed_at timestamptz;

-- Who quarantined a lot, when and why.
ALTER TABLE lots
    ADD COLUMN quarantine_reason text CHECK (quarantine_reason IN ('temperature', 'damaged', 'expired', 'contamination', 'other')),
    ADD COLUMN quarantine_note   text CHECK (length(quarantine_note) <= 500),
    ADD COLUMN quarantined_by    uuid REFERENCES hub_staff (id) ON DELETE RESTRICT,
    ADD COLUMN quarantined_at    timestamptz;

-- +goose Down
ALTER TABLE lots DROP COLUMN quarantined_at, DROP COLUMN quarantined_by, DROP COLUMN quarantine_note, DROP COLUMN quarantine_reason;
ALTER TABLE restock_requests DROP COLUMN sealed_at;
ALTER TABLE shortfalls DROP COLUMN recorded_by, DROP COLUMN reason;
DROP TABLE shortfall_lines;
