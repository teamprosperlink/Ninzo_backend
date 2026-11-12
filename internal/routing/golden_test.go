package routing

// Golden routes (#183): checks of the scooter profile on real OpenStreetMap roads in the two test
// zones, against a real OSRM built from tools/osrm/testdata/test-zones.osm.pbf:
//
//	task geo:osrm:build -- test && OSRM_DATA=test-zones task geo:osrm:up && task geo:test:routes
//
// They run only when ROUTING_TEST_URL is set, and fail instead of skipping when
// REQUIRE_ROUTING_TESTS=yes. The pairs were chosen from the extract of 2026-11-11 (one-way streets
// tagged oneway=yes, points at a quarter and three quarters along each); if OpenStreetMap changes
// a street, re-cut the extract and re-pick, never loosen a rule to make it pass.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The profile's numbers (tools/osrm/profiles/scooter.lua, scooter-1).
const (
	goldenSpeedCapKmh = 40.0
	goldenSignalWaitS = 30.0
)

func goldenURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("ROUTING_TEST_URL")
	if u == "" {
		if os.Getenv("REQUIRE_ROUTING_TESTS") == "yes" {
			t.Fatal("REQUIRE_ROUTING_TESTS=yes but ROUTING_TEST_URL is not set")
		}
		t.Skip("ROUTING_TEST_URL not set: golden routes need a real OSRM (see golden_test.go)")
	}
	return u
}

func goldenClient(t *testing.T) *Client {
	return client(t, Config{BaseURL: goldenURL(t), Profile: "scooter", Timeout: 10 * time.Second})
}

// One-way streets: a is a quarter and b three quarters along the way, in its direction.
var goldenOneWays = []struct {
	zone, street string
	a, b         Point
}{
	{"Koramangala", "Tavarekere Main Road", Point{12.929539, 77.614394}, Point{12.929969, 77.613316}},
	{"Koramangala", "Jyoti Nivas College Road", Point{12.933515, 77.615569}, Point{12.932843, 77.614215}},
	{"Koramangala", "Srinivagilu Main Road", Point{12.939262, 77.625746}, Point{12.938160, 77.626288}},
	{"Koramangala", "80 Feet Road", Point{12.936283, 77.616740}, Point{12.936054, 77.615775}},
	{"Koramangala", "5th Cross Road", Point{12.936979, 77.619943}, Point{12.935382, 77.622514}},
	{"Koramangala", "4th B Cross Road", Point{12.933325, 77.614042}, Point{12.934067, 77.615529}},
	{"Indiranagar", "Indiranagar Double Road", Point{12.967967, 77.636433}, Point{12.968802, 77.636058}},
	{"Indiranagar", "13th A Main", Point{12.968315, 77.647273}, Point{12.968217, 77.646203}},
	{"Indiranagar", "80 Feet Road", Point{12.977721, 77.646710}, Point{12.975913, 77.646812}},
	{"Indiranagar", "Chinmaya Mission Hospital Road", Point{12.978213, 77.636756}, Point{12.978146, 77.634201}},
	{"Indiranagar", "13th Main Road", Point{12.968633, 77.643002}, Point{12.968743, 77.646159}},
}

// Longer routes along main roads, through signals.
var goldenLong = [][2]Point{
	{{12.980279, 77.646225}, {12.965500, 77.648500}},
	{{12.966000, 77.633000}, {12.984000, 77.649000}},
	{{12.984000, 77.633000}, {12.966000, 77.649000}},
	{{12.926000, 77.613000}, {12.944000, 77.639000}},
	{{12.944000, 77.613000}, {12.926000, 77.639000}},
	{{12.935000, 77.612500}, {12.935000, 77.639500}},
	{{12.975000, 77.632500}, {12.975000, 77.649500}},
	{{12.930000, 77.630000}, {12.982000, 77.640000}},
}

func TestGoldenOneWaysAreObeyed(t *testing.T) {
	c := goldenClient(t)
	ctx := context.Background()
	for _, w := range goldenOneWays {
		t.Run(w.zone+"/"+w.street, func(t *testing.T) {
			with, err := c.Route(ctx, w.a, w.b, 18)
			if err != nil {
				t.Fatal(err)
			}
			against, err := c.Route(ctx, w.b, w.a, 18)
			if err != nil {
				t.Fatal(err)
			}
			if with.Source != SourceOSRM || against.Source != SourceOSRM || !with.Reachable || !against.Reachable {
				t.Fatalf("want two OSRM routes, got %+v and %+v", with.Leg, against.Leg)
			}
			// Along the one-way the route is about the street itself; against it, the scooter goes round.
			straight := Metres(w.a, w.b)
			if with.Metres > 1.6*straight+30 {
				t.Errorf("with the one-way: %.0f m for %.0f m straight, want about the street", with.Metres, straight)
			}
			if against.Metres < 2*with.Metres || against.Seconds < 2*with.Seconds {
				t.Errorf("against the one-way: %.0f m, %.0f s; with it: %.0f m, %.0f s; want at least twice as long",
					against.Metres, against.Seconds, with.Metres, with.Seconds)
			}
		})
	}
}

