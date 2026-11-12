// Package hexgrid is the pilot's hexagon grid (decision G18, issue 193): pointy-topped hexagons
// of a fixed edge length in metres, laid on a local equirectangular projection around a city's
// origin. It does the four things the geo work needs (point to cell, cell centre, cells within k
// rings, cell outline) with no dependency, and every answer is exact for the grid it defines.
//
// It stands in for H3 resolution 10 (G7): the default edge of 76 m gives the same average cell
// area, about 0.015 km². Cell ids name their grid, so cells from two grids never mix. Moving to H3
// at city scale means rebuilding the cell sets, which happens with every road data version anyway.
package hexgrid

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const earthRadius = 6371000.0

// Grid is one hexagon grid.
type Grid struct {
	name                 string
	originLat, originLng float64
	edge                 float64 // metres, centre to corner
	cosLat               float64 // at the origin
}

// Point is a position in degrees.
type Point struct {
	Lat, Lng float64
}

// Cell is a hexagon in axial coordinates (q, r) on its grid.
type Cell struct {
	Q, R int32
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// ErrOutOfReach is returned for a point too far from the grid's origin for its flat projection.
var ErrOutOfReach = errors.New("hexgrid: point out of the grid's reach")

// maxReach is how far from the origin the grid is used. The grid is exact in its own plane; on
// the real Earth cells drift in size by about tan(lat) x (distance north or south / Earth radius):
// about 0.36% at 100 km near Bengaluru (13°N), more at higher latitudes.
const maxReach = 100000.0

// New makes a grid. The name goes into every cell id, so keep one name for one origin and edge.
func New(name string, originLat, originLng, edgeMetres float64) (*Grid, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("hexgrid: bad grid name %q", name)
	}
	if !(originLat > -60 && originLat < 60) || !(originLng >= -180 && originLng <= 180) {
		return nil, errors.New("hexgrid: origin out of range (|lat| under 60)")
	}
	if !(edgeMetres >= 5 && edgeMetres <= 5000) {
		return nil, errors.New("hexgrid: edge must be 5 to 5000 m")
	}
	return &Grid{name: name, originLat: originLat, originLng: originLng, edge: edgeMetres,
		cosLat: math.Cos(originLat * math.Pi / 180)}, nil
}

// Bengaluru is the pilot grid: origin at the city centre, 76 m edges (about H3 res 10 by area).
func Bengaluru() *Grid {
	// Built directly: the values are constants that New accepts (tested).
	return &Grid{name: "blr76", originLat: 12.9716, originLng: 77.5946, edge: 76, cosLat: math.Cos(12.9716 * math.Pi / 180)}
}

// Name is the grid's name.
func (g *Grid) Name() string { return g.name }

// Edge is the edge length in metres; the distance between neighbouring centres is √3 times it.
func (g *Grid) Edge() float64 { return g.edge }

// toXY projects a point to metres east and north of the origin.
func (g *Grid) toXY(p Point) (x, y float64) {
	dLng := p.Lng - g.originLng
	// Take the short way round the antimeridian.
	if dLng > 180 {
		dLng -= 360
	} else if dLng < -180 {
		dLng += 360
	}
	return earthRadius * dLng * math.Pi / 180 * g.cosLat, earthRadius * (p.Lat - g.originLat) * math.Pi / 180
}

func (g *Grid) fromXY(x, y float64) Point {
	lng := g.originLng + x/(earthRadius*g.cosLat)*180/math.Pi
	if lng > 180 {
		lng -= 360
	} else if lng < -180 {
		lng += 360
	}
	return Point{Lat: g.originLat + y/earthRadius*180/math.Pi, Lng: lng}
}

// CellAt is the cell a point falls in. A point exactly on an edge or corner always gets the same
// cell, by the rounding rule below.
func (g *Grid) CellAt(p Point) (Cell, error) {
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lng) || math.IsInf(p.Lat, 0) || math.IsInf(p.Lng, 0) ||
		p.Lat < -90 || p.Lat > 90 || p.Lng < -180 || p.Lng > 180 {
		return Cell{}, errors.New("hexgrid: point out of range")
	}
	x, y := g.toXY(p)
	if math.Hypot(x, y) > maxReach {
		return Cell{}, ErrOutOfReach
	}
	// Pointy-topped axial coordinates, then cube rounding to the nearest hexagon centre.
	q := (math.Sqrt(3)/3*x - y/3) / g.edge
	r := (2.0 / 3 * y) / g.edge
	return cubeRound(q, r), nil
}

