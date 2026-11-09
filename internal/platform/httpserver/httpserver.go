// Package httpserver holds what every API shares: the router, health checks, the one error
// shape, request logging and graceful shutdown. It has no business rules.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	maxBodyBytes = 1 << 20
	// RequestTimeout limits a normal request. Idempotency locks must outlast it.
	RequestTimeout  = 20 * time.Second
	readyTimeout    = 10 * time.Second               // Longer than database.PingTimeout, or a far database reads as down.
	shutdownTimeout = RequestTimeout + 5*time.Second // Longer, so no legal request is cut off.
)

// WriteJSON sends a JSON reply.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError sends the one error shape in contracts/shared/common.yaml.
func WriteError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	WriteJSON(w, status, map[string]any{
		"code":        code,
		"message":     message,
		"retryable":   retryable,
		"server_time": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// ClientIP is the caller's network address. header names the one the platform's proxy sets. Without
// it, the connection's address is used. An IPv6 address counts as its /64, the block one customer
// usually holds, so moving around inside it does not reset a limit.
func ClientIP(r *http.Request, header string) string {
	if ip, ok := headerIP(r, header); ok {
		return normalizeIP(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	return normalizeIP(ip)
}

// HasClientIP reports whether the request carries a usable address in that header. If it never does,
// the header is misnamed, and every caller is being counted as the proxy.
func HasClientIP(r *http.Request, header string) bool {
	_, ok := headerIP(r, header)
	return ok
}

// headerIP reads the last value, across repeated headers and lists. That is the one the nearest
// proxy added, and a client writes the rest.
func headerIP(r *http.Request, header string) (netip.Addr, bool) {
	if header == "" {
		return netip.Addr{}, false
	}
	lines := r.Header.Values(header)
	if len(lines) == 0 {
		return netip.Addr{}, false
	}
	values := strings.Split(lines[len(lines)-1], ",")
	ip, err := netip.ParseAddr(strings.TrimSpace(values[len(values)-1]))
	return ip, err == nil
}

func normalizeIP(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		if p, err := ip.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip.String()
}

type requestIDKey struct{}

// RequestID returns the ID of this request, for logs.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// A client's own X-Request-Id is ignored, so nobody can write chosen text into the logs.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			panic(err) // crypto/rand does not fail on supported systems.
		}
		id := hex.EncodeToString(raw)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// withLogAndRecover logs each request and turns a panic into the error shape. It logs the
// route pattern, never the raw path, query, headers or body, because those can hold phone
// numbers, codes and tokens.
func withLogAndRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			abort := false
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic", "request_id", RequestID(r.Context()), "stack", string(debug.Stack()))
				if rec.status == 0 {
					WriteError(rec, http.StatusInternalServerError, "internal_error", "Something went wrong.", true)
				} else {
					// Part of a reply has gone. Break the connection, so the client sees a failure
					// and not a cut-off reply that looks like a success.
					abort = true
				}
			}
			if rec.status == 0 {
				rec.status = http.StatusOK // What net/http sends for a handler that writes nothing.
			}
			defer func() {
				if abort {
					panic(http.ErrAbortHandler)
				}
			}()
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			level := slog.LevelInfo
			if route == "/healthz" || route == "/readyz" {
				level = slog.LevelDebug
			}
			log.Log(r.Context(), level, "request", "request_id", RequestID(r.Context()), "method", r.Method,
				"route", route, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

// withLimits applies to normal requests. A live stream skips it, because a stream built on this
// context would die at RequestTimeout. A stream calls ForStream for the server's ReadTimeout and
// WriteTimeout, and is cut off when the process stops, which the app handles by reconnecting.
func withLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStream(r) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), RequestTimeout)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isStream says a request asks to become a WebSocket stream.
func isStream(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// ForStream readies a request to become a long-lived stream: it clears the read and write
// deadlines the server puts on every connection. Go clears them itself when a connection is
// taken over, so this is a safety net in case that changes. Call it before taking over.
func ForStream(w http.ResponseWriter) error {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	return rc.SetWriteDeadline(time.Time{})
}

// NewRouter builds an API's router. mount adds its endpoints under /v1, and may be nil.
//
// /healthz says the process is up, so a platform only restarts it if it is stuck. /readyz also
// checks the database, so it can fail while the pool is merely busy: use it to hold back traffic
// or a deploy, never to restart a process. Neither says why it failed, because they are public.
func NewRouter(log *slog.Logger, ready func(context.Context) error, mount func(chi.Router)) http.Handler {
	r := chi.NewRouter()
	r.Use(withRequestID)
	r.Use(func(next http.Handler) http.Handler { return withLogAndRecover(log, next) })
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.", false)
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), readyTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			log.Warn("not ready", "request_id", RequestID(req.Context()), "error", err.Error())
			WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.Route("/v1", func(v1 chi.Router) {
		v1.Use(withLimits)
		if mount != nil {
			mount(v1)
		}
	})
	return r
}

// Serve runs until ctx ends, then lets requests in progress finish.
func Serve(ctx context.Context, port int, handler http.Handler, log *slog.Logger) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("could not listen on port %d: %w", port, err)
	}
	return serve(ctx, ln, handler, log)
}

func serve(ctx context.Context, ln net.Listener, handler http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	stop, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(stop); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
