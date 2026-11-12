package hexgrid

import (
	"math"
	"math/rand/v2"
	"testing"
)

// haversine is the true distance on a sphere, to check the flat projection against.
func haversine(a, b Point) float64 {
	p1, p2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dp, dl := (b.Lat-a.Lat)*math.Pi/180, (b.Lng-a.Lng)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * earthRadius * math.Asin(math.Min(1, math.Sqrt(h)))
}

// cityPoints are random points over Bengaluru with a margin, the box tools/osrm builds.
func cityPoints(n int) []Point {
	rng := rand.New(rand.NewPCG(20261113, 193))
	out := make([]Point, n)
	for i := range out {
		out[i] = Point{Lat: 12.75 + rng.Float64()*0.5, Lng: 77.35 + rng.Float64()*0.55}
	}
	return out
}

func TestTheBengaluruGridIsWhatNewMakes(t *testing.T) {
	g, err := New("blr76", 12.9716, 77.5946, 76)
	if err != nil {
		t.Fatal(err)
	}
	if *g != *Bengaluru() {
		t.Fatalf("%+v != %+v", *g, *Bengaluru())
	}
	// About H3 resolution 10 by area (0.015 km² on average).
	if area := 3 * math.Sqrt(3) / 2 * g.Edge() * g.Edge(); math.Abs(area-15000)/15000 > 0.01 {
		t.Fatalf("cell area %.0f m², want about 15,000", area)
	}
}

func TestAPointIsInItsOwnCellAndNearestItsCentre(t *testing.T) {
	g := Bengaluru()
	for _, p := range cityPoints(20000) {
		c, err := g.CellAt(p)
		if err != nil {
			t.Fatal(err)
		}
		// Measured in the grid's own flat metres, where the grid is exact (the real-Earth error of
		// the projection is checked by TestNeighboursAreOneSpacingApartOnTheRealEarth).
		own := g.flatDistance(p, g.Centre(c))
		// Nearer its own centre than any neighbour's (to a micrometre, for points on an edge).
		for _, n := range Neighbours(c) {
			if d := g.flatDistance(p, g.Centre(n)); d < own-1e-6 {
				t.Fatalf("%v in %v is %.2f m from its centre but %.2f m from neighbour %v", p, c, own, d, n)
			}
		}
		// Never further from its centre than a corner is.
		if own > g.Edge()+1e-6 {
			t.Fatalf("%v is %.2f m from its centre; the corner is %.0f m", p, own, g.Edge())
		}
		if !insideOutline(g, p, c) {
			t.Fatalf("%v is outside its cell's outline %v", p, c)
		}
	}
}

