package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

const maxHubEvents = 200

var hubEventTypes = map[string]bool{"accept": true, "scan": true, "unscan": true, "shortfall": true, "ready": true, "quarantine": true}

var eventIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

type hubEvent struct {
	ID         string          `json:"event_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	DeviceTime string          `json:"device_time"`
}

// result is one event's outcome. Final results are kept, so a repeat returns them.
type result struct {
	accepted  bool
	code, msg string
	retryable bool
}

func rejectedEvent(code, msg string, retryable bool) result {
	return result{code: code, msg: msg, retryable: retryable}
}

// MountEvents adds the hub's offline event batch. It takes no Idempotency-Key: each event_id is one.
func (s *Service) MountEvents(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Post("/hub/events", s.events)
	})
}

// events applies a hub's queued scans and status changes in the order sent, each on its own, so
// one rejected event never blocks the rest. The batch fails only if its body cannot be read, or the
// staff member may not act at all.
func (s *Service) events(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Events []hubEvent `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) < 1 || len(body.Events) > maxHubEvents {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send events, from 1 to 200.", false)
		return
	}
	accepted := []string{}
	refused := []map[string]any{}
	for _, e := range body.Events {
		if r.Context().Err() != nil {
			refused = append(refused, map[string]any{"id": e.ID, "code": "internal_error", "message": "Try again shortly.", "retryable": true})
			continue
		}
		o, err := s.applyEvent(r.Context(), actor, e)
		if errors.Is(err, identity.ErrSessionInvalid) || errors.Is(err, errBlocked) {
			s.fail(w, r, err)
			return
		}
		if err != nil {
			s.Log.Error("hub: a queued event failed", "request_id", httpserver.RequestID(r.Context()), "type", e.Type, "error", err.Error())
			o = rejectedEvent("internal_error", "Try again shortly.", true)
		}
		if o.accepted {
			accepted = append(accepted, e.ID)
		} else {
			refused = append(refused, map[string]any{"id": e.ID, "code": o.code, "message": o.msg, "retryable": o.retryable})
		}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"accepted_ids": accepted, "rejected": refused, "server_time": serverTime()})
}

