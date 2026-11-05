package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

const (
	userA = "11111111-1111-1111-1111-111111111111"
	userB = "22222222-2222-2222-2222-222222222222"
	key   = "key-for-this-action"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *httptest.Server
	runs atomic.Int32
}

// newEnv serves one handler behind the middleware. The user comes from a test header.
func newEnv(t *testing.T, opts Options, handler func(*env, http.ResponseWriter, *http.Request)) *env {
	t.Helper()
	e := &env{t: t, pool: dbtest.Fresh(t)}
	opts.Log = quiet
	opts.Actor = func(r *http.Request) (string, bool) {
		u := r.Header.Get("X-Test-User")
		return u, u != ""
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.runs.Add(1)
		handler(e, w, r)
	})
	e.srv = httptest.NewServer(Middleware(e.pool, opts)(inner))
	t.Cleanup(e.srv.Close)
	return e
}

// effectAndSave makes a real change and stores the reply in one transaction, as handlers must.
func effectAndSave(e *env, w http.ResponseWriter, r *http.Request) {
	reply := map[string]any{"n": e.runs.Load(), "server_time": time.Now().UTC()}
	err := database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "INSERT INTO people DEFAULT VALUES"); err != nil {
			return err
		}
		return Save(r.Context(), tx, http.StatusCreated, reply)
	})
	if err != nil {
		httpserver.WriteError(w, 500, "internal_error", "failed", true)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

type reply struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (e *env) post(path, user, idemKey, body string) reply {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: string(raw), header: resp.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (e *env) effects() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM people").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestRepeatReturnsTheFirstReplyAndActsOnce(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	first := e.post("/orders", userA, key, `{"a":1}`)
	if first.status != 201 || first.header.Get("Idempotency-Replayed") != "" {
		t.Fatalf("first: %d %v", first.status, first.header)
	}
	again := e.post("/orders", userA, key, `{"a":1}`)
	if again.status != 201 || again.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("again: %d %v", again.status, again.header)
	}
	if again.body["n"] != first.body["n"] || again.body["server_time"] != first.body["server_time"] {
		t.Errorf("the replay differs: %v, %v", first.body, again.body)
	}
	if again.header.Get("Content-Type") != "application/json" {
		t.Errorf("content type: %s", again.header.Get("Content-Type"))
	}
	if e.effects() != 1 || e.runs.Load() != 1 {
		t.Errorf("acted %d times, ran %d", e.effects(), e.runs.Load())
	}
}

func TestMissingOrBadKeyIsRefused(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	for _, bad := range []string{"", "short", "has spaces in it", "has/slash/in/it", strings.Repeat("x", 129)} {
		r := e.post("/orders", userA, bad, `{}`)
		if r.status != 400 || r.body["code"] != "idempotency_key_required" {
			t.Errorf("key %q: %d %v", bad, r.status, r.body)
		}
	}
	if e.runs.Load() != 0 {
		t.Error("the handler ran with a bad key")
	}
}

func TestNoUserIsRefused(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	if r := e.post("/orders", "", key, `{}`); r.status != 401 {
		t.Errorf("got %d", r.status)
	}
}

func TestSameKeyDifferentBodyIsRefused(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	e.post("/orders", userA, key, `{"a":1}`)
	r := e.post("/orders", userA, key, `{"a":2}`)
	if r.status != 422 || r.body["code"] != "idempotency_key_reused" || r.body["retryable"] != false {
		t.Errorf("got %d %v", r.status, r.body)
	}
	if e.effects() != 1 {
		t.Errorf("acted %d times", e.effects())
	}
}

func TestAKeyBelongsToOneUserMethodAndPath(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	e.post("/orders", userA, key, `{}`)
	if r := e.post("/orders", userB, key, `{}`); r.status != 201 || r.header.Get("Idempotency-Replayed") != "" {
		t.Errorf("another user got %d %v", r.status, r.header)
	}
	if r := e.post("/orders/2", userA, key, `{}`); r.status != 201 || r.header.Get("Idempotency-Replayed") != "" {
		t.Errorf("another path got %d %v", r.status, r.header)
	}
	req, _ := http.NewRequest(http.MethodPut, e.srv.URL+"/orders", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Test-User", userA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("Idempotency-Replayed") != "" {
		t.Errorf("another method got %d", resp.StatusCode)
	}
	if e.effects() != 4 {
		t.Errorf("acted %d times", e.effects())
	}
}

