package shifts

import (
	"math"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
)

// Fields the rider app asked for (issue 158): node_assigned_at, scooter, hub_distance_m, image_url.

func TestAShiftShowsWhenItsNodeWasAssignedAndItsScooter(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-158")
	rider, token := f.rider()
	code, out := f.start(token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	sh := out["shift"].(map[string]any)
	check(t, sh, "Shift")
	if sh["node_assigned_at"] != sh["started_at"] {
		t.Errorf("a new shift is assigned at its start: %v", sh)
	}
	sc, _ := sh["scooter"].(map[string]any)
	if _, ok := sc["battery_pct"]; !ok {
		t.Errorf("battery_pct must be present, as null: %v", sc)
	}
	if sc["id"] != scooter || sc["plate"] != "SC-158" || sc["battery_pct"] != nil {
		t.Errorf("scooter %v", sc)
	}
	// Accepting a move gives the rider the new node (as relocation.AcceptTx does), so the time is
	// the accept, before any arrival, and even if the move later ends without arriving.
	to := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Node B', 12.94, 77.63, 4) RETURNING id::text", f.zone)
	f.exec(`INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, state, expires_at, accepted_at)
		SELECT $1, id, node_id, $2, 'test', 'accepted', now() + interval '5 minutes', now() + interval '1 minute' FROM shifts WHERE rider_id = $1 AND ended_at IS NULL`, rider, to)
	f.exec("UPDATE shifts SET node_id = $2 WHERE rider_id = $1 AND ended_at IS NULL", rider, to)
	want := f.id("SELECT to_char(max(accepted_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US') FROM relocations WHERE rider_id = $1", rider)
	code, out = f.send("POST", "/riders/me/shifts/end", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	ended := out["shift"].(map[string]any)
	got, err := time.Parse(time.RFC3339Nano, ended["node_assigned_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if got.UTC().Format("2006-01-02T15:04:05.000000") != want {
		t.Errorf("node_assigned_at %v, want the accept %s", ended["node_assigned_at"], want)
	}
}

func TestNodesShowTheDistanceToTheNearestActiveHub(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	_, out := f.send("GET", "/nodes?zone_id="+f.zone, token, "")
	check(t, out["nodes"].([]any)[0].(map[string]any), "Node")
	if v, ok := out["nodes"].([]any)[0].(map[string]any)["hub_distance_m"]; !ok || v != nil {
		t.Errorf("no hub: %v", out)
	}
	// A paused hub does not count, even when it is the closest.
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng, status) VALUES ($1, 'Paused', 12.9301, 77.6201, 'paused')", f.zone)
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Far', 12.95, 77.65)", f.zone)
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Near', 12.93, 77.63)", f.zone)
	// A hub in another zone does not count, even when it is closer.
	other := f.id("INSERT INTO zones (name, city, status) VALUES ('Other zone', 'Bengaluru', 'active') RETURNING id::text")
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Next door', 12.9301, 77.6201)", other)
	_, out = f.send("GET", "/nodes?zone_id="+f.zone, token, "")
	want := math.Round(geo.Metres(12.93, 77.62, 12.93, 77.63))
	if got := out["nodes"].([]any)[0].(map[string]any)["hub_distance_m"]; got != want {
		t.Errorf("hub_distance_m %v, want %v", got, want)
	}
}

func TestNearestHubIsRoundedAndNullWithNoHub(t *testing.T) {
	if nearestHub(12.93, 77.62, nil) != nil {
		t.Error("no hubs must give nil")
	}
	hubs := [][2]float64{{12.95, 77.65}, {12.93, 77.63}}
	got := nearestHub(12.93, 77.62, hubs)
	if got == nil || *got != int(math.Round(geo.Metres(12.93, 77.62, 12.93, 77.63))) || *got < 1000 || *got > 1200 {
		t.Errorf("%v", got)
	}
}

func TestStockShowsThePhotoOfTheItem(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	f.lot("LOT-0158", 5, 48*time.Hour)
	_, token := f.rider()
	f.start(token)
	if code, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"LOT-0158","scanned_at":"2026-11-07T10:00:00Z"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	lot := func() map[string]any {
		_, out := f.send("GET", "/riders/me/stock", token, "")
		l := out["lots"].([]any)[0].(map[string]any)
		check(t, l, "StockLot")
		return l
	}
	if v, ok := lot()["image_url"]; !ok || v != nil {
		t.Errorf("an item with no photo: %v", v)
	}
	f.exec("UPDATE menu_items SET photo_url = 'https://example.com/chai.jpg' WHERE id = $1", f.item)
	if got := lot()["image_url"]; got != "https://example.com/chai.jpg" {
		t.Errorf("image_url %v", got)
	}
}
