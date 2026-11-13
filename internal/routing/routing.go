// Package routing answers travel times and distances on real roads, from a self-hosted OSRM
// server running on OpenStreetMap data (decision G3 in docs/engineering/geo/README.md).
//
// When OSRM is not configured, is down, is slow or answers nonsense, every call falls back to a
// straight line at the caller's speed, which is the behaviour before routing existed, and says so
// (Source "placeholder"). Two points with no road between them, or a point with no road near it,
// are reported as unreachable, never as a straight line: a river is not a short cut.
package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source says where an answer came from.
type Source string

const (
	SourceOSRM        Source = "osrm"
	SourcePlaceholder Source = "placeholder"
)

const (
	// DefaultTimeout bounds a whole call, however many requests it makes.
	DefaultTimeout = 300 * time.Millisecond
	// DefaultMaxTable is OSRM's own --max-table-size default. OSRM refuses a table when
	// sources x destinations > max-table-size squared.
	DefaultMaxTable = 100
	DefaultFailures = 3
	DefaultCooldown = 10 * time.Second
	DefaultProfile  = "driving"
	// maxCoordsPerRequest keeps the URL short enough for OSRM's HTTP server (about 22 bytes a point).
	maxCoordsPerRequest = 500
	maxReplyBytes       = 8 << 20
	maxStraightLineKmh  = 200.0
	minStraightLineKmh  = 1.0
)

// Point is a position in degrees.
type Point struct {
	Lat, Lng float64
}

func (p Point) valid() bool {
	return !math.IsNaN(p.Lat) && !math.IsNaN(p.Lng) && !math.IsInf(p.Lat, 0) && !math.IsInf(p.Lng, 0) &&
		p.Lat >= -90 && p.Lat <= 90 && p.Lng >= -180 && p.Lng <= 180
}

// Leg is the travel from one point to another.
type Leg struct {
	Seconds   float64
	Metres    float64
	Reachable bool // false: no road between the two points; Seconds and Metres are then zero
	Source    Source
}

// Route is a single route with its line.
type Route struct {
	Leg
	Line []Point // empty for a placeholder or when unreachable
}

// Snap is the nearest point on a road.
type Snap struct {
	Point  Point
	Moved  float64 // metres between the asked point and the snapped one
	NoRoad bool    // OSRM found no road near the point; Point is the asked point
	Source Source  // placeholder: OSRM was not used and Point is the asked point
}

// ErrBadPoint is returned for a coordinate that is not a real position.
var ErrBadPoint = errors.New("routing: point out of range")

// ErrBadSpeed is returned for a straight-line speed outside 1 to 200 km/h.
var ErrBadSpeed = errors.New("routing: straight-line speed out of range")

// Config sets up a Client. Only BaseURL is needed for OSRM; an empty BaseURL means straight lines.
type Config struct {
	BaseURL  string        // ROUTING_ENGINE_URL, for example http://localhost:5000
	Profile  string        // ROUTING_PROFILE, the profile name in OSRM's URL; "driving" if empty
	Timeout  time.Duration // per Route, Table or Nearest call; DefaultTimeout if zero
	MaxTable int           // the server's --max-table-size; DefaultMaxTable if zero
	Failures int           // failures in a row that open the breaker; DefaultFailures if zero
	Cooldown time.Duration // how long the breaker stays open; DefaultCooldown if zero
	HTTP     *http.Client  // optional; its Timeout is not used, Timeout above is
	Now      func() time.Time
}

// Stats are running counters, for logs and alerts on the fallback rate.
type Stats struct {
	Calls        int64 // Route, Table and Nearest calls
	OSRM         int64 // calls answered by OSRM
	Fallbacks    int64 // calls answered by the straight line
	Failures     int64 // calls where OSRM failed: timeout, network, HTTP error, bad reply
	Refused      int64 // calls OSRM refused as a bad request (InvalidQuery and similar); a bug on our side
	BreakerOpens int64 // times the breaker went from closed to open, or a probe failed
}

