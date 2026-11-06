package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t *testing.T
	s *Sessions
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, s: &Sessions{
		Pool: dbtest.Fresh(t), Signer: Signer{Key: testKey, TTL: 15 * time.Minute}, RefreshTTL: 90 * 24 * time.Hour, Log: quiet,
	}}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.s.Pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) person() string {
	f.t.Helper()
	var id string
	if err := f.s.Pool.QueryRow(context.Background(), "INSERT INTO people DEFAULT VALUES RETURNING id::text").Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) profile(table, person string) string {
	f.t.Helper()
	var id string
	if err := f.s.Pool.QueryRow(context.Background(), "INSERT INTO "+table+" (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) customer() string { return f.profile("customers", f.person()) }

func (f *fixture) start(actorType, id string) (Pair, Actor) {
	f.t.Helper()
	var pair Pair
	var actor Actor
	ctx := context.Background()
	err := database.InTx(ctx, f.s.Pool, func(tx pgx.Tx) error {
		var err error
		pair, actor, err = f.s.Start(ctx, tx, actorType, id)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return pair, actor
}

func (f *fixture) refresh(token, actorType string) (Pair, error) {
	f.t.Helper()
	pair, _, err := f.s.Refresh(context.Background(), token, actorType)
	return pair, err
}

func (f *fixture) mustRefresh(token, actorType string) Pair {
	f.t.Helper()
	pair, err := f.refresh(token, actorType)
	if err != nil {
		f.t.Fatalf("refresh: %v", err)
	}
	return pair
}

func (f *fixture) refused(token, actorType string) {
	f.t.Helper()
	if _, err := f.refresh(token, actorType); !errors.Is(err, ErrSessionInvalid) {
		f.t.Errorf("want a refusal, got %v", err)
	}
}

func TestStartStoresOnlyAHash(t *testing.T) {
	f := newFixture(t)
	id := f.customer()
	pair, actor := f.start(TypeCustomer, id)
	var stored string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT token_hash FROM refresh_tokens").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == pair.RefreshToken || strings.Contains(stored, pair.RefreshToken) || stored != hashToken(pair.RefreshToken) {
		t.Errorf("stored %q for token %q", stored, pair.RefreshToken)
	}
	got, err := f.s.Signer.Verify(pair.AccessToken, TypeCustomer)
	if err != nil || got != actor || got.ID != id || pair.ExpiresInS != 900 {
		t.Errorf("access token: %+v, %v, %d", got, err, pair.ExpiresInS)
	}
}

func TestRefreshGivesANewPairAndRestarts90Days(t *testing.T) {
	f := newFixture(t)
	first, actor := f.start(TypeCustomer, f.customer())
	f.exec("UPDATE refresh_tokens SET expires_at = now() + interval '1 day'")
	second := f.mustRefresh(first.RefreshToken, TypeCustomer)
	if second.RefreshToken == first.RefreshToken {
		t.Error("the refresh token did not change")
	}
	if got, err := f.s.Signer.Verify(second.AccessToken, TypeCustomer); err != nil || got != actor {
		t.Errorf("the new access token: %+v, %v", got, err)
	}
	var fresh bool
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT expires_at > now() + interval '89 days' FROM refresh_tokens WHERE token_hash = $1", hashToken(second.RefreshToken)).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Error("the new token did not get a fresh 90 days, so a working rider could be signed out")
	}
	third := f.mustRefresh(second.RefreshToken, TypeCustomer)
	f.mustRefresh(third.RefreshToken, TypeCustomer)
}

func TestALostReplyDoesNotSignTheUserOut(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeRider, f.profile("riders", f.person()))
	f.mustRefresh(first.RefreshToken, TypeRider) // This reply never reached the app.
	retry := f.mustRefresh(first.RefreshToken, TypeRider)
	again := f.mustRefresh(first.RefreshToken, TypeRider) // Lost a second time.
	f.mustRefresh(again.RefreshToken, TypeRider)
	if retry.RefreshToken == again.RefreshToken {
		t.Error("two retries returned the same token")
	}
}

func TestALostReplyWorksHoweverLongAgo(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeRider, f.profile("riders", f.person()))
	f.mustRefresh(first.RefreshToken, TypeRider)
	f.exec("UPDATE refresh_tokens SET used_at = now() - interval '6 hours' WHERE used_at IS NOT NULL")
	f.mustRefresh(first.RefreshToken, TypeRider)
}

