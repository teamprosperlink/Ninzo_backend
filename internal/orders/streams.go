package orders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// Close codes, from contracts/README.md, Live streams.
const (
	closeUnauthorized websocket.StatusCode = 4401
	closeForbidden    websocket.StatusCode = 4403
	closeNotFound     websocket.StatusCode = 4404
	closeOrderEnded   websocket.StatusCode = 4410
)

// Streams are how the apps follow an order live. Each one reads the seq that every change already
// saves, so a stream never decides anything: GET stays the authority. Polling the database keeps
// this simple at pilot size. A notification channel can replace it without changing the apps.
type Streams struct {
	Signer    identity.Signer
	AuthWait  time.Duration // For the first auth frame. 5 seconds.
	Poll      time.Duration // How often a stream looks for a change. 1 second.
	Heartbeat time.Duration // 15 seconds.
	Position  time.Duration // How often the rider's position goes to the customer. 3 seconds.
	MaxLife   time.Duration // A stream closes after this, and the app reconnects. 30 minutes.
	MaxOpen   int           // At most this many streams at once on one API copy. 200.
	open      chan struct{}
}

func (st *Streams) defaults() {
	if st.AuthWait == 0 {
		st.AuthWait = 5 * time.Second
	}
	if st.Poll == 0 {
		st.Poll = time.Second
	}
	if st.Heartbeat == 0 {
		st.Heartbeat = 15 * time.Second
	}
	if st.Position == 0 {
		st.Position = 3 * time.Second
	}
	if st.MaxLife == 0 {
		st.MaxLife = 30 * time.Minute
	}
	if st.MaxOpen == 0 {
		st.MaxOpen = 200
	}
	if st.open == nil {
		st.open = make(chan struct{}, st.MaxOpen)
	}
}

// MountCustomerStream adds the customer's order stream. It signs in with a frame, not a header.
func (s *Service) MountCustomerStream(r chi.Router, st *Streams) {
	st.defaults()
	r.Get("/orders/{order_id}/stream", func(w http.ResponseWriter, r *http.Request) { s.customerStream(w, r, st) })
}

// MountRiderStream adds the rider's stream of offers and orders.
func (s *Service) MountRiderStream(r chi.Router, st *Streams) {
	st.defaults()
	r.Get("/riders/me/stream", func(w http.ResponseWriter, r *http.Request) { s.riderStream(w, r, st) })
}

type frame struct {
	kind string
	seq  int64
	data any
}