func TestOnlySuccessIsStored(t *testing.T) {
	for _, status := range []int{400, 401, 409, 429, 500, 503} {
		fail := atomic.Bool{}
		fail.Store(true)
		e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				httpserver.WriteError(w, status, "rejected", "No.", false)
				return
			}
			effectAndSave(e, w, r)
		})
		if r := e.post("/cancel", userA, key, `{}`); r.status != status {
			t.Errorf("first: %d", r.status)
		}
		fail.Store(false)
		// The same key is checked again against the current state.
		if r := e.post("/cancel", userA, key, `{}`); r.status != 201 || r.header.Get("Idempotency-Replayed") != "" {
			t.Errorf("after a %d, the retry got %d %v", status, r.status, r.header)
		}
		if e.runs.Load() != 2 {
			t.Errorf("after a %d, ran %d times", status, e.runs.Load())
		}
	}
}

func TestSecondRequestWhileRunningGetsInProgress(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		// Once, so a second run fails the checks below and does not panic or hang.
		once.Do(func() { close(started) })
		select {
		case <-finish:
		case <-time.After(5 * time.Second):
		}
		effectAndSave(e, w, r)
	})
	done := make(chan reply, 1)
	go func() { done <- e.post("/orders", userA, key, `{}`) }()
	<-started

	r := e.post("/orders", userA, key, `{}`)
	if r.status != 409 || r.body["code"] != "idempotency_in_progress" || r.body["retryable"] != true {
		t.Errorf("got %d %v", r.status, r.body)
	}
	after, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || after < 1 || after > int(lockFor.Seconds())+1 {
		t.Errorf("Retry-After: %q", r.header.Get("Retry-After"))
	}
	// A different body while running is still a misuse of the key.
	if r := e.post("/orders", userA, key, `{"other":1}`); r.status != 422 {
		t.Errorf("a different body while running: %d", r.status)
	}
	close(finish)
	if first := <-done; first.status != 201 {
		t.Errorf("first: %d", first.status)
	}
	if e.effects() != 1 {
		t.Errorf("acted %d times", e.effects())
	}
}

func TestManyTapsAtOnceActOnce(t *testing.T) {
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // Long enough for the taps to overlap.
		effectAndSave(e, w, r)
	})
	const taps = 20
	results := make(chan int, taps)
	var wg sync.WaitGroup
	for i := 0; i < taps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- e.post("/orders", userA, key, `{}`).status
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[201] < 1 || counts[409] < 1 || counts[201]+counts[409] != taps {
		t.Errorf("the taps must overlap, with some told to wait: %v", counts)
	}
	if e.effects() != 1 || e.runs.Load() != 1 {
		t.Errorf("%d taps acted %d times, ran %d", taps, e.effects(), e.runs.Load())
	}
}

func TestReplyAndChangeCommitTogether(t *testing.T) {
	failed := errors.New("failed after saving")
	var mode atomic.Int32
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		err := database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), "INSERT INTO people DEFAULT VALUES"); err != nil {
				return err
			}
			if err := Save(r.Context(), tx, 200, map[string]any{"ok": true}); err != nil {
				return err
			}
			if mode.Load() == 0 {
				return failed // Rolls back both the change and the stored reply.
			}
			return nil
		})
		if err != nil {
			httpserver.WriteError(w, 500, "internal_error", "failed", true)
			return
		}
		httpserver.WriteJSON(w, 200, map[string]any{"ok": true})
	})
	if r := e.post("/orders", userA, key, `{}`); r.status != 500 {
		t.Fatalf("first: %d", r.status)
	}
	if e.effects() != 0 {
		t.Error("the change survived a rollback")
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM idempotency_keys").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d key rows left, so the retry would be blocked", rows)
	}
	mode.Store(1)
	if r := e.post("/orders", userA, key, `{}`); r.status != 200 || r.header.Get("Idempotency-Replayed") != "" {
		t.Errorf("the retry must act: %d %v", r.status, r.header)
	}
	if e.effects() != 1 {
		t.Errorf("acted %d times", e.effects())
	}
}

