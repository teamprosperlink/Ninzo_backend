package restock

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

func (f *fixture) withRequests() {
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
		f.svc.MountRequests(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
	}))
	f.t.Cleanup(srv.Close)
	f.srv = srv
}

// packed runs the simulator from confirmed through packing.
func (f *fixture) packed(now time.Time) {
	sim := &Simulator{Service: f.svc}
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(context.Background(), now.Add(after))
	}
}

func (f *fixture) scan(code string) error {
	return database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, err := shifts.ScanTx(context.Background(), tx, f.rider, code, false)
		return err
	})
}

func TestAManualRequestMergesUntilTheHubStarts(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-M-1", f.chai, 10, time.Hour)
	f.hubLot(f.hub, "HUB-M-2", f.samosa, 10, time.Hour)
	f.exec("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, expires_at) VALUES ($1, $2, $3, 'suggested', 'suggested', now() + interval '10 minutes')",
		f.rider, f.scooter, f.other)
	code, out := f.post("/restock", f.token, `{"hub_id":"`+f.hub+`","items":[{"item_id":"`+f.chai+`","qty":3}],"reason":"two cups split"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockResponse")
	id := out["request"].(map[string]any)["id"].(string)
	if f.state(id) != "confirmed" || f.count("SELECT count(*) FROM restock_requests WHERE state = 'suggested'") != 0 {
		t.Error("a manual request must be confirmed, and replace the suggestion")
	}
	// A second request merges into the first.
	code, out = f.post("/restock", f.token, `{"hub_id":"`+f.hub+`","items":[{"item_id":"`+f.chai+`","qty":2},{"item_id":"`+f.samosa+`","qty":1}]}`)
	if code != 201 || out["request"].(map[string]any)["id"] != id {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", id, f.chai) != 5 || f.count("SELECT qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", id, f.samosa) != 1 {
		t.Error("the lines did not merge")
	}
	// Another hub, or once the hub has accepted it, is refused.
	f.hubLot(f.other, "HUB-M-3", f.chai, 10, time.Hour)
	if code, out := f.post("/restock", f.token, `{"hub_id":"`+f.other+`","items":[{"item_id":"`+f.chai+`","qty":1}]}`); code != 409 || out["code"] != "restock_already_open" {
		t.Errorf("%d %v", code, out)
	}
	(&Simulator{Service: f.svc}).Step(context.Background(), time.Now().Add(time.Minute))
	if code, out := f.post("/restock", f.token, `{"hub_id":"`+f.hub+`","items":[{"item_id":"`+f.chai+`","qty":1}]}`); code != 409 || out["code"] != "restock_already_open" {
		t.Errorf("%d %v", code, out)
	}
}

func TestManualRequestRefusals(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-MR-1", f.chai, 2, time.Hour)
	if code, out := f.post("/restock", f.token, `{"hub_id":"`+f.hub+`","items":[{"item_id":"`+f.chai+`","qty":3}]}`); code != 409 || out["code"] != "hub_out_of_stock" {
		t.Errorf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM restock_requests") != 0 {
		t.Error("a refused request was kept")
	}
	far := f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Far', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.70 12.90, 77.71 12.90, 77.71 12.91, 77.70 12.91, 77.70 12.90)))', 4326)) RETURNING id::text`)
	farHub := f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Far hub', 12.905, 77.705) RETURNING id::text", far)
	paused := f.id("INSERT INTO hubs (zone_id, name, lat, lng, status) VALUES ($1, 'Shut', 12.93, 77.62, 'paused') RETURNING id::text", f.zone)
	for _, hub := range []string{farHub, paused, "00000000-0000-0000-0000-000000000000"} {
		if code, _ := f.post("/restock", f.token, `{"hub_id":"`+hub+`","items":[{"item_id":"`+f.chai+`","qty":1}]}`); code != 404 {
			t.Errorf("hub %s: %d", hub, code)
		}
	}
	for _, body := range []string{`{"hub_id":"` + f.hub + `","items":[]}`, `{"hub_id":"` + f.hub + `","items":[{"item_id":"` + f.chai + `","qty":501}]}`,
		`{"hub_id":"nope","items":[{"item_id":"` + f.chai + `","qty":1}]}`, `nope`} {
		if code, _ := f.post("/restock", f.token, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	f.exec("UPDATE shifts SET ended_at = now()")
	if code, _ := f.post("/restock", f.token, `{"hub_id":"`+f.hub+`","items":[{"item_id":"`+f.chai+`","qty":1}]}`); code != 404 {
		t.Errorf("no shift: %d", code)
	}
}

func TestAcceptingAPartialFillKeepsWhatWasPacked(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-P-1", f.chai, 4, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	if f.state(id) != "partial" {
		t.Fatalf("%s", f.state(id))
	}
	code, out := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":true}`)
	if code != 200 || f.state(id) != "ready" || f.id("SELECT status FROM shortfalls WHERE request_id = $1", id) != "accepted" {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockResponse")
	if code, out := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":false}`); code != 409 || out["code"] != "shortfall_not_open" {
		t.Errorf("answered twice: %d %v", code, out)
	}
	if code, _ := f.post("/restock/"+id+"/shortfall-response", f.token, `{}`); code != 400 {
		t.Errorf("no answer: %d", code)
	}
}

func TestRefusingAPartialFillPutsTheLotsBack(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-PR-1", f.chai, 4, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	if code, _ := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":false}`); code != 200 || f.state(id) != "cancelled" {
		t.Fatalf("%d %s", code, f.state(id))
	}
	if f.count("SELECT count(*) FROM restock_lots WHERE released_at IS NULL") != 0 {
		t.Error("the packed lots were not put back")
	}
}

func TestNothingPackedAndAcceptedIsClosed(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	id := f.request(f.chai, 3)
	f.packed(time.Now())
	if code, _ := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":true}`); code != 200 || f.state(id) != "closed" {
		t.Errorf("%d %s", code, f.state(id))
	}
}