func TestReusingAUsedTokenEndsTheChain(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeCustomer, f.customer())
	second := f.mustRefresh(first.RefreshToken, TypeCustomer)
	third := f.mustRefresh(second.RefreshToken, TypeCustomer) // The pair from first has been used.
	f.refused(first.RefreshToken, TypeCustomer)
	f.refused(third.RefreshToken, TypeCustomer) // The whole chain ended, including the newest.
	var audited int
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action = 'session.ended' AND reason = 'two holders of one session'").Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Errorf("%d audit rows", audited)
	}
}

// A thief replays an old token and gets a token of the same generation as the real app's. Whichever
// uses theirs second ends the session for both.
func TestARealAppEndsAThiefsSession(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeCustomer, f.customer())
	real := f.mustRefresh(first.RefreshToken, TypeCustomer)
	thief := f.mustRefresh(first.RefreshToken, TypeCustomer)
	realNext := f.mustRefresh(real.RefreshToken, TypeCustomer)
	f.refused(thief.RefreshToken, TypeCustomer)
	f.refused(realNext.RefreshToken, TypeCustomer)
}

func TestAThiefWhoUsesTheirsFirstIsStillCaught(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeCustomer, f.customer())
	real := f.mustRefresh(first.RefreshToken, TypeCustomer)
	thief := f.mustRefresh(first.RefreshToken, TypeCustomer)
	thiefNext := f.mustRefresh(thief.RefreshToken, TypeCustomer)
	f.refused(real.RefreshToken, TypeCustomer)
	f.refused(thiefNext.RefreshToken, TypeCustomer)
}

// Taking turns cannot hide a thief, because generations only move forward.
func TestAThiefAndARealAppTakingTurnsAreCaught(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeCustomer, f.customer())
	a := f.mustRefresh(first.RefreshToken, TypeCustomer)
	b := f.mustRefresh(first.RefreshToken, TypeCustomer)
	a = f.mustRefresh(a.RefreshToken, TypeCustomer)
	if _, err := f.refresh(b.RefreshToken, TypeCustomer); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the second holder was not caught: %v", err)
	}
	f.refused(a.RefreshToken, TypeCustomer)
}

// A delayed duplicate of a request must not sign out a rider who holds a good token.
func TestADelayedDuplicateDoesNotSignARiderOut(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeRider, f.profile("riders", f.person()))
	f.mustRefresh(first.RefreshToken, TypeRider)         // This reply was lost.
	held := f.mustRefresh(first.RefreshToken, TypeRider) // The app's retry: it holds this.
	f.mustRefresh(first.RefreshToken, TypeRider)         // A delayed duplicate of the request.
	next := f.mustRefresh(held.RefreshToken, TypeRider)  // The app still renews.
	f.mustRefresh(next.RefreshToken, TypeRider)
}

func TestOneDevicesProblemDoesNotSignOutAnother(t *testing.T) {
	f := newFixture(t)
	id := f.customer()
	phone, _ := f.start(TypeCustomer, id)
	tablet, _ := f.start(TypeCustomer, id)
	second := f.mustRefresh(phone.RefreshToken, TypeCustomer)
	f.mustRefresh(second.RefreshToken, TypeCustomer)
	f.refused(phone.RefreshToken, TypeCustomer)
	f.mustRefresh(tablet.RefreshToken, TypeCustomer)
}

func TestRefusals(t *testing.T) {
	f := newFixture(t)
	f.refused("not-a-real-token", TypeCustomer)

	pair, _ := f.start(TypeCustomer, f.customer())
	f.exec("UPDATE refresh_tokens SET expires_at = now() - interval '1 second'")
	f.refused(pair.RefreshToken, TypeCustomer)
}

func TestARidersTokenIsMerelyRefusedOnTheCustomerAPI(t *testing.T) {
	f := newFixture(t)
	pair, _ := f.start(TypeRider, f.profile("riders", f.person()))
	f.refused(pair.RefreshToken, TypeCustomer)
	// It was a mistake, not theft, so the rider's session carries on.
	f.mustRefresh(pair.RefreshToken, TypeRider)
}

