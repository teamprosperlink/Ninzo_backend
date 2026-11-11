package restock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// CancelTx cancels a rider's confirmed request before the hub accepts it (cancelRestock). A
// request already cancelled is returned again. Once the hub has accepted, a suggestion, or an
// expired or finished request cannot be cancelled. Whatever the request held goes back.
func (s *Service) CancelTx(ctx context.Context, tx pgx.Tx, rider, id, reason string, save bool) (map[string]any, error) {
	state, _, err := lockRequest(ctx, tx, id, rider)
	if err != nil {
		return nil, err
	}
	switch state {
	case "cancelled":
	case "confirmed":
		now := s.clock()
		var why any
		if reason != "" {
			why = reason
		}
		if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = 'cancelled', reason = coalesce($2, reason), state_changed_at = $3, updated_at = now() WHERE id = $1",
			id, why, now); err != nil {
			return nil, err
		}
		if err := release(ctx, tx, id, now); err != nil {
			return nil, err
		}
		if err := bumpRider(ctx, tx, rider, id); err != nil {
			return nil, err
		}
		// The hub saw this request while it was confirmed, so its seq rises even for a request that
		// began as a suggestion, which bumpRider treats as the rider's draft once cancelled.
		if _, err := tx.Exec(ctx, `INSERT INTO hub_state (hub_id, seq) SELECT hub_id, 1 FROM restock_requests WHERE id = $1
			ON CONFLICT (hub_id) DO UPDATE SET seq = hub_state.seq + 1, updated_at = now()`, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: rider, Action: "restock.cancelled", EntityType: "restock_request", EntityID: id,
			Reason: reason}); err != nil {
			return nil, err
		}
	default:
		return nil, &refusal{"restock_not_cancellable", "This request can no longer be cancelled."}
	}
	v, err := View(ctx, tx, id)
	return s.reply(ctx, tx, v, err, save)
}

func (s *Service) cancel(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
	}
	// The body is optional.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) || utf8.RuneCountInString(body.Reason) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason of up to 200 characters, or none.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := s.check(r, tx, actor); err != nil {
			return err
		}
		var err error
		reply, err = s.CancelTx(r.Context(), tx, actor.ID, chi.URLParam(r, "request_id"), body.Reason, true)
		return err
	})
	if err != nil {
		s.failTx(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
