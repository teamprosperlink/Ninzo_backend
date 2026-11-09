package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

func ready(context.Context) error { return nil }

func newTestServer(t *testing.T, readyFn func(context.Context) error, mount func(chi.Router)) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(NewRouter(log, readyFn, mount))
	t.Cleanup(srv.Close)
	return srv, &logs
}

func get(t *testing.T, url string) (int, map[string]any, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("%s: the body is not JSON: %v", url, err)
	}
	return resp.StatusCode, body, resp.Header
}

// checkErrorShape reads the required fields from the contract itself, so the two cannot drift.
func checkErrorShape(t *testing.T, body map[string]any) {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/shared/common.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				Error struct {
					Required []string `yaml:"required"`
				} `yaml:"Error"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Components.Schemas.Error.Required) == 0 {
		t.Fatal("no required fields found, so this check would prove nothing")
	}
	for _, field := range doc.Components.Schemas.Error.Required {
		if _, ok := body[field]; !ok {
			t.Errorf("the error has no %s: %v", field, body)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, body["server_time"].(string)); err != nil {
		t.Errorf("server_time: %v", err)
	}
}

func TestHealthChecks(t *testing.T) {
	srv, _ := newTestServer(t, ready, nil)
	if code, body, _ := get(t, srv.URL+"/healthz"); code != 200 || body["status"] != "ok" {
		t.Errorf("healthz: %d %v", code, body)
	}
	if code, body, _ := get(t, srv.URL+"/readyz"); code != 200 || body["status"] != "ready" {
		t.Errorf("readyz: %d %v", code, body)
	}
}

func TestNotReadyHidesTheReason(t *testing.T) {
	srv, logs := newTestServer(t, func(context.Context) error { return errors.New("db host is secret.example") }, nil)
	code, body, _ := get(t, srv.URL+"/readyz")
	if code != 503 || body["status"] != "not_ready" {
		t.Errorf("readyz: %d %v", code, body)
	}
	if strings.Contains(body["status"].(string), "secret.example") || len(body) != 1 {
		t.Errorf("the public reply says too much: %v", body)
	}
	if !strings.Contains(logs.String(), "secret.example") {
		t.Error("the reason must reach the logs")
	}
	// A database outage must not make a platform restart a healthy process.
	if code, _, _ := get(t, srv.URL+"/healthz"); code != 200 {
		t.Errorf("healthz during an outage: %d", code)
	}
}

func TestErrorsUseTheContractShape(t *testing.T) {
	srv, _ := newTestServer(t, ready, func(r chi.Router) {
		r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
		r.Get("/only-get", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	})
	code, body, _ := get(t, srv.URL+"/nothing")
	if code != 404 || body["code"] != "not_found" {
		t.Errorf("404: %d %v", code, body)
	}
	checkErrorShape(t, body)

	code, body, _ = get(t, srv.URL+"/v1/nothing")
	if code != 404 || body["code"] != "not_found" {
		t.Errorf("404 under /v1: %d %v", code, body)
	}
	checkErrorShape(t, body)

	code, body, _ = get(t, srv.URL+"/v1/boom")
	if code != 500 || body["code"] != "internal_error" || body["retryable"] != true {
		t.Errorf("panic: %d %v", code, body)
	}
	checkErrorShape(t, body)
	if strings.Contains(body["message"].(string), "boom") {
		t.Errorf("the panic reached the client: %v", body)
	}

	resp, err := http.Post(srv.URL+"/v1/only-get", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var posted map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&posted); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 405 || posted["code"] != "method_not_allowed" {
		t.Errorf("405: %d %v", resp.StatusCode, posted)
	}
	checkErrorShape(t, posted)
}

func TestPanicAfterAPartialReplyLooksLikeAFailure(t *testing.T) {
	srv, logs := newTestServer(t, ready, func(r chi.Router) {
		r.Get("/half", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"order_id":"abc","total":`))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush: %v", err)
			}
			panic("boom")
		})
		r.Get("/half-chunked", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"order_id":"abc","total":`))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush: %v", err)
			}
			panic("boom")
		})
	})
	for _, path := range []string{"/v1/half", "/v1/half-chunked"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			continue // The connection broke before the headers arrived, which is also a failure.
		}
		_, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr == nil {
			t.Errorf("%s: the client read a cut-off reply with no error", path)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"panic"`) || !strings.Contains(logs.String(), `"route":"/v1/half"`) {
		t.Errorf("the panic and the request must both reach the logs: %s", logs.String())
	}
}

