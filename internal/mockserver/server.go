// Package mockserver serves sample answers for the customer, rider, partner and console contracts,
// so the app developers can build before the real backend exists. It has no
// database, and nothing here is used by the real services.
package mockserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
)

// Test values for the mock only. They are published to the app developers.
const (
	TestOTP          = "123456"
	ExpiredToken     = "mock-expired-token"
	scenarioHeader   = "X-Mock-Scenario"
	replayHeader     = "Idempotency-Replayed"
	idempotencyTTL   = 24 * time.Hour
	maxStoredReplies = 10000
	maxBodyBytes     = 1 << 20
	maxStreams       = 200
	maxStreamLife    = 30 * time.Minute
	oneShotReset     = time.Minute
)

// Server holds the mock's small amount of state: stored replies for repeated requests.
type Server struct {
	now       func() time.Time
	heartbeat time.Duration
	step      time.Duration

	mu     sync.Mutex
	stored map[string]storedReply

	streams chan struct{}
	oneShot map[string]time.Time
}

type storedReply struct {
	bodyHash string
	status   int
	body     []byte
	at       time.Time
}

// New returns a mock server.
func New() *Server {
	return &Server{now: time.Now, heartbeat: 15 * time.Second, step: 3 * time.Second, stored: map[string]storedReply{}, streams: make(chan struct{}, maxStreams), oneShot: map[string]time.Time{}}
}

type route struct {
	method, path string
	open         bool
	noIdempotent bool
	handle       func(*call)
}

// call is one request, with its scenario.
type call struct {
	w        http.ResponseWriter
	r        *http.Request
	s        *Server
	scenario string
	now      time.Time
}

func (c *call) param(name string) string { return chi.URLParam(c.r, name) }

func (c *call) json(status int, body map[string]any) {
	body["server_time"] = c.now.UTC().Format(time.RFC3339Nano)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_ = json.NewEncoder(c.w).Encode(body)
}

func (c *call) fail(status int, code, message string, retryable bool) {
	body := map[string]any{"code": code, "message": message, "retryable": retryable}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || code == "idempotency_in_progress" {
		c.w.Header().Set("Retry-After", "5")
	}
	c.json(status, body)
}

// Handler returns the routes for both contracts, the docs and the contract files.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.home)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/scenarios", s.listScenarios)
	r.Get("/docs/{app}", s.docs)
	r.Get("/contracts/{file}", s.contractFile)
	r.Route("/customer/v1", func(sub chi.Router) {
		for _, rt := range customerRoutes() {
			sub.MethodFunc(rt.method, rt.path, s.wrap(rt))
		}
		sub.Get("/orders/{order_id}/stream", s.customerStream)
	})
	r.Route("/rider/v1", func(sub chi.Router) {
		for _, rt := range riderRoutes() {
			sub.MethodFunc(rt.method, rt.path, s.wrap(rt))
		}
		sub.Get("/riders/me/stream", s.riderStream)
	})
	r.Route("/partner/v1", func(sub chi.Router) {
		for _, rt := range partnerRoutes() {
			sub.MethodFunc(rt.method, rt.path, s.wrap(rt))
		}
		sub.Get("/hub/stream", s.hubStream)
	})
	r.Route("/console/v1", func(sub chi.Router) {
		for _, rt := range consoleRoutes() {
			sub.MethodFunc(rt.method, rt.path, s.wrap(rt))
		}
	})
	return r
}

