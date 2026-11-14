package orders

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// checkGate answers the rider app's "does this gate code match?" (Issue 235, part B3).
//
// It only reads. It takes no Idempotency-Key, writes nothing, raises no seq and never touches the
// order, the timer or arrival: arrival comes from the rider's location alone (decisions 0007 and
// 0012), so neither answer here can mark the rider as arrived or block a handover.
//
// A typed code: no gate in our data has one (the customer saves a pin, a label, a landmark and entry
// notes; the console's gate edit changes the same), so the answer is the contract's "no code on record":
// checked false, matched true.
//
// A scanned QR: the rider app's QR reads NZG|<gate id>. It is compared with the id of the gate the order
// was placed to, and the answer is checked true with matched true or false. A QR in any other shape
// is checked true, matched false. The gate id is the only thing it carries, and it is already in the order
// the rider holds, so a QR tells the rider nothing new. What the rider scanned or typed is not stored,
// logged or audited.
//
// The order must be one this rider accepted, still holds, and has not finished. Anything else is 404,
// whoever owns it, so a rider learns nothing about another rider's order. The reply holds no
// customer data.
func (s *Service) checkGate(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if err := s.riderReadable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Method string `json:"method"`
		Value  string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Method != "qr" && body.Method != "code") ||
		utf8.RuneCountInString(body.Value) < 1 || utf8.RuneCountInString(body.Value) > 64 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send method (qr or code) and a value of 1 to 64 characters.", false)
		return
	}
	id := chi.URLParam(r, "order_id")
	if !idPattern.MatchString(id) {
		s.fail(w, r, errNotFound)
		return
	}
	var gate string
	err := s.Pool.QueryRow(r.Context(), `SELECT o.gate_id::text FROM offers f JOIN orders o ON o.id = f.order_id
		WHERE f.order_id = $1 AND f.rider_id = $2 AND f.state = 'accepted' AND f.released_at IS NULL
			AND o.state NOT IN ('HANDED_OVER', 'COMPLETED', 'CANCELLED', 'TIMED_OUT', 'FAILED')`, id, actor.ID).Scan(&gate)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	checked, matched := true, qrMatches(body.Value, gate)
	if body.Method == "code" {
		// No gate has a typed code, so there is nothing to check, and that is a match.
		checked, matched = false, true
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"checked": checked, "matched": matched, "server_time": serverTime()})
}

// qrPrefix starts every gate QR the rider app reads: NZG|<gate id>.
const qrPrefix = "NZG|"

// qrMatches says whether a scanned value is a gate QR for this gate. The id is compared without regard
// to letter case. Anything that is not exactly NZG| and a gate id does not match.
func qrMatches(value, gate string) bool {
	id, ok := strings.CutPrefix(strings.TrimSpace(value), qrPrefix)
	return ok && idPattern.MatchString(id) && strings.EqualFold(id, gate)
}
