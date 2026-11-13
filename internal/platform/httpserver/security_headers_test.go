package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every reply carries the security headers (#238): a route, a 404, a 405 and the health checks.
func TestEveryReplyCarriesTheSecurityHeaders(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewRouter(log, func(context.Context) error { return nil }, func(r chi.Router) {
		r.Get("/thing", func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
		})
	})
	want := map[string]string{
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":       "nosniff",
		"Cache-Control":                "no-store",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
		"Cross-Origin-Resource-Policy": "same-site",
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/thing"}, {"GET", "/v1/nothing-here"}, {"DELETE", "/v1/thing"}, {"GET", "/healthz"}, {"GET", "/readyz"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s %s: %s = %q, want %q", c.method, c.path, k, got, v)
			}
		}
	}
}