// wrap applies the shared rules in contracts/README.md: sign-in and idempotency.
func (s *Server) wrap(rt route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &call{w: w, r: r, s: s, scenario: r.Header.Get(scenarioHeader), now: s.now()}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if !rt.open {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			switch token {
			case "", r.Header.Get("Authorization"):
				c.fail(http.StatusUnauthorized, "unauthorized", "Send Authorization: Bearer <token>.", false)
				return
			case ExpiredToken:
				c.fail(http.StatusUnauthorized, "token_expired", "The access token has expired.", false)
				return
			}
		}
		if rt.method == http.MethodGet || rt.noIdempotent {
			rt.handle(c)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) < 8 || len(key) > 128 {
			c.fail(http.StatusBadRequest, "idempotency_key_required", "Send an Idempotency-Key of 8 to 128 characters.", false)
			return
		}
		if c.scenario == "idempotency_in_progress" {
			c.fail(http.StatusConflict, "idempotency_in_progress", "This key is still being processed.", true)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			c.fail(http.StatusBadRequest, "invalid_request", "The body is too large.", false)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		sum := sha256.Sum256(raw)
		bodyHash := hex.EncodeToString(sum[:])
		// The scenario is part of the key, so trying another scenario never replays an old answer.
		storeKey := r.Header.Get("Authorization") + "|" + c.scenario + "|" + r.Method + "|" + r.URL.Path + "|" + key
		if prior, ok := s.lookup(storeKey, c.now); ok {
			if prior.bodyHash != bodyHash {
				c.fail(http.StatusUnprocessableEntity, "idempotency_key_reused", "This key was used with a different body.", false)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(replayHeader, "true")
			w.WriteHeader(prior.status)
			_, _ = w.Write(prior.body)
			return
		}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		c.w = rec
		rt.handle(c)
		if rec.status >= 200 && rec.status < 300 {
			s.store(storeKey, storedReply{bodyHash: bodyHash, status: rec.status, body: rec.body, at: c.now})
		}
	}
}

// firstTime is true once per key each minute. A stream that drops or expires does it
// once, so an app's normal reconnect then works, and the scenario can be tried again later.
func (s *Server) firstTime(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at, ok := s.oneShot[key]; ok && now.Sub(at) < oneShotReset {
		return false
	}
	if len(s.oneShot) >= maxStoredReplies {
		s.oneShot = map[string]time.Time{}
	}
	s.oneShot[key] = now
	return true
}

func (s *Server) lookup(key string, now time.Time) (storedReply, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply, ok := s.stored[key]
	if !ok || now.Sub(reply.at) > idempotencyTTL {
		return storedReply{}, false
	}
	return reply, true
}

func (s *Server) store(key string, reply storedReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stored) >= maxStoredReplies {
		s.stored = map[string]storedReply{}
	}
	s.stored[key] = reply
}

type recorder struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return r.ResponseWriter.Write(b)
}

func (s *Server) contractFile(w http.ResponseWriter, r *http.Request) {
	data, err := contracts.Dist.ReadFile("dist/" + chi.URLParam(r, "file"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) docs(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	if app != "customer" && app != "rider" && app != "partner" && app != "console" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>Ninzo ` + app + ` API</title></head><body>
<redoc spec-url="/contracts/` + app + `.openapi.yaml"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.5.0/bundles/redoc.standalone.js"></script>
</body></html>`))
}

func (s *Server) home(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>Ninzo mock server</title></head><body>
<h1>Ninzo mock server</h1>
<p>Sample answers only. There is no real data here.</p>
<ul>
<li><a href="/docs/customer">Customer API docs</a> and <a href="/contracts/customer.openapi.yaml">contract file</a></li>
<li><a href="/docs/rider">Rider API docs</a> and <a href="/contracts/rider.openapi.yaml">contract file</a></li>
<li><a href="/docs/partner">Partner (hub) API docs</a> and <a href="/contracts/partner.openapi.yaml">contract file</a></li>
<li><a href="/docs/console">Console API docs</a> and <a href="/contracts/console.openapi.yaml">contract file</a></li>
<li><a href="/contracts/customer.stream.asyncapi.yaml">Customer stream</a>, <a href="/contracts/rider.stream.asyncapi.yaml">rider stream</a> and <a href="/contracts/partner.stream.asyncapi.yaml">hub stream</a></li>
<li><a href="/scenarios">All scenarios</a></li>
</ul>
<p>Choose a scenario with the X-Mock-Scenario header. On a live stream, you can use ?scenario=name instead.</p>
</body></html>`))
}

func (s *Server) listScenarios(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"header":   scenarioHeader,
		"test_otp": TestOTP,
		"customer": customerScenarios,
		"rider":    riderScenarios,
		"partner":  partnerScenarios,
		"console":  consoleScenarios,
	})
}
