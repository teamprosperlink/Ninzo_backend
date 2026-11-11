package orders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

// MountRider adds the rider's offers and orders: getRiderState, listOffers, acceptOffer,
// declineOffer, markReady, getRiderOrder and handOver.
func (s *Service) MountRider(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/riders/me/state", s.riderState)
		g.Get("/riders/me/offers", s.listOffers)
		g.With(idem).Post("/offers/{offer_id}/accept", s.acceptOffer)
		g.With(idem).Post("/offers/{offer_id}/decline", s.declineOffer)
		g.With(idem).Post("/offers/{offer_id}/ready", s.markReady)
		g.Get("/orders/{order_id}", s.riderOrder)
		g.Get("/orders/{order_id}/route", s.getRoute)
		g.With(idem).Post("/orders/{order_id}/handover", s.handOver)
		g.With(idem).Post("/orders/{order_id}/call", s.startRiderCall)
		g.With(idem).Post("/riders/me/problems", s.reportProblem)
		g.Post("/riders/me/events", s.sendEventBatch)
	})
}

// riderOK refuses a blocked person or a suspended rider.
func riderOK(ctx context.Context, q querier, id string) error {
	var status, person string
	err := q.QueryRow(ctx, "SELECT r.status, p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", id).Scan(&status, &person)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ErrSessionInvalid
	}
	if err == nil && (person != "active" || status == "suspended") {
		err = errBlocked
	}
	return err
}

func (s *Service) riderReadable(r *http.Request, actor identity.Actor) error {
	live, err := identity.SessionLive(r.Context(), s.Pool, actor)
	if err != nil {
		return err
	}
	if !live {
		return identity.ErrSessionInvalid
	}
	return riderOK(r.Context(), s.Pool, actor.ID)
}

func riderSeq(ctx context.Context, q querier, rider string) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, "SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)", rider).Scan(&seq)
	return max(seq, 1), err
}

// timerLabel is the calm, fixed label the rider sees. It never counts down.
func timerLabel(seconds int) string {
	switch {
	case seconds < 60:
		return strconv.Itoa(seconds) + " secs"
	case seconds == 60:
		return "1 min"
	case seconds%60 == 0:
		return strconv.Itoa(seconds/60) + " mins"
	}
	label := strconv.FormatFloat(float64(seconds)/60, 'f', 1, 64)
	return strings.TrimSuffix(label, ".0") + " mins"
}

type offerRow struct {
	id, orderID, rider, token, state, orderState string
	endedReason, finalState                      *string
	expiresAt                                    time.Time
	acceptedAt, releasedAt, handedOverAt         *time.Time
	otpAvailable                                 bool
	timerLength                                  *int
	orderNo                                      int64
}

func readOffer(ctx context.Context, q querier, offerID string) (offerRow, error) {
	var f offerRow
	err := q.QueryRow(ctx, `SELECT f.id::text, f.order_id::text, f.rider_id::text, f.fencing_token, f.state, o.state, f.ended_reason, f.final_state,
			f.expires_at, f.rider_accepted_at, f.released_at, o.handed_over_at, o.timer_length_s, o.order_no,
			f.released_at IS NULL AND o.handover_otp IS NOT NULL
				AND ((o.state = 'ARRIVED' AND coalesce(o.min_wait_until <= o.arrived_at OR o.min_wait_until_seen, false)) OR o.otp_released_at IS NOT NULL)
		FROM offers f JOIN orders o ON o.id = f.order_id WHERE f.id = $1`, offerID).
		Scan(&f.id, &f.orderID, &f.rider, &f.token, &f.state, &f.orderState, &f.endedReason, &f.finalState,
			&f.expiresAt, &f.acceptedAt, &f.releasedAt, &f.handedOverAt, &f.timerLength, &f.orderNo, &f.otpAvailable)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, errNotFound
	}
	return f, err
}

func lines(ctx context.Context, q querier, orderID string) ([]map[string]any, error) {
	rows, err := q.Query(ctx, "SELECT item_id::text, name, qty FROM order_items WHERE order_id = $1 ORDER BY name, item_id", orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
		var id, name string
		var qty int
		err := r.Scan(&id, &name, &qty)
		return map[string]any{"item_id": id, "name": name, "qty": qty}, err
	})
}

