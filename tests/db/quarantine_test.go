package db

import (
	"context"
	"testing"
)

// Migration 0059 (issue 235, part B3): the console's quarantine list and an order's timeline ask which
// orders had units from a lot, so reservations has an index on lot_id, and the ledger knows the reason
// 'quarantine'. Going down drops the index and the column.
func TestMigration0059AddsTheLotIndexAndTheLedgerReason(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'reservations' AND indexname = 'reservations_lot_idx' AND indexdef LIKE '%(lot_id)%'").Scan(&n); err != nil || n != 1 {
		t.Errorf("reservations has no index on lot_id: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_constraint WHERE conname = 'stock_ledger_reason_check' AND pg_get_constraintdef(oid) LIKE '%quarantine%'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the stock ledger does not know the reason quarantine: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'lots' AND column_name = 'quarantined_by_rider'").Scan(&n); err != nil || n != 1 {
		t.Errorf("lots has no quarantined_by_rider: %d %v", n, err)
	}
}
