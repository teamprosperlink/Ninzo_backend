package orders

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/earnings"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

const (
	// A ping whose device time is this far behind the server is evidence only, so a flush of old
	// pings can never start and stop a timer at once (contracts/README.md, Rider offline events).
	lateLimit = 30 * time.Second
	// Faster than this between two pings is a GPS jump, and is dropped (data_flow.md).
	maxPlausibleMps = 40.0
	// Worse accuracy than this cannot place a rider inside a 100 m perimeter.
	maxAccuracyM = 100.0
	// A ping this recent is fresh GPS, for Ready and for handover.
	freshPing = 30 * time.Second
	// Faster than this, a rider is moving and cannot press Ready.
	movingMps = 2.0
	// Wrong handover codes allowed before the code is locked.
	maxOTPAttempts = 5
)

var otpPattern = regexp.MustCompile(`^[0-9]{4,8}$`)

// MountTracking adds locationPing, with its own rate limit, because pings come every few seconds.
func (s *Service) MountTracking(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Post("/riders/me/location", s.ping)
	})
}

// metres between two points, on a sphere. Good to well under a metre at these distances.
func metres(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lng2-lng1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}

func newOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

type ping struct {
	lat, lng, speed, accuracy float64
	device                    time.Time
}

// nextPing is how soon the app should ping again: fast from packed to arrived, slower otherwise.
func nextPing(state string) int {
	switch state {
	case "PACKED", "TIMER_RUNNING", "ARRIVED":
		return 3
	case "RIDER_ACCEPTED":
		return 5
	case "":
		return 15
	}
	return 15
}

func (s *Service) ping(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Lat        *float64 `json:"lat"`
		Lng        *float64 `json:"lng"`
		Speed      *float64 `json:"speed_mps"`
		Accuracy   *float64 `json:"accuracy_m"`
		DeviceTime string   `json:"device_time"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	device, terr := time.Parse(time.RFC3339Nano, body.DeviceTime)
	if err != nil || terr != nil || body.Lat == nil || body.Lng == nil || body.Speed == nil || body.Accuracy == nil ||
		math.Abs(*body.Lat) > 90 || math.Abs(*body.Lng) > 180 || *body.Speed < 0 || *body.Accuracy < 0 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send lat, lng, speed_mps, accuracy_m and device_time.", false)
		return
	}
	// The database keeps microseconds, so a repeated device time must compare equal.
	p := ping{lat: *body.Lat, lng: *body.Lng, speed: *body.Speed, accuracy: *body.Accuracy, device: device.Truncate(time.Microsecond)}
	next := 15
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := riderOK(ctx, tx, actor.ID); err != nil {
			return err
		}
		var err error
		next, err = s.pingTx(ctx, tx, actor.ID, p)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusAccepted, map[string]any{"next_ping_s": next, "server_time": serverTime()})
}

// pingTx applies one ping for a rider and keeps it: the core of locationPing, shared with the
// offline batch. It returns how soon to ping again.
func (s *Service) pingTx(ctx context.Context, tx pgx.Tx, riderID string, p ping) (next int, err error) {
	next = 15
	err = func() error {
		var order any
		applied, note := false, "no order"
		var id string
		err := tx.QueryRow(ctx, "SELECT order_id::text FROM offers WHERE rider_id = $1 AND released_at IS NULL AND state = 'accepted'", riderID).Scan(&id)
		switch {
		case err == nil:
			if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", id); err != nil {
				return err
			}
			order = id
			// The clock is read under the order's lock, so a timer edge is decided under it.
			var state string
			if applied, note, state, err = track(ctx, tx, id, p, s.clock()); err != nil {
				return err
			}
			next = nextPing(state)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var noteValue any
		if note != "" {
			noteValue = note
		}
		_, err = tx.Exec(ctx, `INSERT INTO location_pings (rider_id, order_id, lat, lng, speed_mps, accuracy_m, device_time, received_at, applied, note)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, riderID, order, p.lat, p.lng, p.speed, p.accuracy, p.device, s.clock(), applied, noteValue)
		return err
	}()
	return next, err
}