func TestBlockedAndSuspendedAreRefusedAtRefresh(t *testing.T) {
	f := newFixture(t)
	person := f.person()
	customer := f.profile("customers", person)
	rider := f.profile("riders", person)

	customerPair, _ := f.start(TypeCustomer, customer)
	riderPair, _ := f.start(TypeRider, rider)
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", rider)
	f.refused(riderPair.RefreshToken, TypeRider)
	// Suspension is per role, so the same person can still order food.
	next := f.mustRefresh(customerPair.RefreshToken, TypeCustomer)

	f.exec("UPDATE people SET status = 'blocked' WHERE id = $1", person)
	f.refused(next.RefreshToken, TypeCustomer)

	// Lifting a block does not revive a session that was ended.
	f.exec("UPDATE people SET status = 'active' WHERE id = $1", person)
	f.refused(next.RefreshToken, TypeCustomer)
}

func TestAnApplicantKeepsASession(t *testing.T) {
	f := newFixture(t)
	id := f.profile("riders", f.person())
	var status string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT status FROM riders WHERE id = $1", id).Scan(&status); err != nil || status != "applicant" {
		t.Fatalf("status %q, %v", status, err)
	}
	pair, _ := f.start(TypeRider, id)
	f.mustRefresh(pair.RefreshToken, TypeRider)
}

func TestARemovedProfileHasNoSession(t *testing.T) {
	f := newFixture(t)
	id := f.customer()
	pair, _ := f.start(TypeCustomer, id)
	f.exec("DELETE FROM customers WHERE id = $1", id)
	f.refused(pair.RefreshToken, TypeCustomer)
}

func TestManyRefreshesAtOnceLeaveEveryReplyUsable(t *testing.T) {
	f := newFixture(t)
	first, _ := f.start(TypeRider, f.profile("riders", f.person()))
	const calls = 8
	var wg sync.WaitGroup
	replies := make(chan Pair, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pair, _, err := f.s.Refresh(context.Background(), first.RefreshToken, TypeRider)
			if err != nil {
				t.Errorf("a race must not fail: %v", err)
				return
			}
			replies <- pair
		}()
	}
	wg.Wait()
	close(replies)
	var all []Pair
	for pair := range replies {
		all = append(all, pair)
	}
	if len(all) != calls {
		t.Fatalf("%d replies", len(all))
	}
	// Whichever reply the app ended up with must still work.
	f.mustRefresh(all[len(all)-1].RefreshToken, TypeRider)
}

// refreshInProgress holds a chain as a refresh in progress does, having made a token it has not
// committed. It returns that token, and a function that commits.
//
// Its generation is far ahead, so the token can only be stopped by being revoked with the chain,
// and the check for two holders cannot hide a lock that is missing.
func (f *fixture) refreshInProgress(a Actor) (string, func()) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	// If the test fails first, end the transaction, or closing the pool would wait for ever.
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := lockChain(ctx, tx, a.ChainID); err != nil {
		f.t.Fatal(err)
	}
	pair, _, err := f.s.issue(ctx, tx, a, 50)
	if err != nil {
		f.t.Fatal(err)
	}
	return pair.RefreshToken, func() {
		if err := tx.Commit(ctx); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestEndingAChainWaitsForARefreshInProgress(t *testing.T) {
	ends := map[string]func(*fixture, Actor) error{
		"sign-out": func(f *fixture, a Actor) error { return f.s.SignOut(context.Background(), a) },
		"end-all": func(f *fixture, a Actor) error {
			ctx := context.Background()
			return database.InTx(ctx, f.s.Pool, func(tx pgx.Tx) error { return f.s.EndAll(ctx, tx, a.Type, a.ID, "") })
		},
		"two holders": func(f *fixture, a Actor) error { return nil },
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			first, actor := f.start(TypeCustomer, f.customer())
			if name == "two holders" {
				// The chain ends when a second holder of a generation appears.
				one := f.mustRefresh(first.RefreshToken, TypeCustomer)
				two := f.mustRefresh(first.RefreshToken, TypeCustomer)
				f.mustRefresh(one.RefreshToken, TypeCustomer)
				newToken, commit := f.refreshInProgress(actor)
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = f.refresh(two.RefreshToken, TypeCustomer)
				}()
				time.Sleep(300 * time.Millisecond)
				commit()
				<-done
				f.refused(newToken, TypeCustomer)
				return
			}
			newToken, commit := f.refreshInProgress(actor)
			done := make(chan error, 1)
			go func() { done <- end(f, actor) }()
			select {
			case err := <-done:
				t.Fatalf("it did not wait for the refresh in progress: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			commit()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			f.refused(newToken, TypeCustomer)
			f.refused(first.RefreshToken, TypeCustomer)
		})
	}
}

