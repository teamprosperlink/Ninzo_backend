package db

import (
	"context"
	"database/sql"
	"testing"
)

// Migration 0049 (#199) must leave zone override rows exactly as they were: a value written into
// them would override the global setting for good. Run 0048, add a zone override as ops would,
// then run 0049 and check.
func TestTierSettingsLeaveZoneOverridesAlone(t *testing.T) {
	db, p := freshDB(t)
	ctx := context.Background()
	if _, err := p.UpTo(ctx, 48); err != nil {
		t.Fatal(err)
	}
	var zone string
	if err := db.QueryRow(`INSERT INTO zones (name, city, is_test) VALUES ('Override zone (test)', 'Bengaluru', true) RETURNING id::text`).Scan(&zone); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO zone_settings (zone_id, version, timer_length_s, reason, created_by)
		VALUES ('`+zone+`', 1, 100, 'an override made before 0049', 'ops')`)
	if _, err := p.UpTo(ctx, 49); err != nil {
		t.Fatal(err)
	}
	var margin, outer sql.NullInt64
	var timer int
	if err := db.QueryRow(`SELECT timer_zone_margin_s, outer_ring_budget_s, timer_length_s FROM zone_settings WHERE zone_id = $1`, zone).Scan(&margin, &outer, &timer); err != nil {
		t.Fatal(err)
	}
	if margin.Valid || outer.Valid || timer != 100 {
		t.Fatalf("the zone row changed: margin %v, outer %v, timer %d", margin, outer, timer)
	}
	var gm, gout int
	if err := db.QueryRow(`SELECT timer_zone_margin_s, outer_ring_budget_s FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1`).Scan(&gm, &gout); err != nil {
		t.Fatal(err)
	}
	if gm != 15 || gout != 300 {
		t.Fatalf("global: margin %d, outer %d", gm, gout)
	}
	// The append-only rule is back in force.
	if _, err := db.Exec(`UPDATE zone_settings SET timer_zone_margin_s = 20 WHERE zone_id IS NULL`); err == nil {
		t.Fatal("zone_settings can be updated after 0049")
	}
}
