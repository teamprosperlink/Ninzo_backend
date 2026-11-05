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
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	maxBodyBytes    = 1 << 20
	requestTimeout  = 25 * time.Second
	readyTimeout    = 2 * time.Second
	shutdownTimeout = 15 * time.Second
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
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic", "request_id", RequestID(r.Context()), "stack", string(debug.Stack()))
				if rec.status == 0 {
					WriteError(rec, http.StatusInternalServerError, "internal_error", "Something went wrong.", true)
				}
			}
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

func withLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// NewRouter builds an API's router. mount adds its endpoints under /v1, and may be nil.
//
// /healthz says the process is up, so a platform only restarts it if it is stuck. /readyz also
// checks the database. Neither says why it failed, because they are public.
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
		WriteTimeout:      requestTimeout + 5*time.Second,
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
