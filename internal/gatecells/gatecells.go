// Package gatecells builds and stores the road-time cell sets around each gate version (issue
// 196, geo record W2 step 3): the cells of the pilot grid whose centre is within the search radius
// of the gate, each with its road time to the gate from OSRM. Tiers are read later from these
// times and the settings in force, so changing a budget needs no rebuild.
package gatecells

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Router is what Build needs from internal/routing.
type Router interface {
	Table(ctx context.Context, sources []routing.Point, dest routing.Point, speedKmh float64) ([]routing.Leg, error)
}

// CellTime is one cell of a build.
type CellTime struct {
	Cell         hexgrid.Cell
	CentreMetres float64
	Reachable    bool
	RoadSeconds  float64 // zero when not reachable
	RoadMetres   float64
}

// Limits on the search radius, as the table allows.
const (
	MinRadiusM = 100
	MaxRadiusM = 5000
)

// ErrNoRoads is returned when OSRM did not answer, so the times would be straight lines. A
// straight line is never stored as a road time; the live path keeps its own fallback instead.
var ErrNoRoads = errors.New("gatecells: OSRM did not answer; nothing stored")

// fallbackSpeedKmh is only what routing needs for its straight-line answer, which Build refuses.
const fallbackSpeedKmh = 15

// Candidates are the cells whose centre lies within radius metres of the gate, nearest first
// in ring order, with each centre's straight-line distance.
func Candidates(g *hexgrid.Grid, gate hexgrid.Point, radiusM int) ([]CellTime, error) {
	if radiusM < MinRadiusM || radiusM > MaxRadiusM {
		return nil, fmt.Errorf("gatecells: radius must be %d to %d m", MinRadiusM, MaxRadiusM)
	}
	home, err := g.CellAt(gate)
	if err != nil {
		return nil, err
	}
	// Ring k reaches at least 1.5 × edge × k from the centre (across the flat sides; further at the
	// corners). One ring more covers the gate being off its own cell's centre.
	k := int(math.Ceil(float64(radiusM)/(1.5*g.Edge()))) + 1
	if k > hexgrid.MaxDisk {
		return nil, fmt.Errorf("gatecells: radius %d m needs more than %d rings on this grid", radiusM, hexgrid.MaxDisk)
	}
	disk, err := hexgrid.Disk(home, k)
	if err != nil {
		return nil, err
	}
	out := make([]CellTime, 0, len(disk))
	for _, c := range disk {
		d := metres(gate, g.Centre(c))
		if d <= float64(radiusM) {
			out = append(out, CellTime{Cell: c, CentreMetres: d})
		}
	}
	return out, nil
}

// Build finds the candidate cells and their road times to the gate.
func Build(ctx context.Context, r Router, g *hexgrid.Grid, gate hexgrid.Point, radiusM int) ([]CellTime, error) {
	cells, err := Candidates(g, gate, radiusM)
	if err != nil {
		return nil, err
	}
	sources := make([]routing.Point, len(cells))
	for i, c := range cells {
		p := g.Centre(c.Cell)
		sources[i] = routing.Point{Lat: p.Lat, Lng: p.Lng}
	}
	legs, err := r.Table(ctx, sources, routing.Point{Lat: gate.Lat, Lng: gate.Lng}, fallbackSpeedKmh)
	if err != nil {
		return nil, err
	}
	if len(legs) != len(cells) {
		return nil, fmt.Errorf("gatecells: %d answers for %d cells", len(legs), len(cells))
	}
	reachable := 0
	for i, l := range legs {
		if l.Source != routing.SourceOSRM {
			return nil, ErrNoRoads
		}
		if l.Reachable {
			cells[i].Reachable, cells[i].RoadSeconds, cells[i].RoadMetres = true, l.Seconds, l.Metres
			reachable++
		}
	}
	if reachable == 0 {
		// Not even the gate's own cell reaches it: the gate is off the road data (outside the
		// extract, or OSRM serving other data). Storing that would hide the problem.
		return nil, errors.New("gatecells: no cell reaches the gate; is the gate inside the road data?")
	}
	return cells, nil
}

// ErrGateChanged means the gate version was erased, or is not where the cells were built from,
// by the time the build was stored. Nothing is stored; build again from the current version.
var ErrGateChanged = errors.New("gatecells: the gate version was erased or moved while building; nothing stored")