// insideOutline is a ray-casting test in the grid's own flat coordinates.
func insideOutline(g *Grid, p Point, c Cell) bool {
	px, py := g.toXY(p)
	o := g.Outline(c)
	in := false
	for i, j := 0, 5; i < 6; j, i = i, i+1 {
		xi, yi := g.toXY(o[i])
		xj, yj := g.toXY(o[j])
		// A point on the outline counts as inside (it belongs to exactly one cell by rounding).
		if onSegment(px, py, xi, yi, xj, yj) {
			return true
		}
		if (yi > py) != (yj > py) && px < (xj-xi)*(py-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

func onSegment(px, py, ax, ay, bx, by float64) bool {
	cross := (bx-ax)*(py-ay) - (by-ay)*(px-ax)
	return math.Abs(cross) < 1e-6*math.Hypot(bx-ax, by-ay) &&
		px >= math.Min(ax, bx)-1e-6 && px <= math.Max(ax, bx)+1e-6 && py >= math.Min(ay, by)-1e-6 && py <= math.Max(ay, by)+1e-6
}

func TestNeighboursAreOneSpacingApartOnTheRealEarth(t *testing.T) {
	g := Bengaluru()
	want := math.Sqrt(3) * g.Edge()
	worst := 0.0
	for _, p := range cityPoints(2000) {
		c, _ := g.CellAt(p)
		for _, n := range Neighbours(c) {
			d := haversine(g.Centre(c), g.Centre(n))
			worst = math.Max(worst, math.Abs(d-want)/want)
		}
	}
	// The flat projection's error over the city: well under 1%.
	if worst > 0.005 {
		t.Fatalf("neighbour spacing off by up to %.3f%%, want under 0.5%%", worst*100)
	}
	t.Logf("neighbour spacing within %.4f%% of %.1f m over the city", worst*100, want)
}

func TestTheCentreIsInItsOwnCell(t *testing.T) {
	g := Bengaluru()
	for q := int32(-150); q <= 150; q += 7 {
		for r := int32(-150); r <= 150; r += 7 {
			c := Cell{q, r}
			got, err := g.CellAt(g.Centre(c))
			if err != nil || got != c {
				t.Fatalf("centre of %v is in %v (%v)", c, got, err)
			}
			// Corners are shared by three cells: each must resolve to one of them.
			for _, corner := range g.Outline(c) {
				at, _ := g.CellAt(corner)
				if Distance(at, c) > 1 {
					t.Fatalf("a corner of %v resolves to %v, not a touching cell", c, at)
				}
			}
		}
	}
}

func TestDisks(t *testing.T) {
	c := Cell{3, -7}
	for k := 0; k <= 12; k++ {
		d, err := Disk(c, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(d) != 1+3*k*(k+1) {
			t.Fatalf("k=%d: %d cells, want %d", k, len(d), 1+3*k*(k+1))
		}
		seen := map[Cell]bool{}
		for i, x := range d {
			if seen[x] {
				t.Fatalf("k=%d: %v twice", k, x)
			}
			seen[x] = true
			if Distance(x, c) > k {
				t.Fatalf("k=%d: %v is %d steps away", k, x, Distance(x, c))
			}
			// Ring by ring: never closer than an earlier cell.
			if i > 0 && Distance(x, c) < Distance(d[i-1], c) {
				t.Fatalf("k=%d: not in ring order at %d", k, i)
			}
		}
		// Every cell within k is there (checked against a brute-force scan).
		for q := c.Q - int32(k); q <= c.Q+int32(k); q++ {
			for r := c.R - int32(k); r <= c.R+int32(k); r++ {
				if x := (Cell{q, r}); Distance(x, c) <= k && !seen[x] {
					t.Fatalf("k=%d: %v missing", k, x)
				}
			}
		}
	}
	if d, _ := Disk(c, 0); len(d) != 1 || d[0] != c {
		t.Fatalf("k=0: %v", d)
	}
	for _, k := range []int{-1, MaxDisk + 1} {
		if _, err := Disk(c, k); err == nil {
			t.Errorf("k=%d accepted", k)
		}
	}
	for _, n := range Neighbours(c) {
		if Distance(n, c) != 1 {
			t.Fatalf("neighbour %v is %d steps", n, Distance(n, c))
		}
	}
}

func TestIDsRoundTripAndRefuseOtherGrids(t *testing.T) {
	g := Bengaluru()
	for _, c := range []Cell{{0, 0}, {12, -3}, {-150, 77}, {-500, 600}} {
		id := g.ID(c)
		back, err := g.Parse(id)
		if err != nil || back != c {
			t.Fatalf("%v -> %q -> %v %v", c, id, back, err)
		}
	}
	if g.ID(Cell{12, -3}) != "blr76:12:-3" {
		t.Fatalf("id %q", g.ID(Cell{12, -3}))
	}
	other, _ := New("mys76", 12.2958, 76.6394, 76)
	for _, bad := range []string{other.ID(Cell{1, 2}), "blr76:1", "blr76:1:2:3", "blr76:a:2", "blr76:+1:2", "blr76:01:2", "blr76:1:99999999999", "", g.ID(Cell{math.MaxInt32, math.MinInt32}), g.ID(Cell{2000, 0})} {
		if _, err := g.Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBadInputIsRefused(t *testing.T) {
	g := Bengaluru()
	for _, p := range []Point{{Lat: 91}, {Lng: 181}, {Lat: math.NaN()}, {Lng: math.Inf(-1)}, {Lat: 13.0, Lng: 79.0}} {
		if _, err := g.CellAt(p); err == nil {
			t.Errorf("%v accepted", p)
		}
	}
	for _, c := range []struct {
		name           string
		lat, lng, edge float64
	}{
		{"Upper", 13, 77, 76}, {"", 13, 77, 76}, {"ok", 75, 77, 76}, {"ok", 13, 200, 76}, {"ok", 13, 77, 1}, {"ok", 13, 77, 9000}, {"ok", math.NaN(), 77, 76},
	} {
		if _, err := New(c.name, c.lat, c.lng, c.edge); err == nil {
			t.Errorf("New(%q, %v, %v, %v) accepted", c.name, c.lat, c.lng, c.edge)
		}
	}
}

func TestTheAntimeridianTakesTheShortWay(t *testing.T) {
	g, err := New("fiji", 0, 179.9, 76)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := g.CellAt(Point{0, 179.9995})
	b, _ := g.CellAt(Point{0, -179.9995})
	if Distance(a, b) > 1 {
		t.Fatalf("111 m apart across the antimeridian, %d cells apart", Distance(a, b))
	}
	if c := g.Centre(b); c.Lng < -180 || c.Lng > 180 {
		t.Fatalf("centre %v", c)
	}
}

func BenchmarkCellAt(b *testing.B) {
	g := Bengaluru()
	pts := cityPoints(1024)
	for i := 0; b.Loop(); i++ {
		_, _ = g.CellAt(pts[i%len(pts)])
	}
}

func (g *Grid) flatDistance(a, b Point) float64 {
	ax, ay := g.toXY(a)
	bx, by := g.toXY(b)
	return math.Hypot(ax-bx, ay-by)
}

func TestFarCellsDoNotWrap(t *testing.T) {
	if d := Distance(Cell{math.MaxInt32, 0}, Cell{math.MinInt32, 0}); d != math.MaxInt32-math.MinInt32 {
		t.Fatalf("distance %d", d)
	}
}

func TestAPointOnAnEdgeGetsOneOfItsTwoCells(t *testing.T) {
	g := Bengaluru()
	for q := int32(-40); q <= 40; q += 9 {
		for r := int32(-40); r <= 40; r += 9 {
			c := Cell{q, r}
			o := g.Outline(c)
			for i := range 6 {
				// The midpoint of each edge is shared by c and one neighbour.
				ax, ay := g.toXY(o[i])
				bx, by := g.toXY(o[(i+1)%6])
				mid := g.fromXY((ax+bx)/2, (ay+by)/2)
				first, _ := g.CellAt(mid)
				again, _ := g.CellAt(mid)
				if first != again || Distance(first, c) > 1 {
					t.Fatalf("the midpoint of edge %d of %v gave %v then %v", i, c, first, again)
				}
			}
		}
	}
}
