package restock

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
)

// Issue 158: hub places, distance to another hub, and ready_in_s.

func TestARequestShowsTheHubPlaceAndTimeToItsSlot(t *testing.T) {
	f := newFixture(t)
	id := f.request(f.chai, 1)
	view := func() map[string]any {
		t.Helper()
		code, out := f.get("/restock/"+id, f.token)
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
		r := out["request"].(map[string]any)
		check(t, r, "RestockRequest")
		return r
	}
	r := view()
	hub := r["hub"].(map[string]any)
	check(t, hub, "HubRef")
	var lat, lng float64
	if err := f.pool.QueryRow(context.Background(), "SELECT lat, lng FROM hubs WHERE id = $1", f.hub).Scan(&lat, &lng); err != nil {
		t.Fatal(err)
	}
	if hub["lat"] != lat || hub["lng"] != lng {
		t.Errorf("hub %v, want %v %v", hub, lat, lng)
	}
	if v, ok := r["ready_in_s"]; !ok || v != nil {
		t.Errorf("without a slot ready_in_s must be null: %v", r["ready_in_s"])
	}
	f.exec("UPDATE restock_requests SET slot_at = now() + interval '10 minutes' WHERE id = $1", id)
	got, _ := view()["ready_in_s"].(float64)
	if got < 595 || got > 600 {
		t.Errorf("ready_in_s %v, want about 600", got)
	}
	f.exec("UPDATE restock_requests SET slot_at = now() - interval '5 minutes' WHERE id = $1", id)
	if v := view()["ready_in_s"]; v != float64(0) {
		t.Errorf("a slot in the past stops at 0: %v", v)
	}
}

func TestOptionsAndSuggestionsCarryTheHubPlace(t *testing.T) {
	f := newFixture(t)
	_, out := f.get("/restock/options", f.token)
	for _, h := range out["hubs"].([]any) {
		hub := h.(map[string]any)
		check(t, hub, "HubRef")
		var lat, lng float64
		if err := f.pool.QueryRow(context.Background(), "SELECT lat, lng FROM hubs WHERE id = $1", hub["id"]).Scan(&lat, &lng); err != nil {
			t.Fatal(err)
		}
		if hub["lat"] != lat || hub["lng"] != lng {
			t.Errorf("hub place %v, want %v, %v", hub, lat, lng)
		}
	}
	if len(out["hubs"].([]any)) == 0 {
		t.Error("no hubs to check")
	}
}

func TestAnotherHubShowsItsDistanceFromTheRidersNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.hub, "LOT-D1", f.chai, 5, time.Hour)
	f.hubLot(f.other, "LOT-D2", f.chai, 10, time.Hour)
	id := f.request(f.chai, 8)
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(ctx, now.Add(after))
	}
	_, out := f.get("/restock/"+id, f.token)
	sf, _ := out["request"].(map[string]any)["shortfall"].(map[string]any)
	if sf == nil {
		t.Fatalf("%v", out)
	}
	other := sf["other_hub"].(map[string]any)
	check(t, other, "HubRef")
	var hubLat, hubLng, nodeLat, nodeLng float64
	if err := f.pool.QueryRow(ctx, "SELECT lat, lng FROM hubs WHERE id = $1", f.other).Scan(&hubLat, &hubLng); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT n.lat, n.lng FROM shifts sh JOIN staging_nodes n ON n.id = sh.node_id
		WHERE sh.rider_id = $1 AND sh.ended_at IS NULL`, f.rider).Scan(&nodeLat, &nodeLng); err != nil {
		t.Fatal(err)
	}
	if other["lat"] != hubLat || other["lng"] != hubLng {
		t.Errorf("other_hub place %v", other)
	}
	if want := math.Round(geo.Metres(nodeLat, nodeLng, hubLat, hubLng)); other["distance_m"] != want {
		t.Errorf("distance_m %v, want %v", other["distance_m"], want)
	}
	// With no open shift the distance cannot be worked out, so it is null.
	f.exec("UPDATE shifts SET ended_at = now() WHERE rider_id = $1 AND ended_at IS NULL", f.rider)
	_, out = f.get("/restock/"+id, f.token)
	other = out["request"].(map[string]any)["shortfall"].(map[string]any)["other_hub"].(map[string]any)
	if v, ok := other["distance_m"]; !ok || v != nil {
		t.Errorf("no shift: %v", other)
	}
}