// cubeRound rounds fractional axial coordinates to the hexagon that contains them: round all three
// cube coordinates, then fix the one that moved furthest so they still sum to zero.
func cubeRound(q, r float64) Cell {
	s := -q - r
	rq, rr, rs := math.Round(q), math.Round(r), math.Round(s)
	dq, dr, ds := math.Abs(rq-q), math.Abs(rr-r), math.Abs(rs-s)
	switch {
	case dq > dr && dq > ds:
		rq = -rr - rs
	case dr > ds:
		rr = -rq - rs
	}
	return Cell{Q: int32(rq), R: int32(rr)}
}

// Centre is the cell's centre.
func (g *Grid) Centre(c Cell) Point {
	x, y := g.centreXY(c)
	return g.fromXY(x, y)
}

func (g *Grid) centreXY(c Cell) (float64, float64) {
	q, r := float64(c.Q), float64(c.R)
	return g.edge * math.Sqrt(3) * (q + r/2), g.edge * 1.5 * r
}

// Outline is the cell's six corners, counter-clockwise from the east-north-east corner.
func (g *Grid) Outline(c Cell) [6]Point {
	cx, cy := g.centreXY(c)
	var out [6]Point
	for i := range 6 {
		a := math.Pi / 180 * float64(60*i+30) // pointy-topped: corners at 30, 90, 150 ... degrees
		out[i] = g.fromXY(cx+g.edge*math.Cos(a), cy+g.edge*math.Sin(a))
	}
	return out
}

// directions are the six neighbours in axial coordinates.
var directions = [6]Cell{{1, 0}, {1, -1}, {0, -1}, {-1, 0}, {-1, 1}, {0, 1}}

// Neighbours are the six cells that share an edge with c.
func Neighbours(c Cell) [6]Cell {
	var out [6]Cell
	for i, d := range directions {
		out[i] = Cell{c.Q + d.Q, c.R + d.R}
	}
	return out
}

// Distance is how many steps apart two cells are.
func Distance(a, b Cell) int {
	dq, dr := int(a.Q)-int(b.Q), int(a.R)-int(b.R) // in int, so far cells cannot wrap round
	return (abs(dq) + abs(dr) + abs(dq+dr)) / 2
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// MaxDisk is the largest k Disk answers (k = 60 is about 13 km at 76 m edges, 10,981 cells).
const MaxDisk = 60

// Disk is every cell within k steps of c, c first, then ring by ring.
func Disk(c Cell, k int) ([]Cell, error) {
	if k < 0 || k > MaxDisk {
		return nil, fmt.Errorf("hexgrid: k must be 0 to %d", MaxDisk)
	}
	out := make([]Cell, 0, 1+3*k*(k+1))
	out = append(out, c)
	for ring := 1; ring <= k; ring++ {
		// Start ring steps out in direction 4, then walk each of the six sides.
		cur := Cell{c.Q + directions[4].Q*int32(ring), c.R + directions[4].R*int32(ring)}
		for side := range 6 {
			for range ring {
				out = append(out, cur)
				cur = Cell{cur.Q + directions[side].Q, cur.R + directions[side].R}
			}
		}
	}
	return out, nil
}

// ID is the cell's text id on this grid, for example "blr76:12:-3".
func (g *Grid) ID(c Cell) string {
	return g.name + ":" + strconv.FormatInt(int64(c.Q), 10) + ":" + strconv.FormatInt(int64(c.R), 10)
}

// Parse reads an id made by ID on this grid. An id from another grid is refused.
func (g *Grid) Parse(id string) (Cell, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != g.name {
		return Cell{}, fmt.Errorf("hexgrid: %q is not a cell of grid %s", id, g.name)
	}
	q, err1 := strconv.ParseInt(parts[1], 10, 32)
	r, err2 := strconv.ParseInt(parts[2], 10, 32)
	if err1 != nil || err2 != nil {
		return Cell{}, fmt.Errorf("hexgrid: %q is not a cell id", id)
	}
	c := Cell{int32(q), int32(r)}
	if x, y := g.centreXY(c); math.Hypot(x, y) > maxReach+g.edge {
		return Cell{}, fmt.Errorf("hexgrid: %q is beyond the grid's reach", id)
	}
	if g.ID(c) != id { // refuse "+5", "05" and the like, so each cell has one id
		return Cell{}, fmt.Errorf("hexgrid: %q is not in canonical form", id)
	}
	return c, nil
}