// watcher is what a stream follows: its check after sign-in, and what changed since last time.
type watcher struct {
	actorType string
	// admit returns a close code if this actor may not follow this stream.
	admit func(ctx context.Context, a identity.Actor) (websocket.StatusCode, error)
	// seq returns the current seq.
	seq func(ctx context.Context) (int64, error)
	// changed returns the frames for a new seq, and a close code if the stream should end after them.
	changed func(ctx context.Context, seq int64) ([]frame, websocket.StatusCode, error)
	// initial returns the current state, sent right after auth_ok.
	initial func(ctx context.Context, seq int64) ([]frame, error)
	// passing returns an update with no seq, such as the rider's position, if there is a new one.
	passing func(ctx context.Context) (*frame, error)
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request, st *Streams, wt watcher) {
	// Sign-in is by a token frame, never a cookie, so a page on another site gains nothing by
	// connecting. Any origin may connect, as on the mock, so a web build of the apps works.
	if err := httpserver.ForStream(w); err != nil {
		s.Log.Error("orders: a stream could not clear the server's deadlines", "error", err.Error())
		http.Error(w, "streams are not available", http.StatusInternalServerError)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow() //nolint:errcheck
	conn.SetReadLimit(16 * 1024)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	type authFrame struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	frames := make(chan authFrame, 1)
	go func() {
		defer cancel()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f authFrame
			if json.Unmarshal(raw, &f) != nil {
				f = authFrame{}
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	verify := func(f authFrame) (identity.Actor, time.Time, bool) {
		if f.Type != "auth" || f.Token == "" {
			return identity.Actor{}, time.Time{}, false
		}
		a, exp, err := st.Signer.VerifyUntil(f.Token, wt.actorType)
		return a, exp, err == nil
	}
	var actor identity.Actor
	var expires time.Time
	wait := time.NewTimer(st.AuthWait)
	defer wait.Stop()
	select {
	case <-wait.C:
		_ = conn.Close(closeUnauthorized, "no auth frame in time")
		return
	case <-ctx.Done():
		return
	case f := <-frames:
		var ok bool
		if actor, expires, ok = verify(f); !ok {
			_ = conn.Close(closeUnauthorized, "bad or expired token")
			return
		}
	}
	// Only a signed-in stream takes one of the open slots.
	select {
	case st.open <- struct{}{}:
		defer func() { <-st.open }()
	default:
		_ = conn.Close(websocket.StatusTryAgainLater, "too many streams")
		return
	}
	live, err := identity.SessionLive(ctx, s.Pool, actor)
	if err != nil || !live {
		_ = conn.Close(closeUnauthorized, "the session has ended")
		return
	}
	if code, err := wt.admit(ctx, actor); err != nil || code != 0 {
		if err != nil {
			s.Log.Error("orders: stream check failed", "error", err.Error())
			code = websocket.StatusInternalError
		}
		_ = conn.Close(code, "this stream is not available")
		return
	}
	seq, err := wt.seq(ctx)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "try again")
		return
	}
	if !send(ctx, conn, frame{kind: "auth_ok", data: map[string]any{}}) {
		return
	}
	if wt.initial != nil {
		out, err := wt.initial(ctx, seq)
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "try again")
			return
		}
		for _, f := range out {
			if !send(ctx, conn, f) {
				return
			}
		}
	}
	expiry := time.NewTimer(time.Until(expires))
	defer expiry.Stop()
	poll := time.NewTicker(st.Poll)
	defer poll.Stop()
	beat := time.NewTicker(st.Heartbeat)
	defer beat.Stop()
	position := time.NewTicker(st.Position)
	defer position.Stop()
	life := time.NewTimer(st.MaxLife)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			_ = conn.Close(websocket.StatusTryAgainLater, "reconnect")
			return
		case <-expiry.C:
			_ = conn.Close(closeUnauthorized, "the token has expired")
			return
		case f := <-frames:
			// A renewed token must be for the same person and session.
			next, exp, ok := verify(f)
			if !ok || next.ID != actor.ID || next.ChainID != actor.ChainID {
				_ = conn.Close(closeUnauthorized, "bad or expired token")
				return
			}
			expiry.Reset(time.Until(exp))
		case <-beat.C:
			if live, err := identity.SessionLive(ctx, s.Pool, actor); err == nil && !live {
				_ = conn.Close(closeUnauthorized, "the session has ended")
				return
			}
			if !send(ctx, conn, frame{kind: "heartbeat", seq: max(seq, 1), data: map[string]any{}}) {
				return
			}
		case <-position.C:
			if wt.passing == nil {
				continue
			}
			f, err := wt.passing(ctx)
			if err == nil && f != nil && !send(ctx, conn, *f) {
				return
			}
		case <-poll.C:
			now, err := wt.seq(ctx)
			if err != nil || now <= seq {
				continue
			}
			seq = now
			out, code, err := wt.changed(ctx, now)
			if err != nil {
				s.Log.Error("orders: stream read failed", "error", err.Error())
				continue
			}
			for _, f := range out {
				if !send(ctx, conn, f) {
					return
				}
			}
			if code != 0 {
				_ = conn.Close(code, "the order has ended")
				return
			}
		}
	}
}

// over says the order's story is finished, so its stream closes with 4410, as the mock does.
func over(stage string) bool { return stage == "ended" || stage == "handed_over" }

func send(ctx context.Context, conn *websocket.Conn, f frame) bool {
	body := map[string]any{"type": f.kind, "server_time": serverTime(), "data": f.data}
	if f.seq > 0 {
		body["seq"] = f.seq
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw) == nil
}