// track applies one ping to a locked order. Two pings in a row inside the zone, once the order is
// packed and the minimum wait has passed, start the timer. Two inside the arrival perimeter stop
// it. A packed rider already inside the perimeter gets no timer. It returns whether the ping was
// applied, why not, and the order's state after it.
func track(ctx context.Context, tx pgx.Tx, orderID string, p ping, now time.Time) (bool, string, string, error) {
	var state string
	var gateLat, gateLng float64
	var zone, perimeter *int
	var zoneHits, perimeterHits int
	var minWait, lastDevice *time.Time
	err := tx.QueryRow(ctx, `SELECT o.state, h.lat, h.lng, o.zone_radius_m, o.arrival_perimeter_m, o.zone_hits, o.perimeter_hits,
			o.min_wait_until, o.last_device_time
		FROM orders o JOIN gate_history h ON h.id = o.gate_history_id WHERE o.id = $1`, orderID).
		Scan(&state, &gateLat, &gateLng, &zone, &perimeter, &zoneHits, &perimeterHits, &minWait, &lastDevice)
	if err != nil {
		return false, "", "", err
	}
	if state != "RIDER_ACCEPTED" && state != "PACKED" && state != "TIMER_RUNNING" && state != "ARRIVED" {
		return false, "order not on its way", state, nil
	}
	// Any ping shows the signal is alive, even one too poor to place the rider, so the code is
	// never released while the rider's phone is still reaching us.
	if _, err := tx.Exec(ctx, "UPDATE orders SET last_ping_at = $2 WHERE id = $1", orderID, now); err != nil {
		return false, "", "", err
	}
	switch {
	case zone == nil || perimeter == nil:
		return false, "order has no zone", state, nil
	case now.Sub(p.device) > lateLimit:
		// Too late to move the timer, but it may show the rider was at the gate in time.
		return false, "late", state, reviewLateArrival(ctx, tx, orderID, p, now)
	case p.device.Sub(now) > lateLimit:
		// A phone clock running fast would otherwise make every true ping look older.
		return false, "from the future", state, nil
	}
	if _, err := tx.Exec(ctx, "UPDATE orders SET last_live_ping_at = $2 WHERE id = $1", orderID, now); err != nil {
		return false, "", "", err
	}
	switch {
	case lastDevice != nil && !p.device.After(*lastDevice):
		return false, "older than one applied", state, nil
	case p.accuracy > maxAccuracyM:
		return false, "inaccurate", state, nil
	}
	var lastLat, lastLng float64
	var lastAt time.Time
	// Only this rider's pings: after a rematch, the last rider's position says nothing.
	err = tx.QueryRow(ctx, `SELECT lat, lng, device_time FROM location_pings WHERE order_id = $1 AND applied
		AND rider_id = (SELECT rider_id FROM orders WHERE id = $1) ORDER BY id DESC LIMIT 1`, orderID).
		Scan(&lastLat, &lastLng, &lastAt)
	if err == nil {
		if dt := p.device.Sub(lastAt).Seconds(); dt > 0 && metres(lastLat, lastLng, p.lat, p.lng)/dt > maxPlausibleMps {
			return false, "implausible jump", state, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", "", err
	}
	away := metres(p.lat, p.lng, gateLat, gateLng)
	if away <= float64(*zone) {
		zoneHits++
	} else {
		zoneHits = 0
	}
	if away <= float64(*perimeter) {
		perimeterHits++
	} else {
		perimeterHits = 0
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET zone_hits = $2, perimeter_hits = $3, last_device_time = $4, updated_at = now()
		WHERE id = $1`, orderID, zoneHits, perimeterHits, p.device); err != nil {
		return false, "", "", err
	}
	waited := minWait != nil && !now.Before(*minWait)
	switch {
	case (state == "PACKED" || state == "TIMER_RUNNING") && perimeterHits >= 2:
		if err := arrive(ctx, tx, orderID, state, "gps", now); err != nil {
			return false, "", "", err
		}
		state = "ARRIVED"
	case state == "PACKED" && zoneHits >= 2 && waited:
		tag, err := tx.Exec(ctx, `UPDATE orders SET state = 'TIMER_RUNNING', timer_started_at = $2, seq = seq + 1, updated_at = now()
			WHERE id = $1 AND timer_started_at IS NULL`, orderID, now)
		if err != nil {
			return false, "", "", err
		}
		if tag.RowsAffected() == 1 {
			if err := event(ctx, tx, orderID, "PACKED", "TIMER_RUNNING", "system", nil, "inside the zone"); err != nil {
				return false, "", "", err
			}
			if err := bumpRiderOf(ctx, tx, orderID); err != nil {
				return false, "", "", err
			}
			state = "TIMER_RUNNING"
		}
	}
	return true, "", state, nil
}

func bumpRiderOf(ctx context.Context, tx pgx.Tx, orderID string) error {
	if err := touchOffer(ctx, tx, orderID); err != nil {
		return err
	}
	var rider *string
	if err := tx.QueryRow(ctx, "SELECT rider_id::text FROM orders WHERE id = $1", orderID).Scan(&rider); err != nil || rider == nil {
		return err
	}
	return bumpRider(ctx, tx, *rider)
}

// touchOffer marks the live offer changed, so the rider's stream sends the order's new state.
func touchOffer(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, "UPDATE offers SET updated_at = clock_timestamp() WHERE order_id = $1 AND released_at IS NULL", orderID)
	return err
}

// arrive records the rider at the gate, stopping a running timer. The timer was missed if its
// length had passed when it stopped: now, or earlier if an arrival review already stopped it. During the minimum wait the customer still sees looking_for_rider, so seq
// moves only once that wait is over.
func arrive(ctx context.Context, tx pgx.Tx, orderID, from, source string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE orders SET state = 'ARRIVED', arrived_at = $2, arrival_source = $3,
			timer_stopped_at = CASE WHEN timer_started_at IS NOT NULL AND timer_stopped_at IS NULL THEN $2 ELSE timer_stopped_at END,
			timer_missed = timer_missed OR (timer_started_at IS NOT NULL AND coalesce(timer_stopped_at, $2) >= timer_started_at + make_interval(secs => timer_length_s)),
			seq = seq + CASE WHEN coalesce(min_wait_until <= $2, false) THEN 1 ELSE 0 END, updated_at = now()
		WHERE id = $1`, orderID, now, source); err != nil {
		return err
	}
	if err := event(ctx, tx, orderID, from, "ARRIVED", "system", nil, "arrived, by "+source); err != nil {
		return err
	}
	return bumpRiderOf(ctx, tx, orderID)
}

// handStock takes the handed units off the scooter for good. Units not handed over go back to
// being free on the scooter.
func handStock(ctx context.Context, tx pgx.Tx, orderID, rider string, quantities map[string]int) error {
	handed := map[string]int{} // A copy: the caller still needs the quantities.
	for item, qty := range quantities {
		handed[item] = qty
	}
	rows, err := tx.Query(ctx, `UPDATE reservations SET status = 'handed_over' WHERE order_id = $1 AND status = 'held'
		RETURNING scooter_id::text, lot_id::text, item_id::text, qty`, orderID)
	if err != nil {
		return err
	}
	type held struct {
		scooter, lot, item string
		qty                int
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (held, error) {
		var h held
		return h, r.Scan(&h.scooter, &h.lot, &h.item, &h.qty)
	})
	if err != nil {
		return err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].scooter != all[j].scooter {
			return all[i].scooter < all[j].scooter
		}
		return all[i].lot < all[j].lot
	})
	for _, h := range all {
		take := min(h.qty, handed[h.item])
		handed[h.item] -= take
		if _, err := tx.Exec(ctx, "UPDATE scooter_stock SET on_hand = on_hand - $3, reserved = reserved - $4, updated_at = now() WHERE scooter_id = $1 AND lot_id = $2",
			h.scooter, h.lot, take, h.qty); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, delta_reserved, reason, actor_type, actor_id)
			VALUES ($1, $2, $3, $4, 'handover', 'rider', $5)`, h.scooter, h.lot, -take, -h.qty, rider); err != nil {
			return err
		}
	}
	return nil
}

var errInvalid = errors.New("orders: invalid request")

func (s *Service) handOver(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	var body struct {
		OTP        string `json:"otp"`
		ClientTime string `json:"client_time"`
		Quantities []struct {
			ItemID string `json:"item_id"`
			Qty    *int   `json:"qty"`
		} `json:"quantities"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	client, terr := time.Parse(time.RFC3339Nano, body.ClientTime)
	handed := map[string]int{}
	ok := err == nil && terr == nil && otpPattern.MatchString(body.OTP) && len(body.Quantities) >= 1 && len(body.Quantities) <= maxLines
	for _, q := range body.Quantities {
		if !ok || !idPattern.MatchString(q.ItemID) || q.Qty == nil || *q.Qty < 0 {
			ok = false
			break
		}
		if _, dup := handed[q.ItemID]; dup {
			ok = false
			break
		}
		handed[q.ItemID] = *q.Qty
	}
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send otp, client_time and a quantity for each item.", false)
		return
	}
	var reply map[string]any
	wrong := ""
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := riderOK(ctx, tx, actor.ID); err != nil {
			return err
		}
		var err error
		reply, wrong, err = s.handOverTx(ctx, tx, actor.ID, id, body.OTP, handed, client, true)
		if err == nil && wrong != "" {
			// A wrong code is final. Kept under the key, so the queued copy of this entry counts
			// once and does not use up a second attempt (contracts/README.md, Rider offline events).
			if key := r.Header.Get("Idempotency-Key"); eventIDPattern.MatchString(key) {
				_, err = tx.Exec(ctx, `INSERT INTO rider_events (rider_id, event_id, type, accepted, code, message, device_time)
					VALUES ($1, $2, 'handover', false, $3, 'That code is not right.', $4) ON CONFLICT DO NOTHING`, actor.ID, key, wrong, client)
			}
		}
		return err
	})
	switch {
	case errors.Is(err, errInvalid):
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send one quantity for each ordered item, no more than ordered.", false)
	case err != nil:
		s.fail(w, r, err)
	case wrong != "":
		httpserver.WriteError(w, http.StatusConflict, wrong, "That code is not right.", false)
	default:
		httpserver.WriteJSON(w, http.StatusOK, reply)
	}
}

