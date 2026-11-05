package mockserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
)

const (
	closeUnauthorized websocket.StatusCode = 4401
	closeForbidden    websocket.StatusCode = 4403
	closeNotFound     websocket.StatusCode = 4404
	closeOrderEnded   websocket.StatusCode = 4410
	authWait                               = 5 * time.Second
	minStep                                = 50 * time.Millisecond
)

// streamEvent is one replayed event. Events with no seq are passing updates.
type streamEvent struct {
	kind string
	seq  int
	data any
}

// customerEvents replays a scenario's story. ended means the stream closes afterwards.
func customerEvents(scenario string, now time.Time, step time.Duration) (events []streamEvent, ended bool) {
	story := storyFor(scenario)
	orders := buildStory(story, now.Add(-story.age), step)
	for i, order := range orders {
		events = append(events, streamEvent{kind: "order_update", seq: order["seq"].(int), data: order})
		switch story.steps[i].stage {
		case "rider_accepted", "timer_running", "arrived":
			events = append(events, streamEvent{kind: "rider_position", data: map[string]any{"lat": 12.9716, "lng": 77.5946, "recorded_at": stamp(now)}})
		}
	}
	last := story.steps[len(story.steps)-1].stage
	return events, last == "handed_over" || last == "ended"
}

func (s *Server) customerStream(w http.ResponseWriter, r *http.Request) {
	scenario := scenarioOf(r)
	events, ended := customerEvents(scenario, s.now(), s.stepFor(r))
	refuse := map[string]websocket.StatusCode{"ord_missing": closeNotFound, "ord_forbidden": closeForbidden}[chi.URLParam(r, "order_id")]
	s.serveStream(w, r, scenario, events, ended, refuse)
}

func (s *Server) riderStream(w http.ResponseWriter, r *http.Request) {
	scenario := scenarioOf(r)
	s.serveStream(w, r, scenario, riderStoryFor(scenario, s.now()).events, false, 0)
}

func scenarioOf(r *http.Request) string {
	if v := r.URL.Query().Get("scenario"); v != "" {
		return v
	}
	return r.Header.Get(scenarioHeader)
}

type streamFrame struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// serveStream follows the Live streams rules in contracts/README.md.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, scenario string, events []streamEvent, ended bool, refuse websocket.StatusCode) {
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	// The mock has no real data, and the tokens are samples, so any web origin may connect.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow() //nolint:errcheck
	conn.SetReadLimit(64 * 1024)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// One reader handles every frame: the first auth, and renewed tokens after it.
	frames := make(chan streamFrame, 1)
	go func() {
		defer cancel()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var frame streamFrame
			if json.Unmarshal(raw, &frame) != nil {
				frame = streamFrame{}
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	failOnce := false
	authTimer := time.NewTimer(authWait)
	defer authTimer.Stop()
	select {
	case <-authTimer.C:
		_ = conn.Close(closeUnauthorized, "no auth frame in time")
		return
	case <-ctx.Done():
		return
	case frame := <-frames:
		if !validFrame(frame) {
			_ = conn.Close(closeUnauthorized, "bad or expired token")
			return
		}
		failOnce = (scenario == "socket_drop" || scenario == "token_expiring") && s.firstTime(scenario+"|"+frame.Token, s.now())
	}
	if refuse != 0 {
		_ = conn.Close(refuse, "this stream is not available")
		return
	}
	if !s.send(ctx, conn, streamEvent{kind: "auth_ok", data: map[string]any{}}) {
		return
	}

	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()
	step := time.NewTicker(s.stepFor(r))
	defer step.Stop()
	life := time.NewTimer(maxStreamLife)
	defer life.Stop()
	next, seq, sentState := 0, 1, 0
	renewed := false
	// sendNext sends the next event, and reports whether the stream should stop.
	sendNext := func() bool {
		if next >= len(events) {
			if ended {
				_ = conn.Close(closeOrderEnded, "the order has ended")
				return true
			}
			return false
		}
		event := events[next]
		next++
		if event.seq > 0 {
			seq = event.seq
			sentState++
		}
		if !s.send(ctx, conn, event) {
			return true
		}
		if failOnce && scenario == "socket_drop" && sentState == 2 {
			_ = conn.CloseNow() // A drop with no close frame, as a real network does.
			return true
		}
		if failOnce && scenario == "token_expiring" && sentState == 2 && !renewed {
			_ = conn.Close(closeUnauthorized, "the token has expired")
			return true
		}
		return false
	}
	// The first event goes out at once, so its times are true when it arrives.
	if sendNext() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			_ = conn.Close(websocket.StatusTryAgainLater, "the mock limits how long a stream stays open")
			return
		case frame := <-frames:
			if !validFrame(frame) {
				_ = conn.Close(closeUnauthorized, "bad or expired token")
				return
			}
			renewed = true
		case <-beat.C:
			if !s.send(ctx, conn, streamEvent{kind: "heartbeat", seq: seq, data: map[string]any{}}) {
				return
			}
		case <-step.C:
			if sendNext() {
				return
			}
		}
	}
}

func validFrame(f streamFrame) bool {
	return f.Type == "auth" && f.Token != "" && f.Token != ExpiredToken
}

func (s *Server) stepFor(r *http.Request) time.Duration {
	if v := r.URL.Query().Get("step_ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			if d := time.Duration(ms) * time.Millisecond; d >= minStep {
				return d
			}
			return minStep
		}
	}
	return s.step
}

func (s *Server) send(ctx context.Context, conn *websocket.Conn, e streamEvent) bool {
	body := map[string]any{"type": e.kind, "server_time": stamp(s.now()), "data": e.data}
	if e.seq > 0 {
		body["seq"] = e.seq
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw) == nil
}
