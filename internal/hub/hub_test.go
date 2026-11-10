package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	srv      *httptest.Server
	sessions *identity.Sessions
	hub      string
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t)}
	f.sessions = &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	svc := &Service{Pool: f.pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountStaff(r, identity.RequireActor(f.sessions.Signer, identity.TypeStaff))
	}))
	t.Cleanup(f.srv.Close)
	zone := f.id("INSERT INTO zones (name, city, status) VALUES ('Z', 'Bengaluru', 'active') RETURNING id::text")
	f.hub = f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub A', 12.93, 77.62) RETURNING id::text", zone)
	return f
}

func (f *fixture) staff(phone, role string) (string, string) {
	f.t.Helper()
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
	id := f.id("INSERT INTO hub_staff (person_id, hub_id, role, name) VALUES ($1, $2, $3, 'Asha') RETURNING id::text", person, f.hub, role)
	return id, f.token(identity.TypeStaff, id)
}

func (f *fixture) token(actorType, id string) string {
	f.t.Helper()
	var pair identity.Pair
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.sessions.Start(context.Background(), tx, actorType, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return pair.AccessToken
}

func (f *fixture) me(token string) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest("GET", f.srv.URL+"/v1/hub/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestTheSignedInStaffMember(t *testing.T) {
	f := newFixture(t)
	id, token := f.staff("+919800000091", "hub_lead")
	code, out := f.me(token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	st := out["staff"].(map[string]any)
	if st["id"] != id || st["role"] != "hub_lead" || st["name"] != "Asha" || st["hub_open"] != true || st["hub"].(map[string]any)["id"] != f.hub || out["server_time"] == nil {
		t.Errorf("%v", out)
	}
	f.exec("UPDATE hubs SET status = 'paused'")
	if _, out := f.me(token); out["staff"].(map[string]any)["hub_open"] != false {
		t.Error("a paused hub must show as closed")
	}
}

func TestSignedOutSuspendedAndOtherTokens(t *testing.T) {
	f := newFixture(t)
	id, token := f.staff("+919800000092", "hub_staff")
	if code, _ := f.me("nope"); code != 401 {
		t.Errorf("no token: %d", code)
	}
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000093') RETURNING id::text")
	rider := f.id("INSERT INTO riders (person_id, status) VALUES ($1, 'active') RETURNING id::text", person)
	if code, _ := f.me(f.token(identity.TypeRider, rider)); code != 401 {
		t.Errorf("a rider's token: %d", code)
	}
	f.exec("UPDATE hub_staff SET status = 'suspended' WHERE id = $1", id)
	if code, out := f.me(token); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("suspended: %d %v", code, out)
	}
	f.exec("UPDATE hub_staff SET status = 'active' WHERE id = $1", id)
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.me(token); code != 401 {
		t.Errorf("an ended session: %d", code)
	}
}