func TestAHandlerThatWritesNothingLogsTheRealStatus(t *testing.T) {
	srv, logs := newTestServer(t, ready, func(r chi.Router) {
		r.Get("/quiet", func(http.ResponseWriter, *http.Request) {})
	})
	resp, err := http.Get(srv.URL + "/v1/quiet")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(logs.String(), `"status":200`) {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestNoLegalRequestOutlastsShutdown(t *testing.T) {
	if RequestTimeout >= shutdownTimeout {
		t.Errorf("request timeout %s, shutdown timeout %s", RequestTimeout, shutdownTimeout)
	}
}

func TestServiceKeepsRunningAfterAPanic(t *testing.T) {
	srv, logs := newTestServer(t, ready, func(r chi.Router) {
		r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	})
	get(t, srv.URL+"/v1/boom")
	if code, _, _ := get(t, srv.URL+"/healthz"); code != 200 {
		t.Errorf("after a panic: %d", code)
	}
	if !strings.Contains(logs.String(), `"msg":"panic"`) {
		t.Error("the panic must reach the logs")
	}
}

func TestRequestIDsAreOursAndUnique(t *testing.T) {
	srv, _ := newTestServer(t, ready, nil)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "chosen by client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	first := resp.Header.Get("X-Request-Id")
	if len(first) != 32 || first == "chosen by client" {
		t.Errorf("request ID: %q", first)
	}
	_, _, h := get(t, srv.URL+"/healthz")
	if h.Get("X-Request-Id") == first {
		t.Error("two requests share an ID")
	}
}

func TestLogsHoldNoSensitiveData(t *testing.T) {
	srv, logs := newTestServer(t, ready, func(r chi.Router) {
		r.Post("/gates/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/gates/PATH9876?phone=QUERY9876", strings.NewReader(`{"otp":"BODY9876"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer TOKEN9876")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	out := logs.String()
	for _, secret := range []string{"PATH9876", "QUERY9876", "BODY9876", "TOKEN9876"} {
		if strings.Contains(out, secret) {
			t.Errorf("the logs hold %s: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"route":"/v1/gates/{id}"`) || !strings.Contains(out, `"status":200`) {
		t.Errorf("the log line is missing the route or status: %s", out)
	}
}

func TestBodyIsLimited(t *testing.T) {
	var readErr error
	srv, _ := newTestServer(t, ready, func(r chi.Router) {
		r.Post("/big", func(w http.ResponseWriter, req *http.Request) {
			_, readErr = io.Copy(io.Discard, req.Body)
			w.WriteHeader(204)
		})
	})
	resp, err := http.Post(srv.URL+"/v1/big", "application/json", strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var tooBig *http.MaxBytesError
	if !errors.As(readErr, &tooBig) {
		t.Errorf("an oversized body must be refused, got %v", readErr)
	}
}

func TestRequestsHaveADeadline(t *testing.T) {
	var has bool
	srv, _ := newTestServer(t, ready, func(r chi.Router) {
		r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
			_, has = req.Context().Deadline()
			w.WriteHeader(204)
		})
	})
	resp, err := http.Get(srv.URL + "/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !has {
		t.Error("a request under /v1 has no deadline")
	}
}

func TestShutdownLetsRequestsFinish(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("done"))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, ln, mux, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	reply := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			reply <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		reply <- string(b)
	}()
	<-started
	cancel()
	select {
	case err := <-stopped:
		t.Fatalf("stopped before the request finished: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if got := <-reply; got != "done" {
		t.Errorf("the request in progress got %q", got)
	}
	if err := <-stopped; err != nil {
		t.Errorf("a clean shutdown returned %v", err)
	}
}

func TestARepeatedHeaderUsesTheLastValue(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("X-Real-IP", "6.6.6.6")
	r.Header.Add("X-Real-IP", "9.9.9.9")
	if got := ClientIP(r, "X-Real-IP"); got != "9.9.9.9" {
		t.Errorf("got %s", got)
	}
	if !HasClientIP(r, "X-Real-IP") {
		t.Error("the header is usable")
	}
	if HasClientIP(r, "X-Misspelled") {
		t.Error("that header is missing")
	}
}

func TestClientIP(t *testing.T) {
	for _, c := range []struct{ name, remote, header, value, want string }{
		{"no header set up", "1.2.3.4:5", "", "9.9.9.9", "1.2.3.4"},
		{"the proxy's header", "1.2.3.4:5", "X-Real-IP", "9.9.9.9", "9.9.9.9"},
		{"a client's forged list", "1.2.3.4:5", "X-Forwarded-For", "6.6.6.6, 9.9.9.9", "9.9.9.9"},
		{"nothing in the header", "1.2.3.4:5", "X-Real-IP", "", "1.2.3.4"},
		{"junk in the header", "1.2.3.4:5", "X-Real-IP", "not-an-ip", "1.2.3.4"},
		{"IPv6 counts as its block", "1.2.3.4:5", "X-Real-IP", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"the same block", "1.2.3.4:5", "X-Real-IP", "2001:db8:1:2:ffff::1", "2001:db8:1:2::/64"},
		{"IPv4 written as IPv6", "1.2.3.4:5", "X-Real-IP", "::ffff:9.9.9.9", "9.9.9.9"},
		{"IPv6 connection", "[2001:db8::1]:5", "", "", "2001:db8::/64"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		if got := ClientIP(r, c.header); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A live stream must outlive RequestTimeout and the server's read and write timeouts. This runs
// the real server, so it takes about half a minute.
func TestAStreamOutlivesTheRequestLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	handler := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), ready, func(r chi.Router) {
		r.Get("/stream", func(w http.ResponseWriter, req *http.Request) {
			if err := ForStream(w); err != nil {
				result <- "ForStream: " + err.Error()
				return
			}
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				result <- "hijack: " + err.Error()
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = buf.Flush()
			// Read past the server's ReadTimeout, then write past its WriteTimeout and RequestTimeout.
			line, err := buf.ReadString('\n')
			if err != nil {
				result <- "read: " + err.Error()
				return
			}
			time.Sleep(time.Until(start.Add(WriteTimeoutForTest() + 2*time.Second)))
			state := "live"
			if req.Context().Err() != nil {
				state = "context ended"
			}
			if _, err := conn.Write([]byte(state + " " + line)); err != nil {
				result <- "write: " + err.Error()
				return
			}
			result <- "ok"
		})
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = serve(ctx, ln, handler, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	start = time.Now()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /v1/stream HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	time.Sleep(17 * time.Second)
	_, _ = conn.Write([]byte("hello\n"))
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reply, _ := io.ReadAll(io.LimitReader(conn, 4096))
	if got := <-result; got != "ok" {
		t.Fatalf("the stream was cut off: %s", got)
	}
	if !strings.Contains(string(reply), "live hello") {
		t.Errorf("reply %q", reply)
	}
}

var start time.Time

// WriteTimeoutForTest is the server's write timeout, the longest of its limits.
func WriteTimeoutForTest() time.Duration { return RequestTimeout + 5*time.Second }
