// Package placementplan builds and reads placement plans (issue 224, geo record W4 step 4): for a
// zone, for each IST weekday and half-hour with recorded demand, and for each number of free
// scooters, which staging nodes they should wait at. Demand is internal/demand's forecast per cell;
// the method is internal/placement's maximum expected covering on road times. Relocation reads
// them when relocation_by_plan is on (#226).
package placementplan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/demand"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/placement"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// decay weights recent weeks more in the forecast (week k back counts decay^(k−1)).
const decay = 0.8

var india = time.FixedZone("IST", 5*3600+1800)

// Errors a build can end with; nothing is stored.
var (
	ErrNoNodes    = errors.New("placementplan: the zone has no staging nodes")
	ErrNoScooters = errors.New("placementplan: the zone has no active scooters")
	ErrNoDemand   = errors.New("placementplan: no demand recorded in the zone in the window")
)

// DB is what a build needs: queries, and a transaction for the write.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Begin(context.Context) (pgx.Tx, error)
}

// Plan is where n free scooters wait at one weekday (0 is Sunday) and band.
type Plan struct {
	Weekday, Band, N  int
	Nodes             []string // one per scooter; a node repeats when several wait there
	Expected, Covered float64
	// NodeOrders, one per entry of Nodes, is the forecast paid orders in the band of the cells that
	// entry is the first, in pick order, to cover (#249). Nil for builds made before it.
	NodeOrders []float64
}

// Result describes a stored build.
type Result struct {
	BuildID                       string
	Nodes, Cells, MaxN, PlanCount int
	Plans                         []Plan
}

type node struct {
	id       string
	at       routing.Point
	capacity int
}

func setting(s geo.Effective, name string) (int, error) {
	v, ok := s.Get(name)
	if !ok {
		return 0, fmt.Errorf("placementplan: %s is not set", name)
	}
	return v, nil
}

