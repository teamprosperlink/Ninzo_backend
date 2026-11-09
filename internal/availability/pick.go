package availability

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
)

// Pick is the scooter chosen for an order, and what it was chosen under.
type Pick struct {
	ZoneID    string
	ScooterID string
	Settings  geo.Effective
	Reason    string // Set when no scooter can take the whole basket.
}

// PickScooter finds the nearest scooter in reach that holds the whole basket, and locks its stock
// rows, so the caller can hold units without anyone else taking them. One order comes from one
// scooter. basket maps item IDs to quantities.
func PickScooter(ctx context.Context, tx pgx.Tx, lat, lng float64, now time.Time, basket map[string]int) (Pick, error) {
	return PickScooterExcept(ctx, tx, lat, lng, now, basket, nil)
}

// PickScooterExcept is PickScooter leaving out some scooters, such as those whose rider is busy or
// has already passed on the order.
func PickScooterExcept(ctx context.Context, tx pgx.Tx, lat, lng float64, now time.Time, basket map[string]int, except []string) (Pick, error) {
	zone, found, err := geo.ZoneAt(ctx, tx, lat, lng)
	if err != nil {
		return Pick{}, err
	}
	if !found {
		return Pick{Reason: ReasonOutOfArea}, nil
	}
	settings, err := geo.LoadSettings(ctx, tx, zone)
	if err != nil {
		return Pick{}, err
	}
	pick := Pick{ZoneID: zone, Settings: settings}
	budget, _ := settings.Get("sweet_spot_budget_s")
	speed, _ := settings.Get("straight_line_speed_kmh")
	radius, _ := settings.Get("max_search_radius_m")
	reach := float64(radius)
	if r := float64(speed) / 3.6 * float64(budget); r < reach {
		reach = r
	}
	items := make([]string, 0, len(basket))
	for id := range basket {
		items = append(items, id)
	}
	sort.Strings(items)
	local := now.In(india)
	weekday := (int(local.Weekday()) + 6) % 7
	minute := local.Hour()*60 + local.Minute()
	// Every item must be on sale here, now.
	var onSale int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM menu_items m WHERE m.id = ANY($2::uuid[]) AND m.status = 'active'
		AND (NOT EXISTS (SELECT 1 FROM availability_rules ar WHERE ar.item_id = m.id AND (ar.zone_id IS NULL OR ar.zone_id = $1))
			OR EXISTS (SELECT 1 FROM availability_rules ar WHERE ar.item_id = m.id AND (ar.zone_id IS NULL OR ar.zone_id = $1)
				AND (ar.weekdays >> $3::int) & 1 = 1 AND $4::int >= ar.start_minute AND $4::int < ar.end_minute))`,
		zone, items, weekday, minute).Scan(&onSale); err != nil {
		return Pick{}, err
	}
	if onSale != len(items) {
		pick.Reason = ReasonSoldOut
		return pick, nil
	}
	rows, err := tx.Query(ctx, `SELECT r.id::text FROM (`+reachable+`) r JOIN scooters s ON s.id = r.id JOIN staging_nodes n ON n.id = s.node_id
		ORDER BY extensions.ST_Distance(
			extensions.ST_SetSRID(extensions.ST_MakePoint(n.lng, n.lat), 4326)::extensions.geography,
			extensions.ST_SetSRID(extensions.ST_MakePoint($3, $2), 4326)::extensions.geography), r.id`, zone, lat, lng, reach)
	if err != nil {
		return Pick{}, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Pick{}, err
	}
	left := map[string]bool{}
	for _, id := range except {
		left[id] = true
	}
	scooters := all[:0]
	for _, id := range all {
		if !left[id] {
			scooters = append(scooters, id)
		}
	}
	if len(scooters) == 0 {
		pick.Reason = ReasonNoCoverage
		return pick, nil
	}
	count := func(scooter string) (map[string]int, error) {
		rows, err := tx.Query(ctx, `SELECT l.item_id::text, sum(ss.on_hand - ss.reserved)::int
			FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id
			WHERE ss.scooter_id = $1 AND l.item_id = ANY($2::uuid[]) AND l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > now()
			GROUP BY l.item_id`, scooter, items)
		if err != nil {
			return nil, err
		}
		have := map[string]int{}
		var item string
		var qty int
		_, err = pgx.ForEachRow(rows, []any{&item, &qty}, func() error { have[item] = qty; return nil })
		return have, err
	}
	enough := func(have map[string]int) bool {
		for id, want := range basket {
			if have[id] < want {
				return false
			}
		}
		return true
	}
	for _, scooter := range scooters {
		have, err := count(scooter)
		if err != nil {
			return Pick{}, err
		}
		if !enough(have) {
			continue
		}
		// Lock only this scooter's rows, in one order, then count again: another order may have
		// taken units since the count above.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM scooter_stock WHERE scooter_id = $1 ORDER BY lot_id FOR UPDATE`, scooter); err != nil {
			return Pick{}, err
		}
		if have, err = count(scooter); err != nil {
			return Pick{}, err
		}
		if enough(have) {
			pick.ScooterID = scooter
			return pick, nil
		}
		break // Only one scooter is ever locked, so a lost race is a sold-out answer, not a wait.
	}
	pick.Reason = ReasonSoldOut
	return pick, nil
}
