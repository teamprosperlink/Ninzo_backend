// Package demand records and forecasts where and when customers want food (issue 220, geo record
// W7). Each availability check is kept once per IST day, 30-minute band, blr76 cell, customer and
// outcome, with the customer as a day-salted hash; paid orders come from the orders table. Forecast
// v1 is a weighted mean of the same weekday and band in recent weeks.
package demand

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
)

// india: selling days and bands are Indian time. India has no daylight saving.
var india = time.FixedZone("IST", 5*3600+1800)

// Outcomes, as availability answers them.
const (
	Offered    = "offered"
	NoCoverage = "no_coverage"
	SoldOut    = "sold_out"
	ZoneOff    = "zone_off"
	OutOfArea  = "out_of_service_area"
)

var outcomes = map[string]bool{Offered: true, NoCoverage: true, SoldOut: true, ZoneOff: true, OutOfArea: true}

// Known says whether an outcome is one Record keeps.
func Known(outcome string) bool { return outcomes[outcome] }

// Slot is an IST day and 30-minute band (0 is 00:00–00:30).
type Slot struct {
	Day  time.Time // midnight IST
	Band int
}

// SlotOf gives the IST day and band of a moment.
func SlotOf(t time.Time) Slot {
	l := t.In(india)
	return Slot{Day: time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, india), Band: (l.Hour()*60 + l.Minute()) / 30}
}