func gateFor(ctx context.Context, q querier, orderID string) (map[string]any, error) {
	var id, label string
	var lat, lng float64
	var notes *string
	if err := q.QueryRow(ctx, `SELECT o.gate_id::text, h.label, h.lat, h.lng, h.entry_notes FROM orders o JOIN gate_history h ON h.id = o.gate_history_id
		WHERE o.id = $1`, orderID).Scan(&id, &label, &lat, &lng, &notes); err != nil {
		return nil, err
	}
	var entry any
	if notes != nil {
		entry = *notes
	}
	return map[string]any{"id": id, "label": label, "lat": lat, "lng": lng, "entry_notes": entry}, nil
}

// offerView is one offer as its rider sees it. An open offer past its time shows as expired.
func offerView(ctx context.Context, q querier, f offerRow, now time.Time) (map[string]any, error) {
	items, err := lines(ctx, q, f.orderID)
	if err != nil {
		return nil, err
	}
	gate, err := gateFor(ctx, q, f.orderID)
	if err != nil {
		return nil, err
	}
	state := f.state
	if state == "open" && !now.Before(f.expiresAt) {
		state = "expired"
	}
	var order any
	if f.state == "accepted" {
		order = f.orderID
	}
	return map[string]any{"id": f.id, "order_id": order, "items": items, "gate": gate, "expires_at": f.expiresAt.UTC().Format(time.RFC3339Nano),
		"state": state, "fencing_token": f.token, "order_number": OrderNumber(f.orderNo)}, nil
}

// riderOrderView is an order as the rider holding the offer sees it. Once the rider no longer has
// it, it shows the last state they saw and why, never another rider's progress.
func riderOrderView(ctx context.Context, q querier, f offerRow) (map[string]any, error) {
	items, err := lines(ctx, q, f.orderID)
	if err != nil {
		return nil, err
	}
	gate, err := gateFor(ctx, q, f.orderID)
	if err != nil {
		return nil, err
	}
	seq, err := riderSeq(ctx, q, f.rider)
	if err != nil {
		return nil, err
	}
	length := 0
	if f.timerLength != nil {
		length = *f.timerLength
	} else if length, _, err = pinned(ctx, q, f.orderID, "timer_length_s"); err != nil {
		return nil, err
	}
	state, ended := f.orderState, any(nil)
	if f.releasedAt != nil {
		if f.finalState != nil {
			state = *f.finalState
		}
		if f.endedReason != nil {
			ended = *f.endedReason
		}
	}
	return map[string]any{"id": f.orderID, "offer_id": f.id, "fencing_token": f.token, "state": state, "seq": seq, "ended_reason": ended,
		"items": items, "gate": gate, "timer_label": timerLabel(length), "rider_accepted_at": stamp(f.acceptedAt),
		"otp_available": f.otpAvailable, "handed_over_at": stamp(f.handedOverAt), "order_number": OrderNumber(f.orderNo)}, nil
}

func (s *Service) riderState(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if err := s.riderReadable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, now := r.Context(), s.clock()
	seq, err := riderSeq(ctx, s.Pool, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var shift any
	var id, node string
	var started time.Time
	err = s.Pool.QueryRow(ctx, "SELECT id::text, node_id::text, started_at FROM shifts WHERE rider_id = $1 AND ended_at IS NULL", actor.ID).Scan(&id, &node, &started)
	switch {
	case err == nil:
		var assigned string
		var scooter map[string]any
		if assigned, scooter, err = shifts.Fill(ctx, s.Pool, id, started); err != nil {
			s.fail(w, r, err)
			return
		}
		shift = map[string]any{"id": id, "node_id": node, "started_at": started.UTC().Format(time.RFC3339Nano), "ended_at": nil,
			"node_assigned_at": assigned, "scooter": scooter}
	case !errors.Is(err, pgx.ErrNoRows):
		s.fail(w, r, err)
		return
	}
	offers := []map[string]any{}
	var current any
	var live string
	err = s.Pool.QueryRow(ctx, "SELECT id::text FROM offers WHERE rider_id = $1 AND released_at IS NULL", actor.ID).Scan(&live)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, err)
		return
	}
	if live != "" {
		f, err := readOffer(ctx, s.Pool, live)
		if err == nil && f.state == "open" {
			var v map[string]any
			if v, err = offerView(ctx, s.Pool, f, now); err == nil {
				offers = append(offers, v)
			}
		} else if err == nil {
			current, err = riderOrderView(ctx, s.Pool, f)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	restockNow, err := restock.Current(ctx, s.Pool, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"seq": seq, "shift": shift, "offers": offers, "current_order": current,
		"restock_request": restockNow, "server_time": serverTime()})
}

