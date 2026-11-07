package catalog

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func setup(t *testing.T) (*pgxpool.Pool, *httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Fresh(t)
	sessions := &identity.Sessions{Pool: pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	svc := &Service{Pool: pool, Log: quiet}
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountCustomer(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer))
	}))
	t.Cleanup(srv.Close)
	var person, customer string
	if err := pool.QueryRow(ctx, "INSERT INTO people DEFAULT VALUES RETURNING id::text").Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person).Scan(&customer); err != nil {
		t.Fatal(err)
	}
	var pair identity.Pair
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = sessions.Start(ctx, tx, identity.TypeCustomer, customer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pool, srv, pair.AccessToken
}

func get(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func required(t *testing.T, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/customer/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Components.Schemas[schema].Required) == 0 {
		t.Fatalf("no required fields for %s", schema)
	}
	return doc.Components.Schemas[schema].Required
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestTheMenuShowsOnlyItemsOnSaleInOrder(t *testing.T) {
	pool, srv, token := setup(t)
	exec(t, pool, `INSERT INTO menu_items (name, price_paise, status, sort_order, allergens) VALUES
		('Chai', 4900, 'active', 1, '{milk}'), ('Burger', 14900, 'active', 2, '{gluten,milk}'), ('Secret', 9900, 'hidden', 0, '{}')`)
	code, out := get(t, srv.URL+"/v1/menu/items", token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for _, f := range required(t, "MenuItemList") {
		if _, ok := out[f]; !ok {
			t.Errorf("no %s", f)
		}
	}
	items := out["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "Chai" || items[1].(map[string]any)["name"] != "Burger" {
		t.Fatalf("%v", items)
	}
	first := items[1].(map[string]any)
	for _, f := range required(t, "MenuItem") {
		if _, ok := first[f]; !ok {
			t.Errorf("an item has no %s", f)
		}
	}
	if p := first["price"].(map[string]any); p["amount_minor"] != float64(14900) || p["currency"] != "INR" {
		t.Errorf("%v", p)
	}
}

func TestOneItem(t *testing.T) {
	pool, srv, token := setup(t)
	var id, hidden string
	if err := pool.QueryRow(context.Background(), "INSERT INTO menu_items (name, price_paise, status) VALUES ('Chai', 4900, 'active') RETURNING id::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), "INSERT INTO menu_items (name, price_paise) VALUES ('Hidden', 4900) RETURNING id::text").Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	code, out := get(t, srv.URL+"/v1/menu/items/"+id, token)
	if code != 200 || out["item"].(map[string]any)["allergens"] == nil {
		t.Errorf("%d %v", code, out)
	}
	for _, f := range required(t, "MenuItemResponse") {
		if _, ok := out[f]; !ok {
			t.Errorf("no %s", f)
		}
	}
	for _, path := range []string{hidden, "00000000-0000-0000-0000-000000000000", "not-an-id"} {
		if code, _ := get(t, srv.URL+"/v1/menu/items/"+path, token); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ := get(t, srv.URL+"/v1/menu/items", ""); code != 401 {
		t.Errorf("no sign-in: %d", code)
	}
}

func TestAnEmptyMenuIsAnEmptyList(t *testing.T) {
	_, srv, token := setup(t)
	if _, out := get(t, srv.URL+"/v1/menu/items", token); out["items"] == nil || len(out["items"].([]any)) != 0 {
		t.Errorf("%v", out)
	}
}

func TestEveryPriceIsKept(t *testing.T) {
	pool, _, _ := setup(t)
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, "INSERT INTO menu_items (name, price_paise) VALUES ('Chai', 4900) RETURNING id::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('ninzo.price_reason', 'festival price', true)"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE menu_items SET price_paise = 5900 WHERE id = $1", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, "UPDATE menu_items SET name = 'Masala chai' WHERE id = $1", id) // Not a price change.
	rows, err := pool.Query(ctx, "SELECT price_paise, reason FROM price_history WHERE item_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		price  int
		reason string
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (entry, error) {
		var e entry
		return e, r.Scan(&e.price, &e.reason)
	})
	if err != nil || len(got) != 2 || got[0].price != 4900 || got[1].price != 5900 || got[1].reason != "festival price" {
		t.Errorf("%+v %v", got, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE price_history SET price_paise = 200"); err == nil {
		t.Error("an old price could be rewritten")
	}
}

func TestBadItemsAreRefused(t *testing.T) {
	pool, _, _ := setup(t)
	for _, sql := range []string{
		"INSERT INTO menu_items (name, price_paise) VALUES ('Free', 0)",
		"INSERT INTO menu_items (name, price_paise, currency) VALUES ('Dollars', 4900, 'USD')",
		"INSERT INTO menu_items (name, price_paise, photo_url) VALUES ('Plain', 4900, 'http://x.test/a.png')",
		"INSERT INTO menu_items (name, price_paise, status) VALUES ('Odd', 4900, 'sold')",
		"INSERT INTO menu_items (name, price_paise) VALUES ('', 4900)",
		"INSERT INTO menu_items (name, price_paise, allergens) VALUES ('Nulls', 4900, ARRAY['milk', NULL])",
	} {
		if _, err := pool.Exec(context.Background(), sql); err == nil {
			t.Errorf("stored: %s", sql)
		}
	}
}