func TestACrashedRequestHoldsItsKeyThenARetryRuns(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	ctx := context.Background()
	hash := hashBody([]byte(`{}`), nil)
	// A request that crashed mid-way left this row.
	_, err := e.pool.Exec(ctx, `INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash, locked_until)
		VALUES ($1, 'POST', '/orders', $2, $3, now() + interval '10 seconds')`, userA, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.post("/orders", userA, key, `{}`); r.status != 409 {
		t.Errorf("while locked: %d", r.status)
	}
	if _, err := e.pool.Exec(ctx, "UPDATE idempotency_keys SET locked_until = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	// The earlier action may have run, so a different body stays refused.
	if r := e.post("/orders", userA, key, `{"other":1}`); r.status != 422 {
		t.Errorf("a different body after a crash: %d", r.status)
	}
	if r := e.post("/orders", userA, key, `{}`); r.status != 201 {
		t.Errorf("after the lock ended: %d", r.status)
	}
}

func TestARequestThatLostItsKeyCannotCommit(t *testing.T) {
	var saveErr error
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		// Another request takes over between this one's claim and its save.
		if _, err := e.pool.Exec(r.Context(), "UPDATE idempotency_keys SET locked_until = now() + interval '1 minute'"); err != nil {
			e.t.Error(err)
		}
		saveErr = database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), "INSERT INTO people DEFAULT VALUES"); err != nil {
				return err
			}
			return Save(r.Context(), tx, 200, map[string]any{})
		})
		httpserver.WriteError(w, 409, "lost", "lost", false)
	})
	e.post("/orders", userA, key, `{}`)
	if !errors.Is(saveErr, ErrClaimLost) {
		t.Errorf("got %v", saveErr)
	}
	if e.effects() != 0 {
		t.Error("a request that lost its key committed its change")
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM idempotency_keys").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the other request's key was removed: %d rows", rows)
	}
}

func TestAPanicFreesTheKey(t *testing.T) {
	var boom atomic.Bool
	boom.Store(true)
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		if boom.Load() {
			panic(http.ErrAbortHandler)
		}
		effectAndSave(e, w, r)
	})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/orders", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Test-User", userA)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	boom.Store(false)
	var r reply
	for i := 0; i < 20; i++ { // The release runs as the panic unwinds.
		if r = e.post("/orders", userA, key, `{}`); r.status != 409 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.status != 201 {
		t.Errorf("the retry after a panic: %d %v", r.status, r.body)
	}
}

func TestAKeyOlderThan24HoursIsNew(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	e.post("/orders", userA, key, `{"a":1}`)
	if _, err := e.pool.Exec(context.Background(), "UPDATE idempotency_keys SET created_at = now() - interval '24 hours 1 minute'"); err != nil {
		t.Fatal(err)
	}
	if r := e.post("/orders", userA, key, `{"a":2}`); r.status != 201 || r.header.Get("Idempotency-Replayed") != "" {
		t.Errorf("got %d %v", r.status, r.header)
	}
	if e.effects() != 2 {
		t.Errorf("acted %d times", e.effects())
	}
	if _, err := e.pool.Exec(context.Background(), "UPDATE idempotency_keys SET created_at = now() - interval '23 hours 59 minutes'"); err != nil {
		t.Fatal(err)
	}
	if r := e.post("/orders", userA, key, `{"a":2}`); r.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("under 24 hours must replay: %d", r.status)
	}
}

func TestStoredRepliesHoldNoSecrets(t *testing.T) {
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		reply := map[string]any{"id": "o1", "handover_otp": "4821", "refresh_token": "T1", "nested": map[string]any{"password": "P1"}}
		err := database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error { return Save(r.Context(), tx, 200, reply) })
		if err != nil {
			httpserver.WriteError(w, 500, "internal_error", err.Error(), true)
			return
		}
		httpserver.WriteJSON(w, 200, reply)
	})
	first := e.post("/orders", userA, key, `{}`)
	if first.body["handover_otp"] != "4821" {
		t.Errorf("the live reply must keep the code: %v", first.body)
	}
	var stored string
	if err := e.pool.QueryRow(context.Background(), "SELECT response::text FROM idempotency_keys").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"4821", "T1", "P1", "refresh_token"} {
		if strings.Contains(stored, secret) {
			t.Errorf("the stored row holds %s: %s", secret, stored)
		}
	}
	if !strings.Contains(stored, `"id": "o1"`) || !strings.Contains(stored, `"handover_otp": null`) {
		t.Errorf("the rest was lost, or the required field is missing: %s", stored)
	}
	again := e.post("/orders", userA, key, `{}`)
	if again.header.Get("Idempotency-Replayed") != "true" || strings.Contains(again.raw, "4821") {
		t.Errorf("replay: %d %s", again.status, again.raw)
	}
}

