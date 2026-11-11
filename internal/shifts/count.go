package shifts

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// maxCountLines caps one hand count. The menu is a handful of items.
const maxCountLines = 50

// returnedItems reads the stock snapshot kept at shift end. An open shift, and a shift that ended
// before the snapshot was kept, return nothing.
func returnedItems(raw []byte, ended bool) ([]map[string]any, error) {
	out := []map[string]any{}
	if !ended || len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// submitStockCount records what the rider counted by hand against what the server expects on the
// scooter. It does not change the stock; ops read the mismatches.
func (s *Service) submitStockCount(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Lines []struct {
			ItemID string `json:"item_id"`
			Qty    *int   `json:"qty"`
		} `json:"lines"`
		Reason string `json:"reason"`
	}
	ok := json.NewDecoder(r.Body).Decode(&body) == nil
	reason := strings.TrimSpace(body.Reason)
	ok = ok && len(body.Lines) >= 1 && len(body.Lines) <= maxCountLines && reason != "" && utf8.RuneCountInString(reason) <= 200
	seen := map[string]bool{}
	for _, l := range body.Lines {
		if !ok || !idPattern.MatchString(l.ItemID) || l.Qty == nil || *l.Qty < 0 || *l.Qty > 100000 || seen[strings.ToLower(l.ItemID)] {
			ok = false
			break
		}
		seen[strings.ToLower(l.ItemID)] = true
	}
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send each item once with a qty of 0 or more, and a reason of up to 200 characters.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := readRider(ctx, tx, actor.ID); err != nil {
			return err
		}
		sh, scooter, open, err := openShift(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "Start a shift first.")
		}
		counted := make([]map[string]any, 0, len(body.Lines))
		mismatches := 0
		type line struct {
			item              string
			expected, counted int
		}
		lines := make([]line, 0, len(body.Lines))
		for _, l := range body.Lines {
			var known bool
			var expected int
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM menu_items WHERE id = $1),
					coalesce((SELECT sum(ss.on_hand) FROM scooter_stock ss JOIN lots x ON x.id = ss.lot_id WHERE ss.scooter_id = $2 AND x.item_id = $1), 0)::int`,
				l.ItemID, scooter).Scan(&known, &expected); err != nil {
				return err
			}
			if !known {
				return errInvalid
			}
			if expected != *l.Qty {
				mismatches++
			}
			lines = append(lines, line{l.ItemID, expected, *l.Qty})
			counted = append(counted, map[string]any{"item_id": l.ItemID, "expected": expected, "counted": *l.Qty})
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO stock_counts (rider_id, shift_id, scooter_id, reason, mismatches) VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
			actor.ID, sh.ID, scooter, reason, mismatches).Scan(&id); err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := tx.Exec(ctx, "INSERT INTO stock_count_lines (stock_count_id, item_id, expected, counted) VALUES ($1, $2, $3, $4)", id, l.item, l.expected, l.counted); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "stock.counted", EntityType: "stock_count", EntityID: id,
			After: map[string]any{"lines": len(lines), "mismatches": mismatches, "shift_id": sh.ID}, Reason: reason}); err != nil {
			return err
		}
		reply = map[string]any{"counted": counted, "mismatches": mismatches, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
