package restock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
)

// Issue 235, part B1: each hub in the restock options carries the manager's number, for Call hub manager.

func TestRestockOptionHubsCarryTheManagerPhone(t *testing.T) {
	f := newFixture(t)
	f.exec("UPDATE hubs SET manager_phone = '+919876500001' WHERE id = $1", f.hub)
	code, out := f.get("/restock/options", f.token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, "rider.openapi.yaml", "RestockOptions", out)
	phones := map[string]any{}
	for _, h := range out["hubs"].([]any) {
		hub := h.(map[string]any)
		phone, present := hub["manager_phone"]
		if !present {
			t.Errorf("a hub with no manager_phone key: %v", hub)
		}
		phones[hub["id"].(string)] = phone
	}
	if len(phones) != 2 || phones[f.hub] != "+919876500001" || phones[f.other] != nil {
		t.Errorf("manager phones %v: the hub with a number shows it, the other is null", phones)
	}
	// Only a hub's own number is ever sent: no customer's phone, however the hub is read.
	raw, _ := json.Marshal(out)
	if strings.Count(string(raw), "+91") != 1 {
		t.Errorf("one number was expected in the options: %s", raw)
	}
}

func TestAHubManagerPhoneMustBeAnE164Number(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"98765", "+0123456789", "call me", "+91 98765 00001", "9876500001"} {
		if _, err := f.pool.Exec(context.Background(), "UPDATE hubs SET manager_phone = $2 WHERE id = $1", f.hub, bad); err == nil {
			t.Errorf("the database accepted manager_phone %q", bad)
		}
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE hubs SET manager_phone = NULL WHERE id = $1", f.hub); err != nil {
		t.Error(err)
	}
}