func (s *Service) customerStream(w http.ResponseWriter, r *http.Request, st *Streams) {
	id := chi.URLParam(r, "order_id")
	var lastPosition time.Time
	s.serve(w, r, st, watcher{
		actorType: identity.TypeCustomer,
		admit: func(ctx context.Context, a identity.Actor) (websocket.StatusCode, error) {
			if _, err := customer(ctx, s.Pool, a.ID); errors.Is(err, errBlocked) {
				return closeForbidden, nil
			} else if err != nil {
				return 0, err
			}
			if !idPattern.MatchString(id) {
				return closeNotFound, nil
			}
			var owner string
			err := s.Pool.QueryRow(ctx, "SELECT customer_id::text FROM orders WHERE id = $1", id).Scan(&owner)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return closeNotFound, nil
			case err != nil:
				return 0, err
			case owner != a.ID:
				return closeNotFound, nil // As GET does, so a stream never shows an order exists.
			}
			var state string
			if err := s.Pool.QueryRow(ctx, "SELECT state FROM orders WHERE id = $1", id).Scan(&state); err != nil {
				return 0, err
			}
			if over(Stage(state)) {
				return closeOrderEnded, nil
			}
			return 0, nil
		},
		seq: func(ctx context.Context) (int64, error) {
			var seq int64
			err := s.Pool.QueryRow(ctx, "SELECT seq FROM orders WHERE id = $1", id).Scan(&seq)
			return seq, err
		},
		changed: func(ctx context.Context, seq int64) ([]frame, websocket.StatusCode, error) {
			order, err := view(ctx, s.Pool, s, id, s.clock())
			if err != nil {
				return nil, 0, err
			}
			// What GET returns now carries its own seq, which is never lower than the one seen.
			if v, ok := order["seq"].(int64); ok && v > seq {
				seq = v
			}
			out := []frame{{kind: "order_update", seq: seq, data: order}}
			if stage, _ := order["customer_stage"].(string); over(stage) {
				return out, closeOrderEnded, nil
			}
			return out, 0, nil
		},
		initial: func(ctx context.Context, seq int64) ([]frame, error) {
			order, err := view(ctx, s.Pool, s, id, s.clock())
			if err != nil {
				return nil, err
			}
			return []frame{{kind: "order_update", seq: max(seq, 1), data: order}}, nil
		},
		passing: func(ctx context.Context) (*frame, error) {
			order, err := view(ctx, s.Pool, s, id, s.clock())
			if err != nil {
				return nil, err
			}
			// The rider's position goes out only once the customer can see the rider.
			switch order["customer_stage"] {
			case "rider_accepted", "timer_running", "arrived":
			default:
				return nil, nil
			}
			var lat, lng float64
			var at time.Time
			err = s.Pool.QueryRow(ctx, `SELECT lat, lng, received_at FROM location_pings WHERE order_id = $1 AND applied
				ORDER BY id DESC LIMIT 1`, id).Scan(&lat, &lng, &at)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !at.After(lastPosition)) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			lastPosition = at
			return &frame{kind: "rider_position", data: map[string]any{"lat": lat, "lng": lng, "recorded_at": at.UTC().Format(time.RFC3339Nano)}}, nil
		},
	})
}

func (s *Service) riderStream(w http.ResponseWriter, r *http.Request, st *Streams) {
	var rider string
	var lastChange time.Time
	// latest is the offer that changed last, and when.
	latest := func(ctx context.Context) (string, time.Time, error) {
		var id string
		var at time.Time
		err := s.Pool.QueryRow(ctx, "SELECT id::text, updated_at FROM offers WHERE rider_id = $1 ORDER BY updated_at DESC, id DESC LIMIT 1", rider).Scan(&id, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", time.Time{}, nil
		}
		return id, at, err
	}
	event := func(ctx context.Context, offer string, seq int64) ([]frame, error) {
		f, err := readOffer(ctx, s.Pool, offer)
		if err != nil {
			return nil, err
		}
		if f.state == "accepted" {
			v, err := riderOrderView(ctx, s.Pool, f)
			return []frame{{kind: "order_state", seq: max(seq, 1), data: v}}, err
		}
		v, err := offerView(ctx, s.Pool, f, s.clock())
		return []frame{{kind: "offer", seq: max(seq, 1), data: v}}, err
	}
	s.serve(w, r, st, watcher{
		actorType: identity.TypeRider,
		admit: func(ctx context.Context, a identity.Actor) (websocket.StatusCode, error) {
			rider = a.ID
			if err := riderOK(ctx, s.Pool, a.ID); errors.Is(err, errBlocked) {
				return closeForbidden, nil
			} else if err != nil {
				return 0, err
			}
			return 0, nil
		},
		// The raw seq: a rider with nothing saved reads 0 here, so their first change, saved as 1,
		// is seen as new. Frames never carry less than 1.
		seq: func(ctx context.Context) (int64, error) {
			var seq int64
			err := s.Pool.QueryRow(ctx, "SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)", rider).Scan(&seq)
			return seq, err
		},
		// An event only when an offer or order changed. A shift or stock change sends nothing, so
		// the next heartbeat's higher seq makes the app fetch its state, as the README says.
		changed: func(ctx context.Context, seq int64) ([]frame, websocket.StatusCode, error) {
			offer, at, err := latest(ctx)
			if err != nil || offer == "" || !at.After(lastChange) {
				return nil, 0, err
			}
			lastChange = at
			out, err := event(ctx, offer, seq)
			return out, 0, err
		},
		// A live offer or order is sent at once. An old one is not.
		initial: func(ctx context.Context, seq int64) ([]frame, error) {
			offer, at, err := latest(ctx)
			if err != nil || offer == "" {
				return nil, err
			}
			lastChange = at
			var released *time.Time
			if err := s.Pool.QueryRow(ctx, "SELECT released_at FROM offers WHERE id = $1", offer).Scan(&released); err != nil || released != nil {
				return nil, err
			}
			return event(ctx, offer, seq)
		},
	})
}