// Client talks to OSRM. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	profile   string
	timeout   time.Duration
	perSource int // sources per /table request, with one destination
	http      *http.Client
	now       func() time.Time

	mu        sync.Mutex
	failures  int
	limit     int
	cooldown  time.Duration
	openUntil time.Time
	probing   bool

	calls, osrm, fallbacks, failed, refused, opens atomic.Int64
}

// New builds a Client. A BaseURL that is not an http or https URL is an error, so a typo in the
// setting fails at start-up instead of silently falling back for ever.
func New(cfg Config) (*Client, error) {
	c := &Client{
		profile:  cfg.Profile,
		timeout:  cfg.Timeout,
		http:     cfg.HTTP,
		now:      cfg.Now,
		limit:    cfg.Failures,
		cooldown: cfg.Cooldown,
	}
	if c.profile == "" {
		c.profile = DefaultProfile
	}
	if strings.ContainsAny(c.profile, "/?#% ;,") {
		return nil, fmt.Errorf("routing: bad profile name %q", c.profile)
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	maxTable := cfg.MaxTable
	if maxTable == 0 {
		maxTable = DefaultMaxTable
	}
	if maxTable < 1 {
		return nil, fmt.Errorf("routing: MaxTable must be at least 1, got %d", maxTable)
	}
	// With one destination OSRM allows max-table-size squared sources; the URL caps it lower.
	c.perSource = min(min(maxTable, maxCoordsPerRequest)*min(maxTable, maxCoordsPerRequest), maxCoordsPerRequest-1)
	if c.limit <= 0 {
		c.limit = DefaultFailures
	}
	if c.cooldown <= 0 {
		c.cooldown = DefaultCooldown
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("routing: ROUTING_ENGINE_URL must be an http or https URL")
		}
		c.base = u
	}
	return c, nil
}

// Stats returns the counters so far.
func (c *Client) Stats() Stats {
	return Stats{
		Calls: c.calls.Load(), OSRM: c.osrm.Load(), Fallbacks: c.fallbacks.Load(),
		Failures: c.failed.Load(), Refused: c.refused.Load(), BreakerOpens: c.opens.Load(),
	}
}

// errRefused marks a request OSRM refused as malformed. It says nothing about OSRM's health, so it
// does not count toward the breaker, but the call still falls back.
type errRefused struct{ code string }

func (e errRefused) Error() string { return "routing: OSRM refused the request: " + e.code }

// errNoSegment is OSRM saying a coordinate has no road near it.
var errNoSegment = errors.New("routing: no road near a coordinate")