// applyEvent runs one event in its own transaction. Its effect runs in a savepoint, so a refusal
// rolls the effect back but its final result is still kept.
func (s *Service) applyEvent(ctx context.Context, actor identity.Actor, e hubEvent) (result, error) {
	if !eventIDPattern.MatchString(e.ID) {
		return rejectedEvent("invalid_event", "event_id must be 8 to 128 letters, digits, - or _.", false), nil
	}
	device, terr := time.Parse(time.RFC3339Nano, e.DeviceTime)
	var out result
	err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		st, err := staffIn(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		var isAccepted bool
		var code, msg *string
		err = tx.QueryRow(ctx, "SELECT accepted, code, message FROM hub_events WHERE staff_id = $1 AND event_id = $2", actor.ID, e.ID).Scan(&isAccepted, &code, &msg)
		switch {
		case err == nil:
			out = result{accepted: isAccepted}
			if !isAccepted {
				out.code, out.msg = deref(code), deref(msg)
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// The same action sent directly first, with this event_id as its Idempotency-Key, counts once.
		var direct bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM idempotency_keys WHERE actor_id = $1 AND key = $2 AND status IS NOT NULL)",
			actor.ID, e.ID).Scan(&direct); err != nil {
			return err
		}
		switch {
		case direct:
			out = result{accepted: true}
		case terr != nil:
			out = rejectedEvent("invalid_event", "Send device_time.", false)
		case !hubEventTypes[e.Type]:
			out = rejectedEvent("unknown_event_type", "Unknown event type.", false)
		default:
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			out, err = s.effect(ctx, sp, st, e)
			if err != nil {
				_ = sp.Rollback(ctx)
				return err
			}
			if out.accepted {
				if err := sp.Commit(ctx); err != nil {
					return err
				}
			} else if err := sp.Rollback(ctx); err != nil {
				return err
			}
		}
		if out.retryable {
			return nil // May succeed later, so it is checked again next time.
		}
		var codeValue, msgValue, deviceValue any
		if !out.accepted {
			codeValue, msgValue = out.code, out.msg
		}
		if terr == nil {
			deviceValue = device
		}
		kind := e.Type
		if !hubEventTypes[kind] {
			kind = "unknown" // Never store text the app made up.
		}
		_, err = tx.Exec(ctx, `INSERT INTO hub_events (staff_id, event_id, type, accepted, code, message, device_time) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			actor.ID, e.ID, kind, out.accepted, codeValue, msgValue, deviceValue)
		return err
	})
	return out, err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// effect applies one event, with the same rules as its endpoint.
func (s *Service) effect(ctx context.Context, tx pgx.Tx, st Staff, e hubEvent) (result, error) {
	var p struct {
		RequestID  string      `json:"request_id"`
		LotCode    string      `json:"lot_code"`
		ScannedAt  string      `json:"scanned_at"`
		LotNumbers []string    `json:"lot_numbers"`
		LotID      string      `json:"lot_id"`
		Reason     string      `json:"reason"`
		Note       string      `json:"note"`
		Action     string      `json:"action"`
		Lines      []ShortLine `json:"lines"`
		Substitute string      `json:"substitute_item_id"`
		OtherHub   string      `json:"other_hub_id"`
	}
	bad := rejectedEvent("invalid_event", "The payload does not match the event type.", false)
	if json.Unmarshal(e.Payload, &p) != nil {
		return bad, nil
	}
	now := time.Now()
	if e.Type == "quarantine" {
		return outcomeOf(QuarantineTx(ctx, tx, st, p.LotID, p.Reason, p.Note, now))
	}
	if !idPattern.MatchString(p.RequestID) {
		return bad, nil
	}
	var state, hub string
	err := tx.QueryRow(ctx, "SELECT state, hub_id::text FROM restock_requests WHERE id = $1 AND state <> 'suggested' FOR UPDATE", p.RequestID).Scan(&state, &hub)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hub != st.HubID) {
		return rejectedEvent("not_found", "No such request at this hub.", false), nil
	}
	if err != nil {
		return result{}, err
	}
	code := strings.TrimSpace(p.LotCode)
	switch e.Type {
	case "accept":
		return outcomeOf(AcceptTx(ctx, tx, st, p.RequestID, state, now))
	case "scan":
		if _, err := time.Parse(time.RFC3339, p.ScannedAt); err != nil || code == "" || len(code) > 64 {
			return bad, nil
		}
		_, _, err := ScanTx(ctx, tx, st, p.RequestID, state, code, now)
		return outcomeOf(err)
	case "unscan":
		if code == "" || len(code) > 64 {
			return bad, nil
		}
		return outcomeOf(UnscanTx(ctx, tx, st, p.RequestID, state, code))
	case "shortfall":
		return outcomeOf(ShortfallTx(ctx, tx, st, p.RequestID, state, Shortfall{Action: p.Action, Lines: p.Lines, Substitute: p.Substitute, OtherHub: p.OtherHub, Reason: p.Reason}))
	default: // ready
		if p.LotNumbers == nil || len(p.LotNumbers) > 100 {
			return bad, nil
		}
		for _, c := range p.LotNumbers {
			if c = strings.TrimSpace(c); c == "" || len(c) > 64 {
				return bad, nil
			}
		}
		return outcomeOf(ReadyTx(ctx, tx, st, p.RequestID, state, p.LotNumbers, now))
	}
}

// outcomeOf turns an endpoint's error into an event result: its refusal is final, a clash is retryable.
func outcomeOf(err error) (result, error) {
	var no *refusal
	switch {
	case err == nil:
		return result{accepted: true}, nil
	case errors.As(err, &no):
		return rejectedEvent(no.code, no.message, false), nil
	case errors.Is(err, errNotFound):
		return rejectedEvent("not_found", "Not found at this hub.", false), nil
	case errors.Is(err, errInvalid):
		return rejectedEvent("invalid_event", "The payload does not match the event type.", false), nil
	case errors.Is(err, errLeadOnly):
		return rejectedEvent("forbidden", "Only a hub lead can quarantine a lot.", false), nil
	case retryable(err):
		return rejectedEvent("internal_error", "Another change was saved at the same moment. Send it again.", true), nil
	}
	return result{}, err
}