// goldenAnnotated is one route with OSRM's per-segment annotations, which the client does not expose.
type goldenAnnotated struct {
	Duration, Distance float64
	Nodes              []float64 // OSRM prints ids as doubles with 10 significant digits
	SegDuration        []float64
	SegDistance        []float64
}

func annotatedRoute(t *testing.T, base string, a, b Point) goldenAnnotated {
	t.Helper()
	u := fmt.Sprintf("%s/route/v1/scooter/%.6f,%.6f;%.6f,%.6f?overview=false&annotations=nodes,duration,distance",
		base, a.Lng, a.Lat, b.Lng, b.Lat)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Code   string `json:"code"`
		Routes []struct {
			Duration, Distance float64
			Legs               []struct {
				Annotation struct {
					Nodes    []float64 `json:"nodes"`
					Duration []float64 `json:"duration"`
					Distance []float64 `json:"distance"`
				} `json:"annotation"`
			} `json:"legs"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Code != "Ok" || len(r.Routes) == 0 || len(r.Routes[0].Legs) != 1 {
		t.Fatalf("route %v to %v: code %q, err %v", a, b, r.Code, err)
	}
	rt, an := r.Routes[0], r.Routes[0].Legs[0].Annotation
	return goldenAnnotated{rt.Duration, rt.Distance, an.Nodes, an.Duration, an.Distance}
}

// nodeKey is a node id at the precision OSRM prints it (10 significant digits), so ids longer
// than that still match. An id of 11 digits or more loses its last digit, so it can also match a neighbour
// (OpenStreetMap ids along a way are often consecutive): the signal count can be one or two too high
// near such ids, which only makes the checks stricter. The closed-street check compares exact ids under 1e10 instead.
func nodeKey(id float64) string { return strconv.FormatFloat(id, 'g', 10, 64) }

func goldenSignals(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "tools", "osrm", "testdata", "signals.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id, err := strconv.ParseInt(sc.Text(), 10, 64)
		if err != nil {
			t.Fatalf("signals.txt: %v", err)
		}
		out[nodeKey(float64(id))] = true
	}
	if len(out) < 100 {
		t.Fatalf("only %d signals listed; re-run tools/osrm/build.sh testdata", len(out))
	}
	return out
}

// Straight crossings of one signalised junction: 70 m before to 70 m after a signal, along one road.
// Going straight costs almost nothing in turn penalty, so the junction cost here is the signal wait.
// A junction with a signal node on each side of a dual road is paid once.
var goldenSignalCrossings = [][2]Point{
	{{12.9309259, 77.6222540}, {12.9320988, 77.6227237}},
	{{12.9625144, 77.6416201}, {12.9637721, 77.6415611}},
	{{12.9693471, 77.6412927}, {12.9706040, 77.6412179}},
	{{12.9738615, 77.6469372}, {12.9726063, 77.6470274}},
	{{12.9791800, 77.6408830}, {12.9779223, 77.6409408}},
	{{12.9338397, 77.6302088}, {12.9347458, 77.6293122}},
	{{12.9368026, 77.6262749}, {12.9376090, 77.6272667}},
	{{12.9827802, 77.6368032}, {12.9831826, 77.6380264}},
	{{12.9385822, 77.6319963}, {12.9385672, 77.6332880}},
	{{12.9713682, 77.6351963}, {12.9718681, 77.6363820}},
}

func TestGoldenASignalCostsItsWait(t *testing.T) {
	base := goldenURL(t)
	signals := goldenSignals(t)
	for i, p := range goldenSignalCrossings {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			r := annotatedRoute(t, base, p[0], p[1])
			if straight := Metres(p[0], p[1]); r.Distance > straight+15 {
				t.Fatalf("%.0f m on the road for %.0f m straight: no longer a straight crossing; re-pick", r.Distance, straight)
			}
			passed := 0
			for _, n := range r.Nodes[1 : len(r.Nodes)-1] {
				if signals[nodeKey(n)] {
					passed++
				}
			}
			if passed == 0 {
				t.Fatal("passes no signal; the fixture no longer tests signals; re-pick")
			}
			moving := 0.0
			for _, d := range r.SegDuration {
				moving += d
			}
			// OSRM's segment durations leave out junction costs; what is left is the signal wait.
			extra := r.Duration - moving
			if extra < 0.9*goldenSignalWaitS || extra > goldenSignalWaitS+5 {
				t.Errorf("%.1f s of junction cost crossing %d signal nodes straight, want about %.0f s", extra, passed, goldenSignalWaitS)
			}
		})
	}
}

// Streets tagged motor_vehicle=no, routed end to end: the route must go round, never along them.
var goldenRefused = []struct {
	street string
	a, b   Point
	inner  []int64 // the street's own nodes between its ends, only ids under 1e10, which OSRM prints exactly
}{
	{"unnamed lane, Koramangala", Point{12.9369441, 77.6316204}, Point{12.9359801, 77.6310781}, []int64{3870651468}},
	{"Labay Masjid Street", Point{12.9826218, 77.6071027}, Point{12.9850294, 77.6078127},
		[]int64{9981175219, 1567429346, 1567429287}},
	{"Berlie Street", Point{12.9571893, 77.6058658}, Point{12.9573600, 77.6051074}, nil}, // its inner ids are 11 digits; the detour check stands alone,
	{"1st Cross Road", Point{12.9398885, 77.6093074}, Point{12.9384314, 77.6086925}, []int64{443252747, 443252745, 443252753}},
}

func TestGoldenAClosedStreetIsNotUsed(t *testing.T) {
	base := goldenURL(t)
	for _, s := range goldenRefused {
		t.Run(s.street, func(t *testing.T) {
			r := annotatedRoute(t, base, s.a, s.b)
			inner := map[int64]bool{}
			for _, id := range s.inner {
				inner[id] = true
			}
			for _, n := range r.Nodes {
				if inner[int64(n)] {
					t.Fatalf("the route uses the closed street (node %.0f)", n)
				}
			}
			if straight := Metres(s.a, s.b); r.Distance < 1.4*straight {
				t.Fatalf("%.0f m for %.0f m straight: want a detour round the closed street", r.Distance, straight)
			}
		})
	}
}

func TestGoldenNeverFasterThanTheCap(t *testing.T) {
	base := goldenURL(t)
	routes := append([][2]Point{}, goldenLong...)
	for _, w := range goldenOneWays {
		routes = append(routes, [2]Point{w.a, w.b})
	}
	for i, p := range routes {
		r := annotatedRoute(t, base, p[0], p[1])
		// OSRM snaps both ends onto the road, which can shave a few metres off.
		if r.Distance < Metres(p[0], p[1])-10 {
			t.Errorf("route %d: %.0f m on the road, shorter than %.0f m straight", i, r.Distance, Metres(p[0], p[1]))
		}
		for j := range r.SegDistance {
			// OSRM rounds segment durations to 0.1 s, so short segments are too coarse to judge.
			if r.SegDistance[j] < 60 || r.SegDuration[j] <= 0 {
				continue
			}
			if v := r.SegDistance[j] / r.SegDuration[j] * 3.6; v > goldenSpeedCapKmh*1.03 {
				t.Errorf("route %d segment %d: %.1f km/h, over the %.0f km/h cap", i, j, v, goldenSpeedCapKmh)
			}
		}
	}
}

// A table as large as the client sends (499 sources and the gate in one URL) works on the real
// server (follow-up from #180).
func TestGoldenALargeTableFitsOneRequest(t *testing.T) {
	c := goldenClient(t)
	sources := make([]Point, 499)
	for i := range sources {
		sources[i] = Point{Lat: 12.966 + float64(i%25)*0.0007, Lng: 77.633 + float64(i/25)*0.0008}
	}
	gate := Point{Lat: 12.97194, Lng: 77.64115}
	legs, err := c.Table(context.Background(), sources, gate, 18)
	if err != nil {
		t.Fatal(err)
	}
	reachable := 0
	for _, l := range legs {
		if l.Source != SourceOSRM {
			t.Fatalf("got a %s answer; the large table failed: %+v", l.Source, c.Stats())
		}
		if l.Reachable {
			reachable++
		}
	}
	if reachable < 450 {
		t.Fatalf("only %d of 499 points in Indiranagar reach the gate", reachable)
	}
	if st := c.Stats(); st.Failures != 0 || st.Refused != 0 {
		t.Fatalf("stats %+v", st)
	}
}