func TestIgnoredFieldsAreNotFingerprinted(t *testing.T) {
	e := newEnv(t, Options{IgnoreBodyFields: []string{"password"}}, effectAndSave)
	e.post("/password", userA, key, `{"password":"hunter2-long","x":1}`)
	var hash string
	if err := e.pool.QueryRow(context.Background(), "SELECT request_hash FROM idempotency_keys").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == hashBody([]byte(`{"password":"hunter2-long","x":1}`), nil) || hash != hashBody([]byte(`{"x":1}`), []string{"password"}) {
		t.Errorf("the password shaped the stored hash")
	}
	if r := e.post("/password", userA, key, `{"password":"different-one","x":1}`); r.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("a retry must replay: %d", r.status)
	}
	if r := e.post("/password", userA, key, `{"password":"different-one","x":2}`); r.status != 422 {
		t.Errorf("another field changed: %d", r.status)
	}
}

func TestNoContentReplaysEmpty(t *testing.T) {
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		err := database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error { return Save(r.Context(), tx, 204, nil) })
		if err != nil {
			httpserver.WriteError(w, 500, "internal_error", "failed", true)
			return
		}
		w.WriteHeader(204)
	})
	e.post("/x", userA, key, ``)
	r := e.post("/x", userA, key, ``)
	if r.status != 204 || r.raw != "" || r.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("got %d %q %v", r.status, r.raw, r.header)
	}
}

func TestAHandlerThatForgetsSaveIsStillProtected(t *testing.T) {
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		httpserver.WriteJSON(w, 200, map[string]any{"ok": true})
	})
	e.post("/x", userA, key, `{}`)
	if r := e.post("/x", userA, key, `{}`); r.header.Get("Idempotency-Replayed") != "true" || r.body["ok"] != true {
		t.Errorf("got %d %v", r.status, r.body)
	}
	if e.runs.Load() != 1 {
		t.Errorf("ran %d times", e.runs.Load())
	}
}

func TestSaveOnlyStoresSuccessAndOnlyInsideTheMiddleware(t *testing.T) {
	var badStatus error
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		badStatus = database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error { return Save(r.Context(), tx, 409, map[string]any{}) })
		httpserver.WriteError(w, 409, "no", "no", false)
	})
	e.post("/x", userA, key, `{}`)
	if badStatus == nil {
		t.Error("a 409 must not be stored")
	}
	err := database.InTx(context.Background(), e.pool, func(tx pgx.Tx) error { return Save(context.Background(), tx, 200, map[string]any{}) })
	if err == nil {
		t.Error("Save outside the middleware must fail")
	}
}

func TestAFailedClaimSaysWhenToRetry(t *testing.T) {
	e := newEnv(t, Options{}, effectAndSave)
	e.pool.Close()
	r := e.post("/orders", userA, key, `{}`)
	if r.status != 503 || r.body["code"] != "service_unavailable" || r.body["retryable"] != true || r.header.Get("Retry-After") == "" {
		t.Errorf("got %d %v %v", r.status, r.body, r.header)
	}
	if e.runs.Load() != 0 {
		t.Error("the handler ran with no key held")
	}
}

func TestARollbackThatStillRepliesSuccessIsNotRepeated(t *testing.T) {
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		_ = database.InTx(r.Context(), e.pool, func(tx pgx.Tx) error {
			if err := Save(r.Context(), tx, 200, map[string]any{"ok": true}); err != nil {
				return err
			}
			return errors.New("rolled back")
		})
		httpserver.WriteJSON(w, 200, map[string]any{"ok": true}) // A handler bug.
	})
	e.post("/x", userA, key, `{}`)
	if r := e.post("/x", userA, key, `{}`); r.status != 200 || r.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("got %d %v", r.status, r.header)
	}
	if e.runs.Load() != 1 {
		t.Errorf("ran %d times", e.runs.Load())
	}
}

func TestANonJSONReplyIsNotRepeated(t *testing.T) {
	e := newEnv(t, Options{}, func(e *env, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain text"))
	})
	e.post("/x", userA, key, `{}`)
	if r := e.post("/x", userA, key, `{}`); r.status != 200 || r.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("got %d %v", r.status, r.header)
	}
	if e.runs.Load() != 1 {
		t.Errorf("ran %d times", e.runs.Load())
	}
}

func TestLockOutlastsTheRequestTimeout(t *testing.T) {
	if lockFor <= httpserver.RequestTimeout {
		t.Errorf("lock %s, request timeout %s", lockFor, httpserver.RequestTimeout)
	}
}
