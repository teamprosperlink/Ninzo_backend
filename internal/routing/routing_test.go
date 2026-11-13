package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two points in the Indiranagar test zone, about 1 km apart.
var (
	gate  = Point{Lat: 12.97194, Lng: 77.64115}
	rider = Point{Lat: 12.97840, Lng: 77.64080}
)

// fakeOSRM answers like OSRM's HTTP API, with per-test overrides.
type fakeOSRM struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	calls    atomic.Int64
	// handle answers one request; nil uses the default good answer.
	handle func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool
}

func newFake(t *testing.T) (*fakeOSRM, *httptest.Server) {
	f := &fakeOSRM{t: t}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOSRM) serve(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.String())
	f.mu.Unlock()
	// /{service}/v1/{profile}/{coords}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4)
	if len(parts) != 4 || parts[1] != "v1" {
		http.Error(w, "bad path", http.StatusNotFound)
		return
	}
	service := parts[0]
	var coords []Point
	for _, c := range strings.Split(parts[3], ";") {
		ll := strings.Split(c, ",")
		lng, _ := strconv.ParseFloat(ll[0], 64)
		lat, _ := strconv.ParseFloat(ll[1], 64)
		coords = append(coords, Point{Lat: lat, Lng: lng})
	}
	if f.handle != nil && f.handle(w, r, service, coords) {
		return
	}
	q := r.URL.Query()
	switch service {
	case "route":
		// Like OSRM: without geometries=geojson the line is an encoded polyline string.
		var geometry any = "_p~iF~ps|U_ulLnnqC"
		if q.Get("geometries") == "geojson" {
			geometry = map[string]any{"type": "LineString", "coordinates": [][]float64{{coords[0].Lng, coords[0].Lat}, {coords[1].Lng, coords[1].Lat}}}
		}
		writeJSON(w, map[string]any{"code": "Ok", "routes": []any{map[string]any{
			"duration": 180.5, "distance": 1420.0, "geometry": geometry,
		}}})
	case "table":
		n := len(strings.Split(q.Get("sources"), ";"))
		durs, dists := make([][]any, n), make([][]any, n)
		for i := range n {
			// The source index (within the chunk) is encoded in the answer so order is testable.
			durs[i] = []any{100.0 + float64(i)}
			dists[i] = []any{1000.0 + float64(i)}
		}
		// Like OSRM: distances only when annotations asks for them; durations is the default.
		reply := map[string]any{"code": "Ok"}
		ann := q.Get("annotations")
		if ann == "" || strings.Contains(ann, "duration") {
			reply["durations"] = durs
		}
		if strings.Contains(ann, "distance") {
			reply["distances"] = dists
		}
		writeJSON(w, reply)
	case "nearest":
		writeJSON(w, map[string]any{"code": "Ok", "waypoints": []any{map[string]any{
			"location": []float64{coords[0].Lng + 0.0001, coords[0].Lat}, "distance": 10.8,
		}}})
	default:
		http.Error(w, "unknown service", http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func client(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRouteFromOSRM(t *testing.T) {
	f, srv := newFake(t)
	c := client(t, Config{BaseURL: srv.URL, Profile: "scooter"})
	r, err := c.Route(context.Background(), rider, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceOSRM || !r.Reachable || r.Seconds != 180.5 || r.Metres != 1420 || len(r.Line) != 2 {
		t.Fatalf("got %+v", r)
	}
	if r.Line[0] != rider {
		t.Fatalf("line starts at %+v, want the rider (lat,lng not swapped)", r.Line[0])
	}
	// OSRM wants lng,lat, our profile name, and a GeoJSON line.
	for _, p := range []string{"overview=full", "geometries=geojson", "alternatives=false", "steps=false"} {
		if !strings.Contains(f.requests[0], p) {
			t.Errorf("request %q lacks %s", f.requests[0], p)
		}
	}
	want := "/route/v1/scooter/77.640800,12.978400;77.641150,12.971940"
	if !strings.HasPrefix(f.requests[0], want) {
		t.Fatalf("request %q, want prefix %q", f.requests[0], want)
	}
	if s := c.Stats(); s.Calls != 1 || s.OSRM != 1 || s.Fallbacks != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestNoURLMeansAStraightLine(t *testing.T) {
	c := client(t, Config{})
	r, err := c.Route(context.Background(), rider, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	m := Metres(rider, gate) // equal to geo.Metres: checked in internal/geo (distance_test.go)
	if r.Source != SourcePlaceholder || !r.Reachable || math.Abs(r.Metres-m) > 1e-6 || math.Abs(r.Seconds-m/5) > 1e-6 {
		t.Fatalf("got %+v, want %.1f m at 18 km/h (%.1f s)", r.Leg, m, m/5)
	}
	legs, err := c.Table(context.Background(), []Point{rider, gate}, gate, 18)
	if err != nil || len(legs) != 2 || legs[0].Source != SourcePlaceholder || legs[1].Metres != 0 {
		t.Fatalf("table %+v %v", legs, err)
	}
	s, err := c.Nearest(context.Background(), rider)
	if err != nil || s.Point != rider || s.Source != SourcePlaceholder || s.Moved != 0 {
		t.Fatalf("nearest %+v %v", s, err)
	}
	if st := c.Stats(); st.Calls != 3 || st.Fallbacks != 3 || st.Failures != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestTableKeepsOrderAndSplitsLargeRequests(t *testing.T) {
	f, srv := newFake(t)
	var sizes []int
	f.handle = func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool {
		if service == "table" {
			sizes = append(sizes, len(coords))
			q := r.URL.Query()
			ns, nd := len(strings.Split(q.Get("sources"), ";")), len(strings.Split(q.Get("destinations"), ";"))
			if ns*nd > 2*2 {
				// What OSRM does past --max-table-size (2 here): sources x destinations > 2 squared.
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"code": "TooBig"})
				return true
			}
			if got := r.URL.Query().Get("destinations"); got != strconv.Itoa(len(coords)-1) {
				t.Errorf("destinations=%s, want the last coordinate %d", got, len(coords)-1)
			}
			if coords[len(coords)-1] != gate {
				t.Errorf("last coordinate %+v, want the gate", coords[len(coords)-1])
			}
		}
		return false
	}
	c := client(t, Config{BaseURL: srv.URL, MaxTable: 2})
	sources := make([]Point, 11)
	for i := range sources {
		sources[i] = Point{Lat: 12.97 + float64(i)/1000, Lng: 77.64}
	}
	legs, err := c.Table(context.Background(), sources, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sizes) != "[5 5 4]" {
		t.Fatalf("chunk sizes %v, want [5 5 4] (4 sources + the gate each)", sizes)
	}
	if len(legs) != 11 {
		t.Fatalf("%d legs for 11 sources", len(legs))
	}
	for i, l := range legs {
		if l.Source != SourceOSRM || !l.Reachable || l.Seconds != 100+float64(i%4) {
			t.Fatalf("leg %d = %+v, want OSRM, %v s", i, l, 100+float64(i%4))
		}
	}
	if st := c.Stats(); st.Calls != 1 || st.OSRM != 1 || st.Fallbacks != 0 {
		t.Fatalf("one Table call is one call, whatever the chunks: %+v", st)
	}
}

func TestEmptyTableCallsNothing(t *testing.T) {
	f, srv := newFake(t)
	c := client(t, Config{BaseURL: srv.URL})
	legs, err := c.Table(context.Background(), nil, gate, 18)
	if err != nil || len(legs) != 0 || f.calls.Load() != 0 {
		t.Fatalf("legs %v err %v calls %d", legs, err, f.calls.Load())
	}
}

func TestNoRoadIsUnreachableNotAStraightLine(t *testing.T) {
	f, srv := newFake(t)
	f.handle = func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool {
		switch service {
		case "route":
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"code": "NoRoute", "message": "Impossible route between points"})
			return true
		case "table":
			writeJSON(w, map[string]any{"code": "Ok",
				"durations": [][]any{{120.0}, {nil}, {60.0}},
				"distances": [][]any{{900.0}, {nil}, {400.0}}})
			return true
		}
		return false
	}
	c := client(t, Config{BaseURL: srv.URL})
	r, err := c.Route(context.Background(), rider, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reachable || r.Source != SourceOSRM || r.Seconds != 0 {
		t.Fatalf("NoRoute gave %+v, want unreachable from OSRM", r.Leg)
	}
	legs, err := c.Table(context.Background(), []Point{rider, rider, rider}, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if !legs[0].Reachable || legs[1].Reachable || legs[1].Source != SourceOSRM || !legs[2].Reachable || legs[2].Seconds != 60 {
		t.Fatalf("legs %+v", legs)
	}
	if st := c.Stats(); st.Failures != 0 || st.Fallbacks != 0 {
		t.Fatalf("no road is an answer, not a failure: %+v", st)
	}
}

func TestBadRepliesFallBack(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, service string){
		"server error": func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusInternalServerError) },
		"not json":     func(w http.ResponseWriter, _ string) { _, _ = w.Write([]byte("<html>")) },
		"code not Ok":  func(w http.ResponseWriter, _ string) { writeJSON(w, map[string]any{"code": "InvalidQuery"}) },
		"no routes":    func(w http.ResponseWriter, _ string) { writeJSON(w, map[string]any{"code": "Ok", "routes": []any{}}) },
		"negative duration": func(w http.ResponseWriter, s string) {
			writeJSON(w, map[string]any{"code": "Ok", "routes": []any{map[string]any{"duration": -1.0, "distance": 5.0,
				"geometry": map[string]any{"coordinates": [][]float64{{77.6, 12.9}, {77.6, 12.9}}}}},
				"durations": [][]any{{-1.0}}, "distances": [][]any{{5.0}},
				"waypoints": []any{map[string]any{"location": []float64{77.6, 12.9}, "distance": -1.0}}})
		},
		"missing duration": func(w http.ResponseWriter, s string) {
			writeJSON(w, map[string]any{"code": "Ok", "routes": []any{map[string]any{"distance": 5.0,
				"geometry": map[string]any{"coordinates": [][]float64{{77.6, 12.9}, {77.6, 12.9}}}}},
				"durations": [][]any{{nil}}, "distances": [][]any{{5.0}},
				"waypoints": []any{map[string]any{"location": []float64{77.6}, "distance": 1.0}}})
		},
		"wrong row count": func(w http.ResponseWriter, s string) {
			writeJSON(w, map[string]any{"code": "Ok", "routes": []any{map[string]any{"duration": 1.0, "distance": 5.0,
				"geometry": map[string]any{"coordinates": [][]float64{{77.6, 12.9}}}}},
				"durations": [][]any{{1.0}, {2.0}}, "distances": [][]any{{1.0}, {2.0}},
				"waypoints": []any{map[string]any{"location": []float64{777, 12.9}, "distance": 1.0}}})
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			f.handle = func(w http.ResponseWriter, _ *http.Request, service string, _ []Point) bool {
				answer(w, service)
				return true
			}
			c := client(t, Config{BaseURL: srv.URL, Failures: 100})
			r, err := c.Route(context.Background(), rider, gate, 18)
			if err != nil || r.Source != SourcePlaceholder || !r.Reachable {
				t.Fatalf("route %+v %v", r.Leg, err)
			}
			legs, err := c.Table(context.Background(), []Point{rider}, gate, 18)
			if err != nil || legs[0].Source != SourcePlaceholder {
				t.Fatalf("table %+v %v", legs, err)
			}
			s, err := c.Nearest(context.Background(), rider)
			if err != nil || s.Source != SourcePlaceholder || s.Point != rider {
				t.Fatalf("nearest %+v %v", s, err)
			}
			if st := c.Stats(); st.Failures != 3 || st.Fallbacks != 3 || st.OSRM != 0 {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

func TestSlowOSRMTimesOutAndFallsBack(t *testing.T) {
	f, srv := newFake(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.handle = func(w http.ResponseWriter, r *http.Request, _ string, _ []Point) bool {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	}
	c := client(t, Config{BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	start := time.Now()
	r, err := c.Route(context.Background(), rider, gate, 18)
	if err != nil || r.Source != SourcePlaceholder {
		t.Fatalf("got %+v %v", r.Leg, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v, want about the 50 ms timeout", d)
	}
	if st := c.Stats(); st.Failures != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestTheBreakerOpensThenRecovers(t *testing.T) {
	f, srv := newFake(t)
	var down atomic.Bool
	down.Store(true)
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ string, _ []Point) bool {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	c := client(t, Config{BaseURL: srv.URL, Failures: 3, Cooldown: 10 * time.Second, Now: func() time.Time { return now }})
	ctx := context.Background()

	for range 3 {
		if r, _ := c.Route(ctx, rider, gate, 18); r.Source != SourcePlaceholder {
			t.Fatal("want fallback while down")
		}
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("%d requests, want 3 before the breaker opens", got)
	}
	// Open: no request reaches OSRM.
	for range 5 {
		_, _ = c.Route(ctx, rider, gate, 18)
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("%d requests while open, want still 3", got)
	}
	if st := c.Stats(); st.BreakerOpens != 1 || st.Fallbacks != 8 {
		t.Fatalf("stats %+v", st)
	}
	// After the cool-down one probe goes through; it fails, so the breaker opens again at once.
	now = now.Add(11 * time.Second)
	_, _ = c.Route(ctx, rider, gate, 18)
	_, _ = c.Route(ctx, rider, gate, 18)
	if got := f.calls.Load(); got != 4 {
		t.Fatalf("%d requests, want one probe", got)
	}
	if st := c.Stats(); st.BreakerOpens != 2 {
		t.Fatalf("a failed probe reopens: %+v", st)
	}
	// OSRM comes back: the next probe succeeds and closes the breaker.
	down.Store(false)
	now = now.Add(11 * time.Second)
	for range 3 {
		if r, _ := c.Route(ctx, rider, gate, 18); r.Source != SourceOSRM {
			t.Fatalf("want OSRM after recovery, got %+v", r.Leg)
		}
	}
	if got := f.calls.Load(); got != 7 {
		t.Fatalf("%d requests, want 7", got)
	}
}

func TestACancelledCallerDoesNotHoldTheProbe(t *testing.T) {
	f, srv := newFake(t)
	var mode atomic.Int32 // 0 down, 1 hang, 2 up
	f.handle = func(w http.ResponseWriter, r *http.Request, _ string, _ []Point) bool {
		switch mode.Load() {
		case 0:
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		case 1:
			<-r.Context().Done()
			return true
		}
		return false
	}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	c := client(t, Config{BaseURL: srv.URL, Failures: 1, Timeout: 5 * time.Second, Now: func() time.Time { return now }})
	_, _ = c.Route(context.Background(), rider, gate, 18) // opens
	now = now.Add(time.Minute)
	mode.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Route(ctx, rider, gate, 18); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the caller's own deadline should come back as its error, got %v", err)
	}
	mode.Store(2)
	if r, _ := c.Route(context.Background(), rider, gate, 18); r.Source != SourceOSRM {
		t.Fatalf("after a cancelled probe the next call must probe again, got %+v", r.Leg)
	}
}

func TestNearestSnaps(t *testing.T) {
	_, srv := newFake(t)
	c := client(t, Config{BaseURL: srv.URL})
	s, err := c.Nearest(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceOSRM || s.Moved != 10.8 || s.Point.Lat != gate.Lat || math.Abs(s.Point.Lng-gate.Lng-0.0001) > 1e-9 {
		t.Fatalf("got %+v", s)
	}
}

func TestBadInputIsRefused(t *testing.T) {
	_, srv := newFake(t)
	c := client(t, Config{BaseURL: srv.URL})
	ctx := context.Background()
	for _, p := range []Point{{Lat: 91}, {Lng: -181}, {Lat: math.NaN()}, {Lng: math.Inf(1)}} {
		if _, err := c.Route(ctx, p, gate, 18); !errors.Is(err, ErrBadPoint) {
			t.Errorf("route from %v: %v", p, err)
		}
		if _, err := c.Table(ctx, []Point{rider, p}, gate, 18); !errors.Is(err, ErrBadPoint) {
			t.Errorf("table with %v: %v", p, err)
		}
		if _, err := c.Nearest(ctx, p); !errors.Is(err, ErrBadPoint) {
			t.Errorf("nearest %v: %v", p, err)
		}
	}
	for _, s := range []float64{0, 0.5, 201, math.NaN(), -18} {
		if _, err := c.Route(ctx, rider, gate, s); !errors.Is(err, ErrBadSpeed) {
			t.Errorf("speed %v: %v", s, err)
		}
	}
	if st := c.Stats(); st.Calls != 0 {
		t.Fatalf("refused input is not a call: %+v", st)
	}
}

func TestBadConfigIsRefusedAtStart(t *testing.T) {
	for _, cfg := range []Config{
		{BaseURL: "localhost:5000"},
		{BaseURL: "ftp://osrm"},
		{BaseURL: "http://"},
		{BaseURL: "http://osrm", Profile: "../x"},
		{BaseURL: "http://osrm", MaxTable: -1},
		{BaseURL: "http://osrm", Profile: "a;b"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v was accepted", cfg)
		}
	}
	if _, err := New(Config{BaseURL: "http://osrm:5000/"}); err != nil {
		t.Fatal(err)
	}
}

func TestATooLargeReplyFallsBack(t *testing.T) {
	f, srv := newFake(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ string, _ []Point) bool {
		_, _ = w.Write([]byte(`{"code":"Ok","pad":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", maxReplyBytes)))
		_, _ = w.Write([]byte(`"}`))
		return true
	}
	c := client(t, Config{BaseURL: srv.URL, Timeout: 5 * time.Second})
	if r, err := c.Route(context.Background(), rider, gate, 18); err != nil || r.Source != SourcePlaceholder {
		t.Fatalf("got %+v %v", r.Leg, err)
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	_, srv := newFake(t)
	c := client(t, Config{BaseURL: srv.URL})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Route(context.Background(), rider, gate, 18)
			_, _ = c.Table(context.Background(), []Point{rider}, gate, 18)
		}()
	}
	wg.Wait()
	if st := c.Stats(); st.Calls != 40 || st.OSRM != 40 {
		t.Fatalf("stats %+v", st)
	}
}

func TestNoRoadNearAPointIsUnreachable(t *testing.T) {
	f, srv := newFake(t)
	lake := Point{Lat: 12.9570, Lng: 77.6750} // stands in for a point with no road near it
	f.handle = func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool {
		for _, p := range coords {
			if p == lake {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"code": "NoSegment", "message": "Could not find a matching segment for any coordinate."})
				return true
			}
		}
		return false
	}
	c := client(t, Config{BaseURL: srv.URL, Failures: 1})
	ctx := context.Background()
	r, err := c.Route(ctx, lake, gate, 18)
	if err != nil || r.Reachable || r.Source != SourceOSRM {
		t.Fatalf("route %+v %v, want unreachable from OSRM", r.Leg, err)
	}
	legs, err := c.Table(ctx, []Point{rider, lake, rider}, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if !legs[0].Reachable || legs[1].Reachable || !legs[2].Reachable || legs[1].Source != SourceOSRM || legs[2].Source != SourceOSRM {
		t.Fatalf("only the source with no road is unreachable: %+v", legs)
	}
	all, err := c.Table(ctx, []Point{rider, rider}, lake, 18)
	if err != nil || all[0].Reachable || all[1].Reachable {
		t.Fatalf("a destination with no road makes every source unreachable: %+v %v", all, err)
	}
	s, err := c.Nearest(ctx, lake)
	if err != nil || !s.NoRoad || s.Point != lake || s.Source != SourceOSRM {
		t.Fatalf("nearest %+v %v", s, err)
	}
	if st := c.Stats(); st.Failures != 0 || st.Fallbacks != 0 || st.BreakerOpens != 0 || st.OSRM != 4 {
		t.Fatalf("no road is an answer, not a failure: %+v", st)
	}
}

func TestARefusedRequestFallsBackWithoutTrippingTheBreaker(t *testing.T) {
	f, srv := newFake(t)
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ string, _ []Point) bool {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"code": "InvalidQuery", "message": "Query string malformed"})
		return true
	}
	c := client(t, Config{BaseURL: srv.URL, Failures: 1})
	for range 3 {
		if r, err := c.Route(context.Background(), rider, gate, 18); err != nil || r.Source != SourcePlaceholder {
			t.Fatalf("got %+v %v", r.Leg, err)
		}
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("%d requests, want 3: a refusal must not open the breaker", got)
	}
	if st := c.Stats(); st.Refused != 3 || st.Failures != 0 || st.BreakerOpens != 0 || st.Fallbacks != 3 || st.OSRM != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestBreakerOpensCountOncePerOpening(t *testing.T) {
	f, srv := newFake(t)
	gateCh := make(chan struct{})
	f.handle = func(w http.ResponseWriter, r *http.Request, _ string, _ []Point) bool {
		<-gateCh // hold every request so all 20 are in flight before any fails
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	c := client(t, Config{BaseURL: srv.URL, Failures: 3, Timeout: 5 * time.Second})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Route(context.Background(), rider, gate, 18) }()
	}
	for f.calls.Load() < 20 {
		time.Sleep(time.Millisecond)
	}
	close(gateCh)
	wg.Wait()
	if st := c.Stats(); st.Failures != 20 || st.BreakerOpens != 1 {
		t.Fatalf("stats %+v, want 20 failures and one opening", st)
	}
}

func TestHalfOpenLetsExactlyOneProbeThrough(t *testing.T) {
	f, srv := newFake(t)
	var mode atomic.Int32 // 0 down, 1 slow and up
	hold := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(free) // runs before the server closes, so a failure never hangs
	f.handle = func(w http.ResponseWriter, r *http.Request, _ string, _ []Point) bool {
		if mode.Load() == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		<-hold
		return false
	}
	var mu sync.Mutex
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := client(t, Config{BaseURL: srv.URL, Failures: 1, Timeout: 5 * time.Second, Now: clock})
	_, _ = c.Route(context.Background(), rider, gate, 18) // opens
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	mode.Store(1)

	before := f.calls.Load()
	var wg sync.WaitGroup
	sources := make(chan Source, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := c.Route(context.Background(), rider, gate, 18)
			sources <- r.Source
		}()
	}
	// Nine calls fall back at once; the probe waits on the server.
	deadline := time.Now().Add(2 * time.Second)
	for len(sources) < 9 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := f.calls.Load() - before; got != 1 {
		t.Fatalf("%d requests reached OSRM while half-open, want exactly 1", got)
	}
	free()
	wg.Wait()
	close(sources)
	n := map[Source]int{}
	for s := range sources {
		n[s]++
	}
	if n[SourceOSRM] != 1 || n[SourcePlaceholder] != 9 {
		t.Fatalf("sources %v, want 1 osrm and 9 placeholder", n)
	}
}

func TestATableIsCappedForTheURL(t *testing.T) {
	f, srv := newFake(t)
	var sizes []int
	var mu sync.Mutex
	f.handle = func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool {
		mu.Lock()
		sizes = append(sizes, len(coords))
		mu.Unlock()
		return false
	}
	c := client(t, Config{BaseURL: srv.URL, Timeout: 5 * time.Second})
	sources := make([]Point, 600)
	for i := range sources {
		sources[i] = Point{Lat: 12.9 + float64(i)/10000, Lng: 77.6}
	}
	legs, err := c.Table(context.Background(), sources, gate, 18)
	if err != nil || len(legs) != 600 || legs[599].Source != SourceOSRM {
		t.Fatalf("%d legs, err %v", len(legs), err)
	}
	if fmt.Sprint(sizes) != "[500 102]" {
		t.Fatalf("sizes %v, want [500 102]", sizes)
	}
	if l := len(f.requests[0]); l > 16000 {
		t.Fatalf("first URL is %d bytes", l)
	}
}

func TestOneTimeoutCoversAWholeTable(t *testing.T) {
	f, srv := newFake(t)
	f.handle = func(w http.ResponseWriter, r *http.Request, _ string, _ []Point) bool {
		select {
		case <-time.After(40 * time.Millisecond):
		case <-r.Context().Done():
			return true
		}
		return false
	}
	// 4 chunks of 40 ms each against a 100 ms budget: the call must fall back, not take 160 ms+.
	c := client(t, Config{BaseURL: srv.URL, MaxTable: 1, Timeout: 100 * time.Millisecond, Failures: 100})
	start := time.Now()
	legs, err := c.Table(context.Background(), []Point{rider, rider, rider, rider}, gate, 18)
	if err != nil || legs[0].Source != SourcePlaceholder {
		t.Fatalf("legs %+v %v", legs, err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("took %v, want about the 100 ms budget", d)
	}
}

func TestADestinationWithNoRoadCostsOneMoreRequest(t *testing.T) {
	f, srv := newFake(t)
	lake := Point{Lat: 12.9570, Lng: 77.6750}
	f.handle = func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool {
		for _, p := range coords {
			if p == lake {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"code": "NoSegment"})
				return true
			}
		}
		return false
	}
	c := client(t, Config{BaseURL: srv.URL})
	sources := make([]Point, 200)
	for i := range sources {
		sources[i] = Point{Lat: 12.9 + float64(i)/10000, Lng: 77.6}
	}
	legs, err := c.Table(context.Background(), sources, lake, 18)
	if err != nil || len(legs) != 200 {
		t.Fatalf("%d legs, %v", len(legs), err)
	}
	for i, l := range legs {
		if l.Reachable || l.Source != SourceOSRM {
			t.Fatalf("leg %d = %+v", i, l)
		}
	}
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("%d requests, want 2: the table, then the destination alone", got)
	}
	// One source with no road among 200: found by halving, in far fewer than 200 requests.
	f.calls.Store(0)
	sources[137] = lake
	legs, err = c.Table(context.Background(), sources, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range legs {
		if (i == 137) == l.Reachable {
			t.Fatalf("leg %d = %+v", i, l)
		}
	}
	if got := f.calls.Load(); got > 20 {
		t.Fatalf("%d requests to find one source with no road, want about 2 log2(200)", got)
	}
}

func TestAHugeMaxTableDoesNotOverflow(t *testing.T) {
	c, err := New(Config{BaseURL: "http://osrm", MaxTable: math.MaxInt})
	if err != nil || c.perSource != maxCoordsPerRequest-1 {
		t.Fatalf("perSource %d, err %v", c.perSource, err)
	}
}

func TestViaReturnsOneLegPerPairWithBearings(t *testing.T) {
	f, srv := newFake(t)
	var bearings, straight string
	f.handle = func(w http.ResponseWriter, r *http.Request, service string, coords []Point) bool {
		if service != "route" {
			return false
		}
		bearings, straight = r.URL.Query().Get("bearings"), r.URL.Query().Get("continue_straight")
		legs := make([]any, len(coords)-1)
		for i := range legs {
			legs[i] = map[string]any{"duration": 10.0 * float64(i+1), "distance": 100.0 * float64(i+1)}
		}
		writeJSON(w, map[string]any{"code": "Ok", "routes": []any{map[string]any{"legs": legs}}})
		return true
	}
	c := client(t, Config{BaseURL: srv.URL})
	wps := []Waypoint{{Point: rider, Bearing: -1}, {Point: gate, Bearing: 90}, {Point: gate, Bearing: 359}, {Point: gate, Bearing: -1}}
	legs, err := c.Via(context.Background(), wps, 18)
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 3 || legs[1].Seconds != 20 || legs[2].Metres != 300 || legs[0].Source != SourceOSRM {
		t.Fatalf("%+v", legs)
	}
	if bearings != ";90,30;359,30;" || straight != "true" {
		t.Fatalf("bearings %q, continue_straight %q", bearings, straight)
	}
	// No road: every leg unreachable, from OSRM.
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ string, _ []Point) bool {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"code": "NoRoute"})
		return true
	}
	if legs, err = c.Via(context.Background(), wps, 18); err != nil || len(legs) != 3 || legs[0].Reachable || legs[0].Source != SourceOSRM {
		t.Fatalf("no road: %+v %v", legs, err)
	}
	// Wrong leg count is a bad reply: straight lines, said so.
	f.handle = func(w http.ResponseWriter, _ *http.Request, _ string, _ []Point) bool {
		writeJSON(w, map[string]any{"code": "Ok", "routes": []any{map[string]any{"legs": []any{map[string]any{"duration": 1.0, "distance": 1.0}}}}})
		return true
	}
	if legs, err = c.Via(context.Background(), wps, 18); err != nil || legs[0].Source != SourcePlaceholder || len(legs) != 3 {
		t.Fatalf("bad reply: %+v %v", legs, err)
	}
	for _, bad := range [][]Waypoint{wps[:1], {{Point: rider, Bearing: 360}, {Point: gate, Bearing: -1}}, {{Point: Point{Lat: 91}}, {Point: gate}}} {
		if _, err := c.Via(context.Background(), bad, 18); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestBearing(t *testing.T) {
	o := Point{Lat: 12.97, Lng: 77.6}
	for _, c := range []struct {
		to   Point
		want int
	}{{Point{12.98, 77.6}, 0}, {Point{12.97, 77.61}, 90}, {Point{12.96, 77.6}, 180}, {Point{12.97, 77.59}, 270}} {
		if got := Bearing(o, c.to); got != c.want {
			t.Errorf("to %v: %d, want %d", c.to, got, c.want)
		}
	}
}

// OSRM rounds a point on the destination to a tiny negative distance (#217): that is 0, from
// OSRM; a clearly negative number is still a bad reply.
func TestATinyNegativeFromOSRMIsZero(t *testing.T) {
	f, srv := newFake(t)
	var dist, dur float64
	f.handle = func(w http.ResponseWriter, _ *http.Request, service string, _ []Point) bool {
		if service != "table" {
			return false
		}
		writeJSON(w, map[string]any{"code": "Ok", "durations": [][]any{{dur}, {12.0}}, "distances": [][]any{{dist}, {100.0}}})
		return true
	}
	c := client(t, Config{BaseURL: srv.URL})
	dist, dur = -0.2, 0
	legs, err := c.Table(context.Background(), []Point{gate, rider}, gate, 18)
	if err != nil || legs[0].Source != SourceOSRM || legs[0].Metres != 0 || !legs[0].Reachable || legs[1].Seconds != 12 {
		t.Fatalf("-0.2 m: %+v %v", legs, err)
	}
	dist = -1.5
	if legs, _ = c.Table(context.Background(), []Point{gate, rider}, gate, 18); legs[0].Source != SourcePlaceholder {
		t.Fatalf("-1.5 m was accepted: %+v", legs)
	}
	dist, dur = 3, -0.4
	if legs, _ = c.Table(context.Background(), []Point{gate, rider}, gate, 18); legs[0].Source != SourceOSRM || legs[0].Seconds != 0 {
		t.Fatalf("-0.4 s: %+v", legs)
	}
}