// who is the day-salted hash of a customer: the same customer on another day is another value.
func who(customerID string, day time.Time) []byte {
	h := sha256.Sum256([]byte(customerID + "|" + day.Format("2006-01-02")))
	return h[:]
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type querier interface {
	execer
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Record keeps one availability check. A second check by the same customer at the same cell, band
// and outcome is not counted again.
func Record(ctx context.Context, q execer, customerID string, lat, lng float64, at time.Time, outcome string) error {
	if customerID == "" || !outcomes[outcome] {
		return errors.New("demand: a customer and a known outcome are needed")
	}
	g := hexgrid.Bengaluru()
	c, err := g.CellAt(hexgrid.Point{Lat: lat, Lng: lng})
	if err != nil {
		return err
	}
	s := SlotOf(at)
	_, err = q.Exec(ctx, `INSERT INTO demand_checks (day, band, cell, who, outcome) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`, s.Day.Format("2006-01-02"), s.Band, g.ID(c), who(customerID, s.Day), outcome)
	return err
}

// retention reads demand_retention_days from the global settings.
func retention(ctx context.Context, q querier) (int, error) {
	s, err := geo.LoadSettings(ctx, q, "")
	if err != nil {
		return 0, err
	}
	d, ok := s.Get("demand_retention_days")
	if !ok {
		return 0, errors.New("demand: demand_retention_days is not set")
	}
	return d, nil
}

// Forget deletes one customer's checks on every day the table holds (erasure), so rows a late prune
// left behind, or rows kept under a longer retention, go too. It has no production caller yet,
// like gatecells.RedactGates (#185).
func Forget(ctx context.Context, q querier, customerID string) (int64, error) {
	rows, err := q.Query(ctx, "SELECT DISTINCT day::text FROM demand_checks")
	if err != nil {
		return 0, err
	}
	var days []string
	var hashes [][]byte
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return 0, err
		}
		day, err := time.ParseInLocation("2006-01-02", d, india)
		if err != nil {
			rows.Close()
			return 0, err
		}
		days = append(days, d)
		hashes = append(hashes, who(customerID, day))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(days) == 0 {
		return 0, nil
	}
	tag, err := q.Exec(ctx, "DELETE FROM demand_checks WHERE day = ANY($1::date[]) AND who = ANY($2)", days, hashes)
	return tag.RowsAffected(), err
}

// Prune drops checks from IST days before the retention window.
func Prune(ctx context.Context, q querier, now time.Time) (int64, error) {
	days, err := retention(ctx, q)
	if err != nil {
		return 0, err
	}
	cut := SlotOf(now).Day.AddDate(0, 0, -days)
	tag, err := q.Exec(ctx, "DELETE FROM demand_checks WHERE day < $1", cut.Format("2006-01-02"))
	return tag.RowsAffected(), err
}

// Pruner runs Prune at most once per Every (an hour when unset).
type Pruner struct {
	Every time.Duration
	mu    sync.Mutex
	last  time.Time
}

// Maybe prunes when the last run was at least Every ago, and reports whether it ran.
func (p *Pruner) Maybe(ctx context.Context, q querier, now time.Time) (bool, error) {
	p.mu.Lock()
	every := p.Every
	if every <= 0 {
		every = time.Hour
	}
	if !p.last.IsZero() && now.Sub(p.last) < every {
		p.mu.Unlock()
		return false, nil
	}
	p.last = now
	p.mu.Unlock()
	_, err := Prune(ctx, q, now)
	return true, err
}

// Count is the demand at one cell in one slot.
type Count struct {
	Cell string
	Slot
	// Each is distinct customers within the cell, day and band, so a customer offered food and then
	// refused in the same band counts once in Asked but in both Offered and Refused.
	Asked   int // distinct customers who asked where a zone was on (offered or refused)
	Offered int // distinct customers offered food
	Refused int // distinct customers refused: no scooter in reach, or sold out
	Paid    int // orders paid for, at the gate's cell
}

// Counts gives demand per cell and slot for IST days from..to, inclusive.
func Counts(ctx context.Context, q querier, from, to time.Time) ([]Count, error) {
	type key struct {
		cell string
		day  string
		band int
	}
	byKey := map[key]*Count{}
	get := func(k key) (*Count, error) {
		if c, ok := byKey[k]; ok {
			return c, nil
		}
		d, err := time.ParseInLocation("2006-01-02", k.day, india)
		if err != nil {
			return nil, err
		}
		c := &Count{Cell: k.cell, Slot: Slot{Day: d, Band: k.band}}
		byKey[k] = c
		return c, nil
	}
	first, last := SlotOf(from).Day, SlotOf(to).Day
	rows, err := q.Query(ctx, `SELECT cell, day::text, band,
			count(DISTINCT who) FILTER (WHERE outcome IN ('offered', 'no_coverage', 'sold_out')),
			count(DISTINCT who) FILTER (WHERE outcome = 'offered'),
			count(DISTINCT who) FILTER (WHERE outcome IN ('no_coverage', 'sold_out'))
		FROM demand_checks WHERE day BETWEEN $1 AND $2 GROUP BY cell, day, band`,
		first.Format("2006-01-02"), last.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k key
		var asked, offered, refused int
		if err := rows.Scan(&k.cell, &k.day, &k.band, &asked, &offered, &refused); err != nil {
			rows.Close()
			return nil, err
		}
		c, err := get(k)
		if err != nil {
			rows.Close()
			return nil, err
		}
		c.Asked, c.Offered, c.Refused = asked, offered, refused
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Paid orders, at their gate's cell. A redacted gate no longer says where it was.
	rows, err = q.Query(ctx, `SELECT o.paid_at, gh.lat, gh.lng FROM orders o
		JOIN gate_history gh ON gh.id = o.gate_history_id AND gh.redacted_at IS NULL
		WHERE o.paid_at IS NOT NULL AND o.state NOT IN ('PAYMENT_PENDING', 'PAYMENT_FAILED')
			AND o.paid_at >= $1 AND o.paid_at < $2`, first, last.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g := hexgrid.Bengaluru()
	for rows.Next() {
		var at time.Time
		var lat, lng float64
		if err := rows.Scan(&at, &lat, &lng); err != nil {
			return nil, err
		}
		cell, err := g.CellAt(hexgrid.Point{Lat: lat, Lng: lng})
		if err != nil {
			continue
		}
		s := SlotOf(at)
		c, err := get(key{g.ID(cell), s.Day.Format("2006-01-02"), s.Band})
		if err != nil {
			return nil, err
		}
		c.Paid++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Count, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	return out, nil
}