func TestAcceptingAnotherHubMovesTheRequest(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-O-1", f.chai, 4, time.Hour)
	f.hubLot(f.other, "HUB-O-2", f.chai, 10, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	code, out := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":true}`)
	if code != 200 || f.state(id) != "cancelled" {
		t.Fatalf("%d %v", code, out)
	}
	follow := f.id("SELECT id::text FROM restock_requests WHERE follow_of = $1", id)
	if f.state(follow) != "confirmed" || f.id("SELECT hub_id::text FROM restock_requests WHERE id = $1", follow) != f.other ||
		f.count("SELECT qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", follow, f.chai) != 8 {
		t.Error("the follow-up must ask the other hub for the whole request")
	}
	if f.count("SELECT count(*) FROM restock_lots WHERE request_id = $1 AND released_at IS NULL", id) != 0 {
		t.Error("the first hub's lots were not put back")
	}
	if _, cur := f.get("/restock/"+follow, f.token); cur["request"].(map[string]any)["hub"].(map[string]any)["id"] != f.other {
		t.Errorf("%v", cur)
	}
}

func TestRefusingAnotherHubKeepsWhatWasPacked(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-OR-1", f.chai, 4, time.Hour)
	f.hubLot(f.other, "HUB-OR-2", f.chai, 10, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	if code, _ := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":false}`); code != 200 || f.state(id) != "ready" {
		t.Fatalf("%d %s", code, f.state(id))
	}
	if f.count("SELECT count(*) FROM restock_requests") != 1 || f.count("SELECT count(*) FROM restock_lots WHERE released_at IS NULL") != 1 {
		t.Error("refusing must keep the packed lot and open nothing new")
	}
}

func TestPickingUpTheCassette(t *testing.T) {
	f := newFixture(t)
	f.hubLot(f.hub, "HUB-PU-1", f.chai, 5, time.Hour)
	f.hubLot(f.hub, "HUB-PU-2", f.chai, 5, time.Hour)
	id := f.request(f.chai, 10)
	f.packed(time.Now())
	if f.state(id) != "ready" {
		t.Fatalf("%s", f.state(id))
	}
	if err := f.scan("HUB-PU-1"); err != nil {
		t.Fatal(err)
	}
	if f.state(id) != "picked_up" {
		t.Errorf("after one lot: %s", f.state(id))
	}
	if err := f.scan("HUB-PU-2"); err != nil {
		t.Fatal(err)
	}
	if f.state(id) != "received" {
		t.Errorf("after every lot: %s", f.state(id))
	}
}

func TestScanningAShortCassetteSettlesTheShortfall(t *testing.T) {
	f := newFixture(t)
	f.hubLot(f.hub, "HUB-SS-1", f.chai, 4, time.Hour)
	f.hubLot(f.other, "HUB-SS-2", f.chai, 10, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	if err := f.scan("HUB-SS-1"); err != nil {
		t.Fatal(err)
	}
	if f.state(id) != "received" || f.id("SELECT status FROM shortfalls WHERE request_id = $1", id) != "refused" {
		t.Errorf("taking the cassette refuses the other hub: %s", f.state(id))
	}
	if f.count("SELECT count(*) FROM restock_requests") != 1 {
		t.Error("a follow-up was opened")
	}
}

func TestAnUncollectedCassetteExpires(t *testing.T) {
	f := newFixture(t)
	f.hubLot(f.hub, "HUB-UC-1", f.chai, 5, time.Hour)
	f.hubLot(f.hub, "HUB-UC-2", f.samosa, 5, time.Hour)
	id := f.request(f.chai, 5)
	now := time.Now()
	f.packed(now)
	sim := &Simulator{Service: f.svc}
	if err := sim.expireUncollected(context.Background(), now.Add(20*time.Minute)); err != nil || f.state(id) != "ready" {
		t.Fatalf("expired too soon: %v %s", err, f.state(id))
	}
	if err := sim.expireUncollected(context.Background(), now.Add(45*time.Minute)); err != nil || f.state(id) != "expired" {
		t.Fatalf("%v %s", err, f.state(id))
	}
	if f.count("SELECT count(*) FROM restock_lots WHERE released_at IS NULL") != 0 {
		t.Error("the lots did not go back on the shelf")
	}
	// A cassette the rider started on is received for what was taken.
	second := f.request(f.chai, 5, f.samosa, 5)
	f.packed(now)
	if err := f.scan("HUB-UC-1"); err != nil {
		t.Fatal(err)
	}
	if err := sim.expireUncollected(context.Background(), now.Add(45*time.Minute)); err != nil || f.state(second) != "received" {
		t.Fatalf("%v %s", err, f.state(second))
	}
	if f.count("SELECT count(*) FROM restock_lots x JOIN lots l ON l.id = x.lot_id WHERE x.request_id = $1 AND x.released_at IS NULL", second) != 1 {
		t.Error("only the scanned lot stays with the request")
	}
}

func TestAScanAfterTheCassetteEndedIsRefused(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-LATE-1", f.chai, 4, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	if code, _ := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":false}`); code != 200 {
		t.Fatal(code)
	}
	if code, _, ok := shifts.RefusalOf(f.scan("HUB-LATE-1")); !ok || code != "bad_scan" {
		t.Error("a lot from a refused cassette was scanned")
	}
	if f.state(id) != "cancelled" {
		t.Errorf("%s", f.state(id))
	}
	if _, out := f.get("/restock/"+id, f.token); len(out["request"].(map[string]any)["lot_numbers"].([]any)) != 0 {
		t.Errorf("released lots are still listed: %v", out)
	}
}

// TestAScanRacingARefusal runs a scan that has found the cassette while the rider refuses it. The
// scan waits on the request's lock, then must see it ended.
func TestAScanRacingARefusal(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	ctx := context.Background()
	f.hubLot(f.hub, "HUB-RACE-1", f.chai, 4, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	refuse, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer refuse.Rollback(ctx)
	if _, err := f.svc.RespondTx(ctx, refuse, f.rider, id, false, false); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- f.scan("HUB-RACE-1") }()
	time.Sleep(500 * time.Millisecond) // The scan is now waiting on the request's lock.
	if err := refuse.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _, ok := shifts.RefusalOf(<-result); !ok || code != "bad_scan" {
		t.Error("the scan took a lot from a cassette that was refused while it waited")
	}
	if f.state(id) != "cancelled" || f.count("SELECT count(*) FROM scooter_stock") != 0 {
		t.Errorf("the refused cassette was overwritten: %s", f.state(id))
	}
}

func TestAcceptingAnotherHubReturnsTheFollowUp(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-FR-1", f.chai, 4, time.Hour)
	f.hubLot(f.other, "HUB-FR-2", f.chai, 10, time.Hour)
	id := f.request(f.chai, 8)
	f.packed(time.Now())
	_, out := f.post("/restock/"+id+"/shortfall-response", f.token, `{"accept":true}`)
	follow := f.id("SELECT id::text FROM restock_requests WHERE follow_of = $1", id)
	if out["request"].(map[string]any)["id"] != follow || out["request"].(map[string]any)["state"] != "confirmed" {
		t.Errorf("%v", out)
	}
}

func TestPickupAndExpiryAreAudited(t *testing.T) {
	f := newFixture(t)
	f.hubLot(f.hub, "HUB-AU-1", f.chai, 5, time.Hour)
	f.hubLot(f.hub, "HUB-AU-2", f.samosa, 5, time.Hour)
	first := f.request(f.chai, 5)
	now := time.Now()
	f.packed(now)
	if err := f.scan("HUB-AU-1"); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.received' AND entity_id = $1", first) != 1 {
		t.Error("the pickup was not audited")
	}
	second := f.request(f.samosa, 5)
	f.packed(now)
	if err := (&Simulator{Service: f.svc}).expireUncollected(context.Background(), now.Add(45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.uncollected' AND entity_id = $1", second) != 1 {
		t.Error("the expiry was not audited")
	}
}