// Build makes a zone's plans from the demand of the last placement_weeks weeks before now's IST day,
// and stores them as the zone's newest build.
func Build(ctx context.Context, db DB, r placement.Router, zone string, now time.Time) (Result, error) {
	s, err := geo.LoadSettings(ctx, db, zone)
	if err != nil {
		return Result{}, err
	}
	var budget, speed, busy, weeks int
	for name, dst := range map[string]*int{"sweet_spot_budget_s": &budget, "straight_line_speed_kmh": &speed, "placement_busy_share_pct": &busy, "placement_weeks": &weeks} {
		if *dst, err = setting(s, name); err != nil {
			return Result{}, err
		}
	}
	nodes, err := zoneNodes(ctx, db, zone)
	if err != nil {
		return Result{}, err
	}
	var scooters int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM scooters WHERE zone_id = $1 AND status = 'active' AND NOT is_test", zone).Scan(&scooters); err != nil {
		return Result{}, err
	}
	caps := make([]int, len(nodes))
	maxN := 0
	for i, n := range nodes {
		caps[i] = n.capacity
		maxN += n.capacity
	}
	maxN = min(maxN, scooters)
	if maxN < 1 {
		return Result{}, ErrNoScooters
	}

	today := demand.SlotOf(now).Day
	from, to := today.AddDate(0, 0, -7*weeks), today.AddDate(0, 0, -1)
	counts, err := demand.Counts(ctx, db, from, to)
	if err != nil {
		return Result{}, err
	}
	history := demand.NewHistory(counts, from, func(c demand.Count) float64 { return float64(c.Asked) })
	paid := demand.NewHistory(counts, from, func(c demand.Count) float64 { return float64(c.Paid) })
	cells, centres, err := zoneCells(ctx, db, zone, history)
	if err != nil {
		return Result{}, err
	}
	if len(cells) == 0 {
		return Result{}, ErrNoDemand
	}
	at := make([]routing.Point, len(nodes))
	for i, n := range nodes {
		at[i] = n.at
	}
	cov, err := placement.Matrix(ctx, r, at, centres, float64(budget), float64(speed))
	if err != nil {
		return Result{}, err
	}
	q := float64(busy) / 100
	res := Result{Nodes: len(nodes), Cells: len(cells), MaxN: maxN}
	for wd := range 7 {
		// The next such weekday from today (today itself included).
		day := today.AddDate(0, 0, (wd-int(today.Weekday())+7)%7)
		for band := range 48 {
			slot := demand.Slot{Day: day, Band: band}
			w := make([]float64, len(cells))
			has := false
			for i, c := range cells {
				if f, ok := history.Forecast(c, slot, weeks, decay); ok && f > 0 {
					w[i], has = f, true
				}
			}
			if !has {
				continue
			}
			full, err := placement.GreedyCaps(cov, w, maxN, q, caps)
			if err != nil {
				return Result{}, err
			}
			// Each cell's forecast orders go to the first pick that covers it. Plans for fewer
			// scooters are prefixes of the full one, so the same split holds for every n.
			orders := make([]float64, len(full.Picks))
			for d, c := range cells {
				f, ok := paid.Forecast(c, slot, weeks, decay)
				if !ok || f <= 0 {
					continue
				}
				for i, pick := range full.Picks {
					if cov[pick][d] {
						orders[i] += f
						break
					}
				}
			}
			for n := 1; n <= maxN; n++ {
				picks := full.Picks[:n]
				exp, covd := placement.Score(cov, w, q, picks)
				ids := make([]string, n)
				for i, c := range picks {
					ids[i] = nodes[c].id
				}
				res.Plans = append(res.Plans, Plan{Weekday: wd, Band: band, N: n, Nodes: ids, Expected: exp, Covered: covd,
					NodeOrders: slices.Clone(orders[:n])})
			}
		}
	}
	if len(res.Plans) == 0 {
		return Result{}, ErrNoDemand
	}
	res.PlanCount = len(res.Plans)
	tx, err := db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `INSERT INTO placement_builds (zone_id, demand_from, demand_to, busy_share_pct, budget_s, nodes, cells, plans)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		zone, from.Format("2006-01-02"), to.Format("2006-01-02"), busy, budget, res.Nodes, res.Cells, res.PlanCount).Scan(&res.BuildID); err != nil {
		return Result{}, err
	}
	// One round trip for all rows: a build can hold 7 × 48 × n plans, and staging is far away.
	batch := &pgx.Batch{}
	for _, p := range res.Plans {
		batch.Queue(`INSERT INTO placement_plans (build_id, weekday, band, n, node_ids, expected, covered, node_orders)
			VALUES ($1, $2, $3, $4, $5::uuid[], $6, $7, $8::float8[])`, res.BuildID, p.Weekday, p.Band, p.N, p.Nodes, p.Expected, p.Covered, p.NodeOrders)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return Result{}, err
	}
	return res, tx.Commit(ctx)
}

func zoneNodes(ctx context.Context, db DB, zone string) ([]node, error) {
	rows, err := db.Query(ctx, "SELECT id::text, lat, lng, capacity FROM staging_nodes WHERE zone_id = $1 AND NOT is_test ORDER BY id", zone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []node
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.id, &n.at.Lat, &n.at.Lng, &n.capacity); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoNodes
	}
	return out, nil
}

// zoneCells lists, sorted, the cells with demand whose centre lies in the zone's area.
func zoneCells(ctx context.Context, db DB, zone string, h demand.History) ([]string, []routing.Point, error) {
	g := hexgrid.Bengaluru()
	var ids []string
	var lats, lngs []float64
	for id := range h.At {
		c, err := g.Parse(id)
		if err != nil {
			continue
		}
		p := g.Centre(c)
		ids = append(ids, id)
		lats = append(lats, p.Lat)
		lngs = append(lngs, p.Lng)
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	rows, err := db.Query(ctx, `SELECT t.id, t.lat, t.lng FROM unnest($2::text[], $3::float8[], $4::float8[]) AS t(id, lat, lng)
		JOIN zones z ON z.id = $1
		WHERE extensions.ST_Covers(z.area, extensions.ST_SetSRID(extensions.ST_MakePoint(t.lng, t.lat), 4326))
		ORDER BY t.id`, zone, ids, lats, lngs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var cells []string
	var centres []routing.Point
	for rows.Next() {
		var id string
		var p routing.Point
		if err := rows.Scan(&id, &p.Lat, &p.Lng); err != nil {
			return nil, nil, err
		}
		cells = append(cells, id)
		centres = append(centres, p)
	}
	return cells, centres, rows.Err()
}

// Latest reads the plan for n free scooters at a moment from a zone's newest build. ok is false when
// the zone has no build, or no plan for that weekday and band (no demand then), or for that n.
func Latest(ctx context.Context, db DB, zone string, at time.Time, n int) (Plan, bool, error) {
	l := at.In(india)
	p := Plan{Weekday: int(l.Weekday()), Band: (l.Hour()*60 + l.Minute()) / 30, N: n}
	err := db.QueryRow(ctx, `SELECT array(SELECT x::text FROM unnest(p.node_ids) AS x), p.expected, p.covered, p.node_orders
		FROM placement_plans p
		WHERE p.build_id = (SELECT id FROM placement_builds WHERE zone_id = $1 ORDER BY built_at DESC, id DESC LIMIT 1)
			AND p.weekday = $2 AND p.band = $3 AND p.n = $4`, zone, p.Weekday, p.Band, n).Scan(&p.Nodes, &p.Expected, &p.Covered, &p.NodeOrders)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, err
	}
	return p, true, nil
}