// listOffers shows the live offer, and offers that ended in the last 10 minutes and were never
// accepted, so a rider who looks away sees what happened.
func (s *Service) listOffers(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if err := s.riderReadable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, now := r.Context(), s.clock()
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM offers WHERE rider_id = $1
		AND (state = 'open' OR (state <> 'accepted' AND updated_at > now() - interval '10 minutes'))
		ORDER BY created_at DESC, id DESC LIMIT 20`, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		f, err := readOffer(ctx, s.Pool, id)
		var v map[string]any
		if err == nil {
			v, err = offerView(ctx, s.Pool, f, now)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"offers": out, "server_time": serverTime()})
}

func (s *Service) riderOrder(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	if err := s.riderReadable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	if !idPattern.MatchString(id) {
		s.fail(w, r, errNotFound)
		return
	}
	// Only an order this rider accepted. An offer never accepted shows through listOffers.
	var offer string
	err := s.Pool.QueryRow(r.Context(), `SELECT id::text FROM offers WHERE order_id = $1 AND rider_id = $2 AND state = 'accepted'
		ORDER BY created_at DESC, id DESC LIMIT 1`, id, actor.ID).Scan(&offer)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	var view map[string]any
	if err == nil {
		var f offerRow
		if f, err = readOffer(r.Context(), s.Pool, offer); err == nil {
			view, err = riderOrderView(r.Context(), s.Pool, f)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"order": view, "server_time": serverTime()})
}

// onOffer runs a rider's action on an offer. It checks the rider and the token, then takes the
// order's row lock and the offer's, in that order, as every change to an order does.
func (s *Service) onOffer(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, tx pgx.Tx, f offerRow, now time.Time) (any, error)) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "offer_id")
	var body struct {
		Token  string `json:"fencing_token"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" || len(body.Token) > 100 || utf8.RuneCountInString(body.Reason) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send fencing_token.", false)
		return
	}
	var reply any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := riderOK(ctx, tx, actor.ID); err != nil {
			return err
		}
		if !idPattern.MatchString(id) {
			return errNotFound
		}
		f, err := readOffer(ctx, tx, id)
		if err != nil {
			return err
		}
		if f.rider != actor.ID {
			return errNotFound
		}
		if body.Token != f.token {
			return &refusal{"stale_fencing_token", "This device holds an older offer. Refresh."}
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", f.orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM offers WHERE id = $1 FOR UPDATE", f.id); err != nil {
			return err
		}
		if f, err = readOffer(ctx, tx, id); err != nil {
			return err
		}
		// The clock is read under the lock, so the offer's end is decided under it.
		if reply, err = act(ctx, tx, f, s.clock()); err != nil {
			return err
		}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

func expired() error { return &refusal{"offer_expired", "This offer has ended."} }