// handOverTx hands over a locked order for a rider: the core of handOver, shared with the offline
// batch. wrong is set, with the attempt committed, when the code is wrong. save stores the reply
// for the direct request's Idempotency-Key.
func (s *Service) handOverTx(ctx context.Context, tx pgx.Tx, riderID, id, otpSent string, handed map[string]int, client time.Time, save bool) (reply map[string]any, wrong string, err error) {
	if !idPattern.MatchString(id) {
		return nil, "", errNotFound
	}
	err = func() error {
		var offer string
		err := tx.QueryRow(ctx, `SELECT id::text FROM offers WHERE order_id = $1 AND rider_id = $2 AND state = 'accepted'
			ORDER BY created_at DESC, id DESC LIMIT 1`, id, riderID).Scan(&offer)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", id); err != nil {
			return err
		}
		now := s.clock()
		f, err := readOffer(ctx, tx, offer)
		if err != nil {
			return err
		}
		var otp *string
		var attempts, perimeter int
		var gateLat, gateLng float64
		if err := tx.QueryRow(ctx, `SELECT o.handover_otp, o.otp_attempts, coalesce(o.arrival_perimeter_m, 100), h.lat, h.lng
			FROM orders o JOIN gate_history h ON h.id = o.gate_history_id WHERE o.id = $1`, id).Scan(&otp, &attempts, &perimeter, &gateLat, &gateLng); err != nil {
			return err
		}
		finish := func() error {
			if f, err = readOffer(ctx, tx, offer); err != nil {
				return err
			}
			view, err := riderOrderView(ctx, tx, f)
			if err != nil {
				return err
			}
			reply = map[string]any{"order": view, "server_time": serverTime()}
			if !save {
				return nil
			}
			return idempotency.Save(ctx, tx, http.StatusOK, reply)
		}
		if f.orderState == "HANDED_OVER" || f.orderState == "COMPLETED" {
			return finish() // A repeat by the same rider after success.
		}
		if f.releasedAt != nil {
			if f.endedReason != nil && *f.endedReason == "cancelled" {
				return &refusal{"order_cancelled", "The customer cancelled this order."}
			}
			return &refusal{"order_reassigned", "This order is no longer yours."}
		}
		// Before packing only once the code was released after a lost signal: the customer has it.
		var released bool
		if err := tx.QueryRow(ctx, "SELECT otp_released_at IS NOT NULL FROM orders WHERE id = $1", id).Scan(&released); err != nil {
			return err
		}
		if f.orderState != "PACKED" && f.orderState != "TIMER_RUNNING" && f.orderState != "ARRIVED" && !(f.orderState == "RIDER_ACCEPTED" && released) {
			return &refusal{"handover_not_allowed", "This order cannot be handed over now."}
		}
		if attempts >= maxOTPAttempts {
			return &refusal{"otp_attempts_exceeded", "Too many wrong codes. Call support."}
		}
		if otp == nil || otpSent != *otp {
			// The count must stick, so this commits, and the 409 is written afterwards.
			_, err := tx.Exec(ctx, "UPDATE orders SET otp_attempts = otp_attempts + 1, updated_at = now() WHERE id = $1", id)
			wrong = "wrong_otp"
			return err
		}
		items, err := basketOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(handed) != len(items) {
			return errInvalid
		}
		for item, qty := range handed {
			if ordered, ok := items[item]; !ok || qty > ordered {
				return errInvalid
			}
		}
		// Fresh GPS decides whether the rider is at the gate. A current handover far from it is
		// refused. An older queued one is accepted and flagged, as the rider may have ridden on.
		var lat, lng float64
		fresh := true
		err = tx.QueryRow(ctx, `SELECT lat, lng FROM location_pings WHERE rider_id = $1 AND applied AND received_at >= $2 ORDER BY id DESC LIMIT 1`,
			riderID, now.Add(-freshPing)).Scan(&lat, &lng)
		if errors.Is(err, pgx.ErrNoRows) {
			fresh = false
		} else if err != nil {
			return err
		}
		current := now.Sub(client) <= freshPing
		far := fresh && metres(lat, lng, gateLat, gateLng) > 2*float64(perimeter)
		if far && current {
			return &refusal{"rider_not_near", "Fresh GPS shows you away from the gate."}
		}
		flag := ""
		switch {
		case far:
			flag = "late_handover_away_from_gate"
		case !fresh:
			flag = "arrival_from_handover"
		}
		if f.orderState != "ARRIVED" {
			source := "gps"
			if !fresh {
				source = "handover"
			}
			if err := arrive(ctx, tx, id, f.orderState, source, now); err != nil {
				return err
			}
		}
		if err := handStock(ctx, tx, id, riderID, handed); err != nil {
			return err
		}
		for item, qty := range handed {
			if _, err := tx.Exec(ctx, "UPDATE order_items SET qty_handed = $3 WHERE order_id = $1 AND item_id = $2", id, item, qty); err != nil {
				return err
			}
		}
		var flagValue any
		if flag != "" {
			flagValue = flag
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET state = 'HANDED_OVER', handed_over_at = $2, handover_otp = NULL, seq = seq + 1,
				audit_flag = coalesce($3, audit_flag), updated_at = now() WHERE id = $1`, id, now, flagValue); err != nil {
			return err
		}
		if err := refundMissingFood(ctx, tx, id); err != nil {
			return err
		}
		if err := event(ctx, tx, id, "ARRIVED", "HANDED_OVER", identity.TypeRider, riderID, "handed over"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE offers SET final_state = 'HANDED_OVER', released_at = now(), updated_at = now() WHERE id = $1", offer); err != nil {
			return err
		}
		if err := bumpRider(ctx, tx, riderID); err != nil {
			return err
		}
		if err := earnings.RecordOrder(ctx, tx, riderID, id, now); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: riderID, Action: "order.handed_over", EntityType: "order", EntityID: id,
			After: map[string]any{"flag": flag}}); err != nil {
			return err
		}
		return finish()
	}()
	return reply, wrong, err
}

// riderMoving says whether the rider's latest fresh ping shows them moving.
func riderMoving(ctx context.Context, tx pgx.Tx, rider string, now time.Time) (bool, error) {
	var speed float64
	err := tx.QueryRow(ctx, "SELECT speed_mps FROM location_pings WHERE rider_id = $1 AND received_at >= $2 ORDER BY id DESC LIMIT 1",
		rider, now.Add(-10*time.Second)).Scan(&speed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return speed > movingMps, err
}