func TestSignOutEndsOnlyThisDevice(t *testing.T) {
	f := newFixture(t)
	id := f.customer()
	phone, phoneActor := f.start(TypeCustomer, id)
	tablet, _ := f.start(TypeCustomer, id)
	ctx := context.Background()
	if err := f.s.SignOut(ctx, phoneActor); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SignOut(ctx, phoneActor); err != nil {
		t.Errorf("signing out twice: %v", err)
	}
	f.refused(phone.RefreshToken, TypeCustomer)
	f.mustRefresh(tablet.RefreshToken, TypeCustomer)
}

func TestEndAllEndsEverySessionOfOneRole(t *testing.T) {
	f := newFixture(t)
	person := f.person()
	rider := f.profile("riders", person)
	customer := f.profile("customers", person)
	one, _ := f.start(TypeRider, rider)
	two, keep := f.start(TypeRider, rider)
	three, _ := f.start(TypeRider, rider)
	shopping, _ := f.start(TypeCustomer, customer)
	ctx := context.Background()
	err := database.InTx(ctx, f.s.Pool, func(tx pgx.Tx) error { return f.s.EndAll(ctx, tx, TypeRider, rider, keep.ChainID) })
	if err != nil {
		t.Fatal(err)
	}
	f.refused(one.RefreshToken, TypeRider)
	f.refused(three.RefreshToken, TypeRider)
	next := f.mustRefresh(two.RefreshToken, TypeRider)
	f.mustRefresh(shopping.RefreshToken, TypeCustomer)

	err = database.InTx(ctx, f.s.Pool, func(tx pgx.Tx) error { return f.s.EndAll(ctx, tx, TypeRider, rider, "") })
	if err != nil {
		t.Fatal(err)
	}
	f.refused(next.RefreshToken, TypeRider)
}