// attempt runs fn against OSRM if the breaker allows and records the outcome. It returns
// used=false when the caller must fall back. A caller whose own context ended gets its error.
func (c *Client) attempt(ctx context.Context, fn func(ctx context.Context) error) (used bool, err error) {
	try, probe := c.allow()
	if !try {
		return false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ferr := fn(cctx)
	var refused errRefused
	switch {
	case ferr == nil:
		c.done(probe, true)
		return true, nil
	case ctx.Err() != nil:
		c.release(probe)
		return false, ctx.Err()
	case errors.As(ferr, &refused):
		// OSRM answered, so it is up: a refusal closes a half-open breaker like a success would.
		c.refused.Add(1)
		c.up(probe)
		return false, nil
	default:
		c.done(probe, false)
		return false, nil
	}
}

// Route returns the road route from one point to another. speedKmh is the straight-line speed
// for the fallback (the setting straight_line_speed_kmh).
func (c *Client) Route(ctx context.Context, from, to Point, speedKmh float64) (Route, error) {
	if !from.valid() || !to.valid() {
		return Route{}, ErrBadPoint
	}
	if err := checkSpeed(speedKmh); err != nil {
		return Route{}, err
	}
	c.calls.Add(1)
	var out Route
	used, err := c.attempt(ctx, func(ctx context.Context) error {
		var reply struct {
			Code   string `json:"code"`
			Routes []struct {
				Duration *float64 `json:"duration"`
				Distance *float64 `json:"distance"`
				Geometry struct {
					Coordinates [][]float64 `json:"coordinates"`
				} `json:"geometry"`
			} `json:"routes"`
		}
		q := url.Values{"overview": {"full"}, "geometries": {"geojson"}, "alternatives": {"false"}, "steps": {"false"}}
		code, err := c.get(ctx, "route", []Point{from, to}, q, &reply)
		switch {
		case errors.Is(err, errNoSegment) || code == "NoRoute":
			out = Route{Leg: Leg{Source: SourceOSRM}}
			return nil
		case err != nil:
			return err
		case len(reply.Routes) == 0:
			return errors.New("routing: route answered no routes")
		}
		r := reply.Routes[0]
		line, err := toLine(r.Geometry.Coordinates)
		if err != nil {
			return err
		}
		if !goodNumber(r.Duration) || !goodNumber(r.Distance) {
			return errors.New("routing: route answered a missing, negative or non-finite number")
		}
		out = Route{Leg: Leg{Seconds: *r.Duration, Metres: *r.Distance, Reachable: true, Source: SourceOSRM}, Line: line}
		return nil
	})
	if err != nil {
		return Route{}, err
	}
	if used {
		return out, nil
	}
	c.fallbacks.Add(1)
	return Route{Leg: straight(from, to, speedKmh)}, nil
}

// Table returns the travel from each source to one destination, in the order given. Large
// requests are split; one timeout covers them all. If any part fails, the whole answer is the
// straight line, so one answer never mixes sources.
func (c *Client) Table(ctx context.Context, sources []Point, dest Point, speedKmh float64) ([]Leg, error) {
	if !dest.valid() {
		return nil, ErrBadPoint
	}
	for _, s := range sources {
		if !s.valid() {
			return nil, ErrBadPoint
		}
	}
	if err := checkSpeed(speedKmh); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return []Leg{}, nil
	}
	c.calls.Add(1)
	var out []Leg
	used, err := c.attempt(ctx, func(ctx context.Context) error {
		out = make([]Leg, 0, len(sources))
		for start := 0; start < len(sources); start += c.perSource {
			legs, err := c.tableChunk(ctx, sources[start:min(start+c.perSource, len(sources))], dest, false)
			if err != nil {
				return err
			}
			out = append(out, legs...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if used {
		return out, nil
	}
	c.fallbacks.Add(1)
	out = make([]Leg, len(sources))
	for i, s := range sources {
		out[i] = straight(s, dest, speedKmh)
	}
	return out, nil
}

// tableChunk asks one /table request. If OSRM says some coordinate has no road near it, it first
// asks whether the destination is the one (then every source is unreachable, in one request),
// and otherwise splits the sources in halves to find which have no road. destOK says the
// destination is already known to have a road.
func (c *Client) tableChunk(ctx context.Context, chunk []Point, dest Point, destOK bool) ([]Leg, error) {
	coords := append(append(make([]Point, 0, len(chunk)+1), chunk...), dest)
	idx := make([]string, len(chunk))
	for i := range chunk {
		idx[i] = strconv.Itoa(i)
	}
	q := url.Values{
		"sources":      {strings.Join(idx, ";")},
		"destinations": {strconv.Itoa(len(chunk))},
		"annotations":  {"duration,distance"},
	}
	var reply struct {
		Durations [][]*float64 `json:"durations"`
		Distances [][]*float64 `json:"distances"`
	}
	_, err := c.get(ctx, "table", coords, q, &reply)
	if errors.Is(err, errNoSegment) {
		if !destOK {
			var near struct{}
			_, nerr := c.get(ctx, "nearest", []Point{dest}, url.Values{"number": {"1"}}, &near)
			switch {
			case errors.Is(nerr, errNoSegment):
				return unreachable(len(chunk)), nil
			case nerr != nil:
				return nil, nerr
			}
		}
		if len(chunk) == 1 {
			return []Leg{{Source: SourceOSRM}}, nil
		}
		half := len(chunk) / 2
		left, err := c.tableChunk(ctx, chunk[:half], dest, true)
		if err != nil {
			return nil, err
		}
		right, err := c.tableChunk(ctx, chunk[half:], dest, true)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	if err != nil {
		return nil, err
	}
	if len(reply.Durations) != len(chunk) || len(reply.Distances) != len(chunk) {
		return nil, fmt.Errorf("routing: table answered %d duration and %d distance rows for %d sources",
			len(reply.Durations), len(reply.Distances), len(chunk))
	}
	out := make([]Leg, 0, len(chunk))
	for i := range chunk {
		if len(reply.Durations[i]) != 1 || len(reply.Distances[i]) != 1 {
			return nil, errors.New("routing: table row is not one column")
		}
		d, m := reply.Durations[i][0], reply.Distances[i][0]
		if d == nil || m == nil {
			// OSRM writes null when no route exists between the two.
			if (d == nil) != (m == nil) {
				return nil, errors.New("routing: table answered a duration without a distance")
			}
			out = append(out, Leg{Source: SourceOSRM})
			continue
		}
		if !goodNumber(d) || !goodNumber(m) {
			return nil, errors.New("routing: table answered a negative or non-finite number")
		}
		out = append(out, Leg{Seconds: *d, Metres: *m, Reachable: true, Source: SourceOSRM})
	}
	return out, nil
}

// Nearest snaps a point to the nearest road. On fallback the point is returned unmoved.
func (c *Client) Nearest(ctx context.Context, p Point) (Snap, error) {
	if !p.valid() {
		return Snap{}, ErrBadPoint
	}
	c.calls.Add(1)
	var out Snap
	used, err := c.attempt(ctx, func(ctx context.Context) error {
		var reply struct {
			Waypoints []struct {
				Location []float64 `json:"location"`
				Distance *float64  `json:"distance"`
			} `json:"waypoints"`
		}
		_, err := c.get(ctx, "nearest", []Point{p}, url.Values{"number": {"1"}}, &reply)
		if errors.Is(err, errNoSegment) {
			out = Snap{Point: p, NoRoad: true, Source: SourceOSRM}
			return nil
		}
		if err != nil {
			return err
		}
		if len(reply.Waypoints) == 0 {
			return errors.New("routing: nearest answered no waypoints")
		}
		w := reply.Waypoints[0]
		if len(w.Location) != 2 || !goodNumber(w.Distance) {
			return errors.New("routing: nearest answered a bad waypoint")
		}
		snapped := Point{Lat: w.Location[1], Lng: w.Location[0]}
		if !snapped.valid() {
			return ErrBadPoint
		}
		out = Snap{Point: snapped, Moved: *w.Distance, Source: SourceOSRM}
		return nil
	})
	if err != nil {
		return Snap{}, err
	}
	if used {
		return out, nil
	}
	c.fallbacks.Add(1)
	return Snap{Point: p, Source: SourcePlaceholder}, nil
}

// get calls one OSRM service and decodes the reply into into. Coordinates go in OSRM's lng,lat
// order. It returns OSRM's code: "Ok" and "NoRoute" with a nil error, errNoSegment for
// NoSegment, errRefused for the other codes OSRM sends with HTTP 400, and an error for anything
// that is not a proper OSRM answer.
func (c *Client) get(ctx context.Context, service string, pts []Point, q url.Values, into any) (string, error) {
	parts := make([]string, len(pts))
	for i, p := range pts {
		parts[i] = strconv.FormatFloat(p.Lng, 'f', 6, 64) + "," + strconv.FormatFloat(p.Lat, 'f', 6, 64)
	}
	u := *c.base
	u.Path = c.base.Path + "/" + service + "/v1/" + c.profile + "/" + strings.Join(parts, ";")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxReplyBytes {
		return "", errors.New("routing: reply too large")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return "", fmt.Errorf("routing: %s answered HTTP %d", service, resp.StatusCode)
	}
	var head struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return "", fmt.Errorf("routing: %s reply is not JSON: %w", service, err)
	}
	switch {
	case head.Code == "Ok" && resp.StatusCode == http.StatusOK:
	case head.Code == "NoRoute":
		return head.Code, nil
	case head.Code == "NoSegment":
		return head.Code, errNoSegment
	case resp.StatusCode == http.StatusBadRequest && head.Code != "":
		return head.Code, errRefused{code: head.Code}
	default:
		return head.Code, fmt.Errorf("routing: %s answered HTTP %d with code %q", service, resp.StatusCode, head.Code)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return head.Code, fmt.Errorf("routing: %s reply has the wrong shape: %w", service, err)
	}
	return head.Code, nil
}

// allow says whether to try OSRM now. While the breaker is open, after its cool-down, exactly one
// call at a time probes it (probe=true).
func (c *Client) allow() (try, probe bool) {
	if c.base == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openUntil.IsZero() {
		return true, false
	}
	if c.now().Before(c.openUntil) || c.probing {
		return false, false
	}
	c.probing = true
	return true, true
}

// done records the result of an OSRM attempt. probe says whether this call held the probe slot.
func (c *Client) done(probe, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if probe {
		c.probing = false
	}
	if ok {
		c.osrm.Add(1)
		c.failures = 0
		c.openUntil = time.Time{}
		return
	}
	c.failed.Add(1)
	c.failures++
	switch {
	case probe:
		// A failed probe opens the breaker again for a full cool-down.
		c.openUntil = c.now().Add(c.cooldown)
		c.opens.Add(1)
	case c.openUntil.IsZero() && c.failures >= c.limit:
		c.openUntil = c.now().Add(c.cooldown)
		c.opens.Add(1)
	}
	// A call that started before the breaker opened and fails after changes nothing more.
}

// release frees a probe slot without counting a result, when the caller gave up (its context
// ended): that says nothing about OSRM.
func (c *Client) release(probe bool) {
	if !probe {
		return
	}
	c.mu.Lock()
	c.probing = false
	c.mu.Unlock()
}

func checkSpeed(kmh float64) error {
	if math.IsNaN(kmh) || kmh < minStraightLineKmh || kmh > maxStraightLineKmh {
		return ErrBadSpeed
	}
	return nil
}

// tinyNegative bounds how far below zero OSRM may round a duration or distance (seconds or metres;
// a number at or below it is a bad reply):
// for a point on the destination it answers, say, -0.2 m (#217).
const tinyNegative = -1.0

// goodNumber says a reply number is real and not negative; a tiny negative from OSRM's rounding is
// set to 0 in place.
func goodNumber(v *float64) bool {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= tinyNegative {
		return false
	}
	if *v < 0 {
		*v = 0
	}
	return true
}

func toLine(coords [][]float64) ([]Point, error) {
	if len(coords) < 2 {
		return nil, errors.New("routing: route line has fewer than two points")
	}
	line := make([]Point, len(coords))
	for i, c := range coords {
		if len(c) != 2 {
			return nil, errors.New("routing: route line point is not lng,lat")
		}
		line[i] = Point{Lat: c[1], Lng: c[0]}
		if !line[i].valid() {
			return nil, ErrBadPoint
		}
	}
	return line, nil
}

func straight(from, to Point, kmh float64) Leg {
	m := Metres(from, to)
	return Leg{Seconds: m / (kmh / 3.6), Metres: m, Reachable: true, Source: SourcePlaceholder}
}

// Metres is the straight-line distance on a sphere. It matches geo.Metres; it is repeated here so
// this package does not pull in the geo package's HTTP and database code.
func Metres(a, b Point) float64 {
	const r = 6371000.0
	p1, p2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dp, dl := (b.Lat-a.Lat)*math.Pi/180, (b.Lng-a.Lng)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// up records that OSRM answered, without counting the call as answered by OSRM.
func (c *Client) up(probe bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if probe {
		c.probing = false
	}
	c.failures = 0
	c.openUntil = time.Time{}
}

// unreachable is n legs with no road, answered by OSRM.
func unreachable(n int) []Leg {
	out := make([]Leg, n)
	for i := range out {
		out[i] = Leg{Source: SourceOSRM}
	}
	return out
}

// Waypoint is a point a route must pass, optionally with the heading (degrees from north, 0 to
// 359) the route should pass it on, so OSRM keeps the road and direction it is on there instead of
// snapping to, say, the other side of a divided road. A negative Bearing means any heading.
type Waypoint struct {
	Point
	Bearing int
}

// Via returns one route through every waypoint in order, as one leg per pair, each leg's time
// counting the junction and signal costs on it (issue 205). On fallback every leg is a straight
// line at speedKmh. A route with no road between some pair is unreachable as a whole.
func (c *Client) Via(ctx context.Context, wps []Waypoint, speedKmh float64) ([]Leg, error) {
	if len(wps) < 2 || len(wps) > 25 {
		return nil, errors.New("routing: Via takes 2 to 25 waypoints")
	}
	pts := make([]Point, len(wps))
	bearings := make([]string, len(wps))
	for i, w := range wps {
		if !w.valid() || w.Bearing > 359 {
			return nil, ErrBadPoint
		}
		pts[i] = w.Point
		if w.Bearing >= 0 {
			bearings[i] = strconv.Itoa(w.Bearing) + ",30"
		}
	}
	if err := checkSpeed(speedKmh); err != nil {
		return nil, err
	}
	c.calls.Add(1)
	var out []Leg
	used, err := c.attempt(ctx, func(ctx context.Context) error {
		var reply struct {
			Routes []struct {
				Legs []struct {
					Duration *float64 `json:"duration"`
					Distance *float64 `json:"distance"`
				} `json:"legs"`
			} `json:"routes"`
		}
		q := url.Values{"overview": {"false"}, "steps": {"false"}, "alternatives": {"false"}, "continue_straight": {"true"},
			"bearings": {strings.Join(bearings, ";")}}
		code, err := c.get(ctx, "route", pts, q, &reply)
		switch {
		case errors.Is(err, errNoSegment) || code == "NoRoute":
			out = make([]Leg, len(wps)-1)
			for i := range out {
				out[i] = Leg{Source: SourceOSRM}
			}
			return nil
		case err != nil:
			return err
		case len(reply.Routes) == 0 || len(reply.Routes[0].Legs) != len(wps)-1:
			return errors.New("routing: route answered the wrong number of legs")
		}
		out = make([]Leg, len(wps)-1)
		for i, l := range reply.Routes[0].Legs {
			if !goodNumber(l.Duration) || !goodNumber(l.Distance) {
				return errors.New("routing: route leg answered a missing, negative or non-finite number")
			}
			out[i] = Leg{Seconds: *l.Duration, Metres: *l.Distance, Reachable: true, Source: SourceOSRM}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if used {
		return out, nil
	}
	c.fallbacks.Add(1)
	out = make([]Leg, len(wps)-1)
	for i := range out {
		out[i] = straight(wps[i].Point, wps[i+1].Point, speedKmh)
	}
	return out, nil
}

// Bearing is the heading from a to b, in whole degrees from north (0 to 359).
func Bearing(a, b Point) int {
	p1, p2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dl := (b.Lng - a.Lng) * math.Pi / 180
	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
	deg := int(math.Round(math.Atan2(y, x) * 180 / math.Pi))
	return ((deg % 360) + 360) % 360
}
