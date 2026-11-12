// Package riderpos keeps where each rider is now (issue 207, geo record W3): the cell of the pilot
// grid each ping falls in, and one position row per rider that follows the newest ping by device
// time. It answers which tier around a gate a rider is in, for matching (W5).
package riderpos

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/gatecells"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
)

// Ping is what Record keeps from a rider's ping.
type Ping struct {
	Lat, Lng, SpeedMps, AccuracyM float64
	DeviceTime, ReceivedAt        time.Time
}

// Cell is the ping's cell id on the grid, or nil for a point beyond the grid's reach (the column
// then holds null).
func Cell(g *hexgrid.Grid, lat, lng float64) *string {
	c, err := g.CellAt(hexgrid.Point{Lat: lat, Lng: lng})
	if err != nil {
		return nil
	}
	id := g.ID(c)
	return &id
}

// FutureLimit is how far ahead of the server a ping's device time may be and still move the
// rider, as tracking allows for the timer. A phone clock running fast (or a forged ping) would
// otherwise set a device time no later ping could beat, and freeze the position.
const FutureLimit = 30 * time.Second

// Record moves the rider's position to this ping, unless the stored one is newer or as new by device
// time (a late or out-of-order ping never moves a rider back; of two with the same device time the
// first received stays) or the ping is from the future. It runs in the ping's transaction.
func Record(ctx context.Context, tx pgx.Tx, g *hexgrid.Grid, riderID string, p Ping) error {
	if p.DeviceTime.After(p.ReceivedAt.Add(FutureLimit)) {
		return nil // kept in location_pings as evidence; it does not move the rider
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO rider_positions (rider_id, lat, lng, cell, speed_mps, accuracy_m, device_time, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (rider_id) DO UPDATE SET lat = EXCLUDED.lat, lng = EXCLUDED.lng, cell = EXCLUDED.cell,
			speed_mps = EXCLUDED.speed_mps, accuracy_m = EXCLUDED.accuracy_m,
			device_time = EXCLUDED.device_time, received_at = EXCLUDED.received_at
		WHERE rider_positions.device_time < EXCLUDED.device_time`,
		riderID, p.Lat, p.Lng, Cell(g, p.Lat, p.Lng), p.SpeedMps, p.AccuracyM, p.DeviceTime.UTC(), p.ReceivedAt.UTC())
	return err
}

// Position is a rider's latest known position.
type Position struct {
	Lat, Lng, SpeedMps, AccuracyM float64
	Cell                          *string
	DeviceTime, ReceivedAt        time.Time
}

// ErrNoPosition means the rider has sent no ping yet.
var ErrNoPosition = errors.New("riderpos: no position for this rider")

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Latest is the rider's position now.
func Latest(ctx context.Context, q querier, riderID string) (Position, error) {
	var p Position
	err := q.QueryRow(ctx, `SELECT lat, lng, cell, speed_mps, accuracy_m, device_time, received_at
		FROM rider_positions WHERE rider_id = $1`, riderID).Scan(&p.Lat, &p.Lng, &p.Cell, &p.SpeedMps, &p.AccuracyM, &p.DeviceTime, &p.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, ErrNoPosition
	}
	return p, err
}

// RiderTier is where a rider is around a gate.
type RiderTier struct {
	Tier gatecells.Tier
	// InCellSet: the rider's cell is one of the gate's cells. When false the rider is beyond the
	// build's search radius (or off the grid), and Tier.Reach is ReachBeyond.
	InCellSet bool
	Position  Position
	// Age is how old the position is at now, by its device time: a ping queued offline and sent
	// later is as old as when it was taken, not when it arrived.
	Age time.Duration
}

// TierFor answers which tier of a gate version's newest build the rider is in, read with the
// settings given. It returns ErrNoPosition with no ping yet, and gatecells.ErrNoBuild with no build.
func TierFor(ctx context.Context, q querier, g *hexgrid.Grid, riderID, gateHistoryID string, s gatecells.Getter, now time.Time) (RiderTier, error) {
	pos, err := Latest(ctx, q, riderID)
	if err != nil {
		return RiderTier{}, err
	}
	t, err := gatecells.Tiers(ctx, q, g, gateHistoryID, s)
	if err != nil {
		return RiderTier{}, err
	}
	out := RiderTier{Position: pos, Age: now.Sub(pos.DeviceTime), Tier: gatecells.Tier{Reach: gatecells.ReachBeyond}}
	if out.Age < 0 {
		out.Age = 0 // a phone clock up to FutureLimit fast
	}
	if pos.Cell == nil {
		return out, nil
	}
	cell, err := g.Parse(*pos.Cell)
	if err != nil {
		// A cell from another grid: the rider is not on this one.
		return out, nil
	}
	out.Tier.Cell = cell
	for _, c := range t.Cells {
		if c.Cell == cell {
			out.Tier, out.InCellSet = c, true
			break
		}
	}
	return out, nil
}
