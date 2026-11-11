package orders

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
)

// errNoCalls answers a masked call while no telephony provider is set up (#151). The contract's
// call_not_available is the right answer: the call cannot be placed now.
var errNoCalls = &refusal{"call_not_available", "Calls are not available yet. Use the order's notes or support."}

// startCustomerCall is startMaskedCall: the customer calls the rider. Only for the customer's own
// order; until a telephony provider is chosen, every call is call_not_available.
func (s *Service) startCustomerCall(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if err := s.readable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := owned(r.Context(), s.Pool, chi.URLParam(r, "order_id"), actor.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.fail(w, r, errNoCalls)
}

// startRiderCall is startRiderMaskedCall: the rider calls the customer. Only for an order this
// rider accepted; until a telephony provider is chosen, every call is call_not_available.
func (s *Service) startRiderCall(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if err := s.riderReadable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	id := chi.URLParam(r, "order_id")
	if !idPattern.MatchString(id) {
		s.fail(w, r, errNotFound)
		return
	}
	var ok bool
	err := s.Pool.QueryRow(r.Context(), "SELECT true FROM offers WHERE order_id = $1 AND rider_id = $2 AND state = 'accepted' LIMIT 1", id, actor.ID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fail(w, r, errNoCalls)
}
