package orders

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

const maxEvents = 500

var knownEvent = map[string]bool{"location": true, "handover": true, "problem": true, "stock_scan": true, "restock_confirm": true, "restock_decline": true}

var eventIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

type queued struct {
	ID         string          `json:"event_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	DeviceTime string          `json:"device_time"`
}

// outcome is one event's result. final results are kept, so a repeat returns them.
type outcome struct {
	accepted  bool
	code, msg string
	retryable bool
}

func rejected(code, msg string, retryable bool) outcome {
	return outcome{code: code, msg: msg, retryable: retryable}
}

// sendEventBatch applies a rider's queued actions in order, each on its own, so one bad event never
// blocks the queue (contracts/README.md, Rider offline events). The batch fails only if its body
// cannot be read.
func (s *Service) sendEventBatch(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Events []queued `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) < 1 || len(body.Events) > maxEvents {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send events, from 1 to 500.", false)
		return
	}
	accepted := []string{}
	refused := []map[string]any{}
	for _, e := range body.Events {
		if r.Context().Err() != nil {
			// Out of time: the rest are not tried, and the app sends them again.
			refused = append(refused, map[string]any{"id": e.ID, "code": "internal_error", "message": "Try again shortly.", "retryable": true})
			continue
		}
		o, err := s.applyEvent(r.Context(), actor, e)
		if errors.Is(err, identity.ErrSessionInvalid) || errors.Is(err, errBlocked) {
			s.fail(w, r, err) // The rider cannot act at all, so nothing in the queue can apply.
			return
		}
		if err != nil {
			s.Log.Error("orders: a queued event failed", "request_id", httpserver.RequestID(r.Context()), "type", e.Type, "error", err.Error())
			o = rejected("internal_error", "Try again shortly.", true)
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
// rolls back the effect but still keeps the final result.
func (s *Service) applyEvent(ctx context.Context, actor identity.Actor, e queued) (outcome, error) {
	if !eventIDPattern.MatchString(e.ID) {
		return rejected("invalid_event", "event_id must be 8 to 128 letters, digits, - or _.", false), nil
	}
	device, terr := time.Parse(time.RFC3339Nano, e.DeviceTime)
	var result outcome
	err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := riderOK(ctx, tx, actor.ID); err != nil {
			return err
		}
		// A repeat returns its first final result.
		var done, isAccepted bool
		var code, msg *string
		err := tx.QueryRow(ctx, "SELECT true, accepted, code, message FROM rider_events WHERE rider_id = $1 AND event_id = $2", actor.ID, e.ID).
			Scan(&done, &isAccepted, &code, &msg)
		switch {
		case err == nil:
			result = outcome{accepted: isAccepted}
			if !isAccepted {
				result.code, result.msg = deref(code), deref(msg)
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
		if direct {
			result = outcome{accepted: true}
		} else {
			if terr != nil {
				result = rejected("invalid_event", "Send device_time.", false)
			} else {
				sp, err := tx.Begin(ctx)
				if err != nil {
					return err
				}
				result, err = s.effect(ctx, sp, actor.ID, e, device.Truncate(time.Microsecond))
				if err != nil {
					_ = sp.Rollback(ctx)
					return err
				}
				if result.accepted || result.code == "wrong_otp" {
					// A wrong code still counts as an attempt.
					if err := sp.Commit(ctx); err != nil {
						return err
					}
				} else if err := sp.Rollback(ctx); err != nil {
					return err
				}
			}
		}
		if result.retryable {
			return nil // May succeed later, so it is checked again next time.
		}
		// A rejected handover may mean the food was already given, so support is told.
		if e.Type == "handover" && !result.accepted && (result.code == "wrong_otp" || result.code == "order_cancelled" || result.code == "order_reassigned") {
			var ref struct {
				OrderID string `json:"order_id"`
			}
			alert := map[string]any{"rider_id": actor.ID, "event_id": e.ID, "reason": "handover_" + result.code}
			if json.Unmarshal(e.Payload, &ref) == nil && idPattern.MatchString(ref.OrderID) {
				alert["order_id"] = ref.OrderID
			}
			if err := outbox.Add(ctx, tx, "support.alert", alert); err != nil {
				return err
			}
		}
		var codeValue, msgValue, deviceValue any
		if !result.accepted {
			codeValue, msgValue = result.code, result.msg
		}
		if terr == nil {
			deviceValue = device
		}
		kind := e.Type
		if !knownEvent[kind] {
			kind = "unknown" // Never store text the app made up.
		}
		_, err = tx.Exec(ctx, `INSERT INTO rider_events (rider_id, event_id, type, accepted, code, message, device_time) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			actor.ID, e.ID, kind, result.accepted, codeValue, msgValue, deviceValue)
		return err
	})
	return result, err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// effect applies one event's action. Refusals the contract names become rejections.
func (s *Service) effect(ctx context.Context, tx pgx.Tx, rider string, e queued, device time.Time) (outcome, error) {
	bad := rejected("invalid_event", "This event's payload is not valid.", false)
	switch e.Type {
	case "location":
		var raw struct {
			Lat      *float64 `json:"lat"`
			Lng      *float64 `json:"lng"`
			Speed    *float64 `json:"speed_mps"`
			Accuracy *float64 `json:"accuracy_m"`
		}
		if json.Unmarshal(e.Payload, &raw) != nil || raw.Lat == nil || raw.Lng == nil || raw.Speed == nil || raw.Accuracy == nil ||
			math.Abs(*raw.Lat) > 90 || math.Abs(*raw.Lng) > 180 || *raw.Speed < 0 || *raw.Accuracy < 0 {
			return bad, nil
		}
		_, err := s.pingTx(ctx, tx, rider, ping{lat: *raw.Lat, lng: *raw.Lng, speed: *raw.Speed, accuracy: *raw.Accuracy, device: device})
		return outcome{accepted: true}, err
	case "handover":
		var raw struct {
			OrderID    string `json:"order_id"`
			OTP        string `json:"otp"`
			Quantities []struct {
				ItemID string `json:"item_id"`
				Qty    *int   `json:"qty"`
			} `json:"quantities"`
		}
		if json.Unmarshal(e.Payload, &raw) != nil || !idPattern.MatchString(raw.OrderID) || !otpPattern.MatchString(raw.OTP) ||
			len(raw.Quantities) < 1 || len(raw.Quantities) > maxLines {
			return bad, nil
		}
		handed := map[string]int{}
		for _, q := range raw.Quantities {
			if !idPattern.MatchString(q.ItemID) || q.Qty == nil || *q.Qty < 0 {
				return bad, nil
			}
			if _, dup := handed[q.ItemID]; dup {
				return bad, nil
			}
			handed[q.ItemID] = *q.Qty
		}
		// device_time is when the rider entered the code, so an old queued handover is judged as old.
		_, wrong, err := s.handOverTx(ctx, tx, rider, raw.OrderID, raw.OTP, handed, device, false)
		switch {
		case wrong != "":
			return rejected(wrong, "That code is not right.", false), nil
		case errors.Is(err, errInvalid):
			return bad, nil
		case errors.Is(err, errNotFound):
			return rejected("not_found", "That order is not one of yours.", false), nil
		}
		return refusalOutcome(err)
	case "problem":
		var raw struct {
			Type    string `json:"problem_type"`
			OrderID string `json:"order_id"`
			Note    string `json:"note"`
		}
		if json.Unmarshal(e.Payload, &raw) != nil || !problemTypes[raw.Type] || utf8.RuneCountInString(raw.Note) > 1000 ||
			(raw.OrderID != "" && !idPattern.MatchString(raw.OrderID)) {
			return bad, nil
		}
		_, err := s.problemTx(ctx, tx, rider, raw.Type, raw.OrderID, raw.Note, false)
		return refusalOutcome(err)
	case "stock_scan":
		var raw struct {
			LotCode string `json:"lot_code"`
		}
		code := ""
		if json.Unmarshal(e.Payload, &raw) == nil {
			code = strings.TrimSpace(raw.LotCode)
		}
		if code == "" || len(code) > 64 {
			return bad, nil
		}
		_, err := shifts.ScanTx(ctx, tx, rider, code, false)
		if c, m, ok := shifts.RefusalOf(err); ok {
			// With no shift yet, the scan may work once the rider starts one.
			return rejected(c, m, c == "no_open_shift"), nil
		}
		return outcome{accepted: err == nil}, err
	case "restock_confirm", "restock_decline":
		var raw struct {
			RequestID string         `json:"request_id"`
			Reason    string         `json:"reason"`
			Override  []restock.Line `json:"quantity_override"`
		}
		if json.Unmarshal(e.Payload, &raw) != nil || !idPattern.MatchString(raw.RequestID) || utf8.RuneCountInString(raw.Reason) > 200 {
			return bad, nil
		}
		svc := &restock.Service{Pool: s.Pool, Log: s.Log}
		var err error
		if e.Type == "restock_confirm" {
			_, err = svc.ConfirmTx(ctx, tx, rider, raw.RequestID, raw.Override, false)
		} else {
			_, err = svc.DeclineTx(ctx, tx, rider, raw.RequestID, raw.Reason, false)
		}
		if c, m, ok := restock.RefusalOf(err); ok {
			return rejected(c, m, false), nil
		}
		if errors.Is(err, restock.ErrNotFound) {
			return rejected("not_found", "That restock is not one of yours.", false), nil
		}
		if errors.Is(err, restock.ErrInvalid) {
			return bad, nil
		}
		return outcome{accepted: err == nil}, err
	}
	return rejected("unknown_event_type", "This event type is not known.", false), nil
}

// refusalOutcome turns a contract refusal into a final rejection, and passes other errors on.
func refusalOutcome(err error) (outcome, error) {
	var no *refusal
	if errors.As(err, &no) {
		return rejected(no.code, no.message, false), nil
	}
	return outcome{accepted: err == nil}, err
}
