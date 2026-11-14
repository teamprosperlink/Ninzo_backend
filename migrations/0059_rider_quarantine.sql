-- +goose Up
-- A rider can quarantine a lot on their own scooter (issue 235, part B3). The lot keeps the quarantine
-- columns of 0032, which hub staff fill. A rider is not hub staff, so the rider gets a column of their
-- own, and a lot is quarantined by one or the other, never both. The stock ledger gets the reason
-- 'quarantine' for the units that leave the scooter.

-- Take every lock first, in one fixed order (alphabetical), as 0056 and 0058 do: riders is only
-- referenced, so it only needs to stop changing; reservations gets an index; lots and stock_ledger are altered.
LOCK TABLE reservations, riders IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE lots, stock_ledger IN ACCESS EXCLUSIVE MODE;

ALTER TABLE lots
    ADD COLUMN quarantined_by_rider uuid REFERENCES riders (id) ON DELETE RESTRICT,
    ADD CONSTRAINT lots_quarantined_by_one CHECK (quarantined_by IS NULL OR quarantined_by_rider IS NULL);

ALTER TABLE stock_ledger DROP CONSTRAINT stock_ledger_reason_check;
ALTER TABLE stock_ledger ADD CONSTRAINT stock_ledger_reason_check
    CHECK (reason IN ('scan', 'reserve', 'release', 'handover', 'unload', 'correction', 'quarantine'));

-- The console list and the order timeline ask which orders had units from a lot.
CREATE INDEX reservations_lot_idx ON reservations (lot_id);

-- +goose Down
LOCK TABLE reservations, riders IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE lots, stock_ledger IN ACCESS EXCLUSIVE MODE;
-- The ledger never forgets, so rows with the new reason stay. NOT VALID keeps the old rule for new rows only.
ALTER TABLE stock_ledger DROP CONSTRAINT stock_ledger_reason_check;
ALTER TABLE stock_ledger ADD CONSTRAINT stock_ledger_reason_check
    CHECK (reason IN ('scan', 'reserve', 'release', 'handover', 'unload', 'correction')) NOT VALID;
ALTER TABLE lots DROP CONSTRAINT lots_quarantined_by_one, DROP COLUMN quarantined_by_rider;
DROP INDEX reservations_lot_idx;