func TestStartCommitsWithTheSignIn(t *testing.T) {
	f := newFixture(t)
	id := f.customer()
	ctx := context.Background()
	failed := errors.New("the sign-in failed")
	var pair Pair
	err := database.InTx(ctx, f.s.Pool, func(tx pgx.Tx) error {
		var err error
		if pair, _, err = f.s.Start(ctx, tx, TypeCustomer, id); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	f.refused(pair.RefreshToken, TypeCustomer)
}

// ---------- HTTP

func (f *fixture) server(actorType string) (*httptest.Server, *bytes.Buffer) {
	f.t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.s.Log = log
	handler := httpserver.NewRouter(log, func(context.Context) error { return nil }, func(r chi.Router) {
		MountSessions(r, f.s, actorType)
		r.With(RequireActor(f.s.Signer, actorType)).Get("/whoami", func(w http.ResponseWriter, req *http.Request) {
			a, _ := ActorFrom(req.Context())
			id, ok := ActorID(req)
			httpserver.WriteJSON(w, 200, map[string]any{"type": a.Type, "id": id, "ok": ok})
		})
	})
	srv := httptest.NewServer(handler)
	f.t.Cleanup(srv.Close)
	return srv, &logs
}

func call(t *testing.T, method, url, bearer, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
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

// requiredFields reads a schema's required fields from the contract itself.
func requiredFields(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/shared/common.yaml")
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
	fields := doc.Components.Schemas[name].Required
	if len(fields) == 0 {
		t.Fatalf("no required fields for %s, so this check would prove nothing", name)
	}
	return fields
}

func TestRefreshEndpoint(t *testing.T) {
	f := newFixture(t)
	srv, logs := f.server(TypeCustomer)
	pair, _ := f.start(TypeCustomer, f.customer())

	code, body := call(t, "POST", srv.URL+"/v1/auth/refresh", "", `{"refresh_token":"`+pair.RefreshToken+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	for _, field := range requiredFields(t, "TokenPair") {
		if _, ok := body[field]; !ok {
			t.Errorf("the reply has no %s: %v", field, body)
		}
	}
	code, whoami := call(t, "GET", srv.URL+"/v1/whoami", body["access_token"].(string), "")
	if code != 200 || whoami["type"] != TypeCustomer || whoami["ok"] != true {
		t.Errorf("the new access token: %d %v", code, whoami)
	}

	for name, c := range map[string]struct {
		body string
		want int
		code string
	}{
		"not JSON":      {`nope`, 400, "invalid_request"},
		"no token":      {`{}`, 400, "invalid_request"},
		"unknown token": {`{"refresh_token":"unknown"}`, 401, "unauthorized"},
	} {
		got, out := call(t, "POST", srv.URL+"/v1/auth/refresh", "", c.body)
		if got != c.want || out["code"] != c.code {
			t.Errorf("%s: %d %v", name, got, out)
		}
		for _, field := range requiredFields(t, "Error") {
			if _, ok := out[field]; !ok {
				t.Errorf("%s: the error has no %s", name, field)
			}
		}
	}
	if strings.Contains(logs.String(), pair.RefreshToken) || strings.Contains(logs.String(), body["access_token"].(string)) {
		t.Errorf("a token reached the logs: %s", logs.String())
	}
}

func TestRefreshReportsAnOutageAsRetryable(t *testing.T) {
	f := newFixture(t)
	srv, logs := f.server(TypeCustomer)
	f.s.Pool.Close()
	code, body := call(t, "POST", srv.URL+"/v1/auth/refresh", "", `{"refresh_token":"S3CRET-token"}`)
	if code != 503 || body["retryable"] != true {
		t.Errorf("an outage must not look like a signed-out session: %d %v", code, body)
	}
	if strings.Contains(logs.String(), "S3CRET-token") {
		t.Errorf("the token reached the logs: %s", logs.String())
	}
}

func TestUserTypesAreKeptApartOverHTTP(t *testing.T) {
	f := newFixture(t)
	riderAPI, _ := f.server(TypeRider)
	pair, _ := f.start(TypeCustomer, f.customer())
	if code, body := call(t, "GET", riderAPI.URL+"/v1/whoami", pair.AccessToken, ""); code != 401 || body["code"] != "unauthorized" {
		t.Errorf("a customer token on the rider API: %d %v", code, body)
	}
	if code, _ := call(t, "POST", riderAPI.URL+"/v1/auth/refresh", "", `{"refresh_token":"`+pair.RefreshToken+`"}`); code != 401 {
		t.Errorf("a customer refresh on the rider API: %d", code)
	}
	if code, _ := call(t, "POST", riderAPI.URL+"/v1/auth/signout", pair.AccessToken, ""); code != 401 {
		t.Errorf("a customer sign-out on the rider API: %d", code)
	}
}

func TestRequireActor(t *testing.T) {
	f := newFixture(t)
	srv, _ := f.server(TypeCustomer)
	if code, body := call(t, "GET", srv.URL+"/v1/whoami", "", ""); code != 401 || body["code"] != "unauthorized" {
		t.Errorf("no token: %d %v", code, body)
	}
	if code, body := call(t, "GET", srv.URL+"/v1/whoami", "garbage", ""); code != 401 || body["code"] != "unauthorized" {
		t.Errorf("a bad token: %d %v", code, body)
	}
	old := Signer{Key: testKey, TTL: -time.Minute}
	expired, err := old.Sign(Actor{Type: TypeCustomer, ID: "i", ChainID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, "GET", srv.URL+"/v1/whoami", expired, ""); code != 401 || body["code"] != "token_expired" {
		t.Errorf("expired: %d %v", code, body)
	}
}

func TestSignOutEndpoint(t *testing.T) {
	f := newFixture(t)
	srv, _ := f.server(TypeCustomer)
	pair, _ := f.start(TypeCustomer, f.customer())
	if code, _ := call(t, "POST", srv.URL+"/v1/auth/signout", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	code, body := call(t, "POST", srv.URL+"/v1/auth/signout", pair.AccessToken, "")
	if code != 200 || body["server_time"] == nil {
		t.Errorf("%d %v", code, body)
	}
	if code, _ := call(t, "POST", srv.URL+"/v1/auth/refresh", "", `{"refresh_token":"`+pair.RefreshToken+`"}`); code != 401 {
		t.Errorf("refresh after sign-out: %d", code)
	}
}
