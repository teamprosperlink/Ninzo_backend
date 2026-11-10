package restock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

func (f *fixture) withSuggestions() {
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
		f.svc.MountSuggestions(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
	}))
	f.t.Cleanup(srv.Close)
	f.srv = srv
}

func (f *fixture) post(path, token, body string) (int, map[string]any) {
	f.t.Helper()
	f.phones++
	req, _ := http.NewRequest("POST", f.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("restock-key-%05d", f.phones))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// onScooter loads a lot of an item onto the rider's scooter.
func (f *fixture) onScooter(code, item string, qty int) {
	f.t.Helper()
	lot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ($1, $2, $3, now() + interval '1 day') RETURNING id::text", code, item, qty)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, $3)", f.scooter, lot, qty)
}

func (f *fixture) suggestion() string {
	return f.id("SELECT id::text FROM restock_requests WHERE state = 'suggested'")
}

func TestALowScooterGetsOneSuggestion(t *testing.T) {
	f := newFixture(t)
	f.withSuggestions()
	ctx := context.Background()
	f.onScooter("ON-CHAI", f.chai, 2)                                                                 // Below the low level of 3: bring it up to 10.
	f.onScooter("ON-SAMOSA", f.samosa, 5)                                                             // Fine.
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub Z', 12.944, 77.639)", f.zone) // Farther, and last by name.
	now := time.Now()
	if n, err := f.svc.Suggest(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := f.svc.Suggest(ctx, now); n != 0 {
		t.Error("a second suggestion for the same scooter")
	}
	code, out := f.get("/riders/me/restock-suggestions", f.token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockSuggestionList")
	list := out["suggestions"].([]any)
	if len(list) != 1 {
		t.Fatalf("%v", out)
	}
	s := list[0].(map[string]any)
	check(t, s, "RestockSuggestion")
	items := s["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["item_id"] != f.chai || items[0].(map[string]any)["qty"] != float64(8) {
		t.Errorf("%v", items)
	}
	if s["hub"].(map[string]any)["id"] != f.other { // Hub B is nearer the scooter's node.
		t.Errorf("the nearest hub: %v", s["hub"])
	}
}

func TestConfirmingASuggestion(t *testing.T) {
	f := newFixture(t)
	f.withSuggestions()
	ctx := context.Background()
	f.hubLot(f.other, "HUB-CHAI-1", f.chai, 10, time.Hour)
	f.hubLot(f.other, "HUB-SAM-1", f.samosa, 10, time.Hour)
	f.svc.Suggest(ctx, time.Now())
	id := f.suggestion()
	code, out := f.post("/restock/"+id+"/confirm", f.token, `{"quantity_override":[{"item_id":"`+f.chai+`","qty":4},{"item_id":"`+f.samosa+`","qty":2}]}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockResponse")
	if out["request"].(map[string]any)["state"] != "confirmed" || len(out["request"].(map[string]any)["items"].([]any)) != 2 {
		t.Errorf("%v", out)
	}
	if f.count("SELECT qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", id, f.chai) != 4 || f.count("SELECT qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", id, f.samosa) != 2 {
		t.Error("the rider's changed quantities were not kept")
	}
	// A repeat creates no second request.
	if code, again := f.post("/restock/"+id+"/confirm", f.token, `{}`); code != 200 || again["request"].(map[string]any)["id"] != id {
		t.Errorf("%d %v", code, again)
	}
	if f.count("SELECT count(*) FROM restock_requests WHERE state <> 'suggested'") != 1 {
		t.Error("a repeat made a second request")
	}
	// The simulator now takes it from there.
	sim := &Simulator{Service: f.svc}
	sim.Step(ctx, time.Now().Add(time.Minute))
	if f.state(id) != "hub_accepted" {
		t.Errorf("%s", f.state(id))
	}
}

func TestConfirmRefusals(t *testing.T) {
	f := newFixture(t)
	f.withSuggestions()
	ctx := context.Background()
	f.svc.Suggest(ctx, time.Now())
	id := f.suggestion()
	// The hub holds none of it.
	if code, out := f.post("/restock/"+id+"/confirm", f.token, `{}`); code != 409 || out["code"] != "hub_out_of_stock" {
		t.Errorf("%d %v", code, out)
	}
	for _, body := range []string{`{"quantity_override":[{"item_id":"nope","qty":1}]}`, `{"quantity_override":[{"item_id":"` + f.chai + `","qty":0}]}`,
		`{"quantity_override":[{"item_id":"` + f.chai + `","qty":1},{"item_id":"` + f.chai + `","qty":2}]}`, `nope`} {
		if code, _ := f.post("/restock/"+id+"/confirm", f.token, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	// Another request is already open for this scooter.
	f.hubLot(f.other, "HUB-CHAI-2", f.chai, 50, time.Hour)
	f.hubLot(f.other, "HUB-SAM-2", f.samosa, 50, time.Hour)
	f.request(f.chai, 1)
	if code, out := f.post("/restock/"+id+"/confirm", f.token, `{}`); code != 409 || out["code"] != "restock_already_open" {
		t.Errorf("%d %v", code, out)
	}
	// Expired.
	f.exec("UPDATE restock_requests SET state = 'cancelled' WHERE state = 'confirmed'")
	f.exec("UPDATE restock_requests SET expires_at = now() - interval '1 second' WHERE id = $1", id)
	if code, out := f.post("/restock/"+id+"/confirm", f.token, `{}`); code != 409 || out["code"] != "suggestion_expired" {
		t.Errorf("%d %v", code, out)
	}
	_, other := f.signIn(f.id("SELECT node_id::text FROM shifts LIMIT 1"), "")
	if code, _ := f.post("/restock/"+id+"/confirm", other, `{}`); code != 404 {
		t.Errorf("another rider: %d", code)
	}
}

func TestDecliningASuggestion(t *testing.T) {
	f := newFixture(t)
	f.withSuggestions()
	ctx := context.Background()
	now := time.Now()
	f.svc.Suggest(ctx, now)
	id := f.suggestion()
	code, out := f.post("/restock/"+id+"/decline", f.token, `{"reason":"I am heading back to the hub anyway"}`)
	if code != 200 || out["request"].(map[string]any)["state"] != "cancelled" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := f.post("/restock/"+id+"/decline", f.token, `{}`); code != 200 {
		t.Errorf("declining twice: %d", code)
	}
	// Drafted again only at the next stock check, 5 minutes on.
	if n, _ := f.svc.Suggest(ctx, now.Add(time.Minute)); n != 0 {
		t.Error("suggested again straight after a decline")
	}
	if n, _ := f.svc.Suggest(ctx, now.Add(6*time.Minute)); n != 1 {
		t.Error("not suggested again at the next stock check")
	}
}

func TestASuggestionExpires(t *testing.T) {
	f := newFixture(t)
	f.withSuggestions()
	ctx := context.Background()
	now := time.Now()
	f.svc.Suggest(ctx, now)
	id := f.suggestion()
	f.svc.Suggest(ctx, now.Add(11*time.Minute))
	if f.state(id) != "expired" {
		t.Errorf("%s", f.state(id))
	}
	if _, out := f.get("/riders/me/restock-suggestions", f.token); len(out["suggestions"].([]any)) != 0 {
		t.Errorf("%v", out)
	}
}

func TestNoSuggestionWithoutAShiftOrAHub(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec("UPDATE hubs SET status = 'paused'")
	if n, _ := f.svc.Suggest(ctx, time.Now()); n != 0 {
		t.Error("suggested with no active hub")
	}
	f.exec("UPDATE hubs SET status = 'active'")
	f.exec("UPDATE shifts SET ended_at = now()")
	if n, _ := f.svc.Suggest(ctx, time.Now()); n != 0 {
		t.Error("suggested to a rider with no shift")
	}
}
