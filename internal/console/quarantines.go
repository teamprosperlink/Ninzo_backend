package console

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// listQuarantinedStock shows the lots a rider quarantined on a scooter, and the lots a hub lead
// quarantined at a hub, newest first (Issue 235, part B3). Support reads it to answer a customer who
// asks why the food was not fresh. It only reads. The note a rider or hub lead typed never leaves.
//
// A rider's lot is found by its stock ledger entry (reason 'quarantine'), which names the scooter
// and the units that left it; a hub lead's lot is the lot itself, at its hub.
func (s *Service) listQuarantinedStock(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	q := r.URL.Query()
	zone, scooter := strings.ToLower(q.Get("zone_id")), strings.ToLower(q.Get("scooter_id"))
	var from, to *time.Time
	for name, dst := range map[string]**time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				ok = false
			}
			*dst = &t
		}
	}
	if !ok || (zone != "" && !idPattern.MatchString(zone)) || (scooter != "" && !idPattern.MatchString(scooter)) || (from != nil && to != nil && !from.Before(*to)) {
		badBody(w, "Bad zone_id, scooter_id, from, to, cursor or limit. from must be before to.")
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT l.id::text, l.code, i.id::text, i.name, coalesce(-g.delta_on_hand, l.qty), l.quarantine_reason, l.quarantined_at,
			l.quarantined_by_rider IS NOT NULL, sc.id::text, sc.code, rd.id::text, rd.name, h.id::text, h.name,
			ARRAY(SELECT x.order_id::text FROM reservations x WHERE x.lot_id = l.id ORDER BY x.created_at DESC, x.id DESC LIMIT 20)
		FROM lots l JOIN menu_items i ON i.id = l.item_id
		LEFT JOIN stock_ledger g ON g.lot_id = l.id AND g.reason = 'quarantine' AND l.quarantined_by_rider IS NOT NULL
		LEFT JOIN scooters sc ON sc.id = g.scooter_id
		LEFT JOIN riders rd ON rd.id = l.quarantined_by_rider
		LEFT JOIN hubs h ON h.id = l.hub_id AND l.quarantined_by_rider IS NULL
		WHERE l.status = 'quarantined' AND l.quarantined_at IS NOT NULL AND (l.quarantined_by_rider IS NOT NULL OR l.quarantined_by IS NOT NULL)
			AND ($1 = '' OR coalesce(sc.zone_id, h.zone_id)::text = $1) AND ($2 = '' OR sc.id::text = $2)
			AND ($3::timestamptz IS NULL OR l.quarantined_at >= $3) AND ($4::timestamptz IS NULL OR l.quarantined_at < $4)
			AND (l.quarantined_at, l.id) < ($5, $6::uuid)
		ORDER BY l.quarantined_at DESC, l.id DESC LIMIT $7`, zone, scooter, from, to, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type row struct {
		lot, code, item, itemName, reason                        string
		qty                                                      int
		at                                                       time.Time
		byRider                                                  bool
		scooterID, scooterCode, riderID, riderName, hubID, hubNm *string
		orders                                                   []string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.lot, &x.code, &x.item, &x.itemName, &x.qty, &x.reason, &x.at, &x.byRider, &x.scooterID, &x.scooterCode, &x.riderID, &x.riderName, &x.hubID, &x.hubNm, &x.orders)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].at, list[len(list)-1].lot)
	}
	pair := func(id, name *string, key string) any {
		if id == nil {
			return nil
		}
		n := ""
		if name != nil {
			n = *name
		}
		return map[string]any{"id": *id, key: n}
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		source := "hub"
		if x.byRider {
			source = "rider"
		}
		orders := x.orders
		if orders == nil {
			orders = []string{}
		}
		out = append(out, map[string]any{"lot_id": x.lot, "lot_code": x.code, "item": map[string]any{"id": x.item, "name": x.itemName}, "qty": x.qty,
			"source": source, "reason": x.reason, "quarantined_at": x.at.UTC().Format(time.RFC3339Nano),
			"scooter": pair(x.scooterID, x.scooterCode, "code"), "rider": pair(x.riderID, x.riderName, "name"), "hub": pair(x.hubID, x.hubNm, "name"), "order_ids": orders})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"lots": out, "next_cursor": next, "server_time": serverTime()})
}