func (s *Service) acceptOffer(w http.ResponseWriter, r *http.Request) {
	s.onOffer(w, r, func(ctx context.Context, tx pgx.Tx, f offerRow, now time.Time) (any, error) {
		switch f.state {
		case "accepted":
			if f.releasedAt != nil {
				return nil, expired()
			}
		case "taken":
			return nil, &refusal{"offer_taken", "Another rider has this order."}
		case "open":
			if !now.Before(f.expiresAt) {
				return nil, expired()
			}
			var onShift bool
			// The open shift must be on the scooter that holds this order's stock.
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shifts sh JOIN offers f ON f.id = $2 AND f.scooter_id = sh.scooter_id
				WHERE sh.rider_id = $1 AND sh.ended_at IS NULL)`, f.rider, f.id).Scan(&onShift); err != nil {
				return nil, err
			}
			if !onShift {
				return nil, expired()
			}
			if err := accept(ctx, tx, f, now); err != nil {
				return nil, err
			}
			var err error
			if f, err = readOffer(ctx, tx, f.id); err != nil {
				return nil, err
			}
		default:
			return nil, expired()
		}
		view, err := riderOrderView(ctx, tx, f)
		return map[string]any{"order": view, "server_time": serverTime()}, err
	})
}

// accept fixes the timer length and radius on the order, as the README says, so a later console
// change never alters this order.
func accept(ctx context.Context, tx pgx.Tx, f offerRow, now time.Time) error {
	if _, err := tx.Exec(ctx, "UPDATE offers SET state = 'accepted', rider_accepted_at = $2, updated_at = now() WHERE id = $1", f.id, now); err != nil {
		return err
	}
	fixed := map[string]int{}
	for _, name := range []string{"timer_length_s", "zone_radius_m", "arrival_perimeter_m"} {
		v, ok, err := pinned(ctx, tx, f.orderID, name)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("orders: required setting missing: " + name)
		}
		fixed[name] = v
	}
	otp, err := newOTP()
	if err != nil {
		return err
	}
	// The customer sees rider_accepted only once the minimum wait has passed. If it already has,
	// that is a change they can see, so seq rises now.
	if _, err := tx.Exec(ctx, `UPDATE orders SET state = 'RIDER_ACCEPTED', rider_id = $2, rider_accepted_at = $3,
			timer_length_s = $4, zone_radius_m = $5, arrival_perimeter_m = $6,
			seq = seq + CASE WHEN coalesce(min_wait_until <= $3, false) THEN 1 ELSE 0 END,
			min_wait_until_seen = coalesce(min_wait_until <= $3, false), handover_otp = $7, updated_at = now()
		WHERE id = $1`, f.orderID, f.rider, now, fixed["timer_length_s"], fixed["zone_radius_m"], fixed["arrival_perimeter_m"], otp); err != nil {
		return err
	}
	if err := event(ctx, tx, f.orderID, "OFFERED", "RIDER_ACCEPTED", identity.TypeRider, f.rider, "accepted"); err != nil {
		return err
	}
	if err := bumpRider(ctx, tx, f.rider); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: f.rider, Action: "offer.accepted", EntityType: "order", EntityID: f.orderID})
}

func (s *Service) declineOffer(w http.ResponseWriter, r *http.Request) {
	s.onOffer(w, r, func(ctx context.Context, tx pgx.Tx, f offerRow, now time.Time) (any, error) {
		switch f.state {
		case "declined":
		case "open":
			if !now.Before(f.expiresAt) {
				return nil, expired()
			}
			// No penalty. The reason is not kept. The next pass, within seconds, offers the order
			// to the next rider, so a clash with another order's offer never fails this decline.
			if err := releaseOffer(ctx, tx, f.id, "declined", "declined", identity.TypeRider, f.rider); err != nil {
				return nil, err
			}
		default:
			return nil, expired()
		}
		return map[string]any{"server_time": serverTime()}, nil
	})
}

func (s *Service) markReady(w http.ResponseWriter, r *http.Request) {
	s.onOffer(w, r, func(ctx context.Context, tx pgx.Tx, f offerRow, now time.Time) (any, error) {
		if f.state != "accepted" {
			return nil, expired()
		}
		if f.releasedAt != nil {
			return nil, &refusal{"order_cancelled", "This order has ended."}
		}
		// The rider packs while stopped: a fresh ping showing movement refuses Ready.
		if f.orderState == "RIDER_ACCEPTED" {
			moving, err := riderMoving(ctx, tx, f.rider, now)
			if err != nil {
				return nil, err
			}
			if moving {
				return nil, &refusal{"rider_moving", "Stop before marking the order packed."}
			}
		}
		if err := markPacked(ctx, tx, f.orderID, identity.TypeRider, f.rider); err != nil {
			return nil, err
		}
		f, err := readOffer(ctx, tx, f.id)
		if err != nil {
			return nil, err
		}
		view, err := riderOrderView(ctx, tx, f)
		return map[string]any{"order": view, "server_time": serverTime()}, err
	})
}