// Store saves a build for a gate version and returns its id. from is the gate position the cells
// were built from. The gate version is locked and checked first, in this transaction, so an
// erasure that lands during a slow build can never be followed by exact cells around the home.
func Store(ctx context.Context, tx pgx.Tx, gateHistoryID, roadDataVersionID string, g *hexgrid.Grid, radiusM int, from hexgrid.Point, cells []CellTime) (string, error) {
	if len(cells) == 0 {
		return "", errors.New("gatecells: no cells to store")
	}
	// FOR SHARE conflicts with erasure's UPDATE of this row: one of the two waits for the other.
	var lat, lng float64
	var erased bool
	err := tx.QueryRow(ctx, `SELECT lat, lng, redacted_at IS NOT NULL FROM gate_history WHERE id = $1 FOR SHARE`, gateHistoryID).Scan(&lat, &lng, &erased)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("gatecells: no such gate version")
	}
	if err != nil {
		return "", err
	}
	if erased || lat != from.Lat || lng != from.Lng {
		return "", ErrGateChanged
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO gate_cell_builds (gate_history_id, road_data_version_id, grid, max_radius_m, cells)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text`, gateHistoryID, roadDataVersionID, g.Name(), radiusM, len(cells)).Scan(&id)
	if err != nil {
		return "", err
	}
	rows := make([][]any, len(cells))
	for i, c := range cells {
		var secs, m any
		if c.Reachable {
			secs, m = c.RoadSeconds, c.RoadMetres
		}
		rows[i] = []any{id, g.ID(c.Cell), c.CentreMetres, secs, m}
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"gate_cells"},
		[]string{"build_id", "cell", "centre_metres", "road_seconds", "road_metres"}, pgx.CopyFromRows(rows))
	if err != nil {
		return "", err
	}
	if int(n) != len(cells) {
		return "", fmt.Errorf("gatecells: stored %d of %d cells", n, len(cells))
	}
	return id, nil
}

// BuildInfo is a stored build's header.
type BuildInfo struct {
	ID, GateHistoryID, RoadDataVersionID, Grid string
	MaxRadiusM, Cells                          int
}

// querier is what Latest reads with.
type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// ErrNoBuild means the gate version has no build that is not redacted.
var ErrNoBuild = errors.New("gatecells: no build for this gate version")

// Latest is the newest build of a gate version that is not redacted, with its cells.
func Latest(ctx context.Context, q querier, g *hexgrid.Grid, gateHistoryID string) (BuildInfo, []CellTime, error) {
	var b BuildInfo
	err := q.QueryRow(ctx, `SELECT id::text, gate_history_id::text, road_data_version_id::text, grid, max_radius_m, cells
		FROM gate_cell_builds WHERE gate_history_id = $1 AND redacted_at IS NULL
		ORDER BY built_at DESC, id DESC LIMIT 1`, gateHistoryID).Scan(&b.ID, &b.GateHistoryID, &b.RoadDataVersionID, &b.Grid, &b.MaxRadiusM, &b.Cells)
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildInfo{}, nil, ErrNoBuild
	}
	if err != nil {
		return BuildInfo{}, nil, err
	}
	if b.Grid != g.Name() {
		return BuildInfo{}, nil, fmt.Errorf("gatecells: build %s is on grid %s, not %s", b.ID, b.Grid, g.Name())
	}
	rows, err := q.Query(ctx, `SELECT cell, centre_metres, road_seconds, road_metres FROM gate_cells
		WHERE build_id = $1 ORDER BY centre_metres, cell`, b.ID)
	if err != nil {
		return BuildInfo{}, nil, err
	}
	defer rows.Close()
	var cells []CellTime
	for rows.Next() {
		var id string
		var centre float32
		var secs, m *float32
		if err := rows.Scan(&id, &centre, &secs, &m); err != nil {
			return BuildInfo{}, nil, err
		}
		c, err := g.Parse(id)
		if err != nil {
			return BuildInfo{}, nil, err
		}
		ct := CellTime{Cell: c, CentreMetres: float64(centre)}
		if secs != nil && m != nil {
			ct.Reachable, ct.RoadSeconds, ct.RoadMetres = true, float64(*secs), float64(*m)
		}
		cells = append(cells, ct)
	}
	if err := rows.Err(); err != nil {
		return BuildInfo{}, nil, err
	}
	if len(cells) != b.Cells {
		return BuildInfo{}, nil, fmt.Errorf("gatecells: build %s has %d cells, its header says %d", b.ID, len(cells), b.Cells)
	}
	return b, cells, nil
}

// Redact marks every build of a customer's gates redacted and deletes their cells (erasure).
func Redact(ctx context.Context, tx pgx.Tx, customerID string) error {
	if _, err := tx.Exec(ctx, `UPDATE gate_cell_builds b SET redacted_at = now()
		FROM gate_history h JOIN gates g ON g.id = h.gate_id
		WHERE b.gate_history_id = h.id AND g.customer_id = $1 AND b.redacted_at IS NULL`, customerID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM gate_cells c USING gate_cell_builds b, gate_history h, gates g
		WHERE c.build_id = b.id AND b.gate_history_id = h.id AND h.gate_id = g.id AND g.customer_id = $1`, customerID)
	return err
}

// metres is the straight-line distance on a sphere, as routing.Metres.
func metres(a, b hexgrid.Point) float64 {
	return routing.Metres(routing.Point{Lat: a.Lat, Lng: a.Lng}, routing.Point{Lat: b.Lat, Lng: b.Lng})
}
