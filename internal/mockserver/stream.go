package mockserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

const (
	closeUnauthorized websocket.StatusCode = 4401
	closeOrderEnded   websocket.StatusCode = 4410
	authWait                               = 5 * time.Second
)

// streamEvent is one replayed event. Events with no seq are passing updates.
type streamEvent struct {
	kind string
	seq  int
	data any
}

// customerTimeline replays an order's stages for a scenario, as the customer would see them.
func customerTimeline(scenario string, now time.Time) (events []streamEvent, ended bool) {
	stages := []string{"looking_for_rider", "rider_accepted", "timer_running", "arrived", "handed_over"}
	switch scenario {
	case "no_scooter", "offer_timeout":
		stages = []string{"looking_for_rider", "no_scooter"}
	case "payment_declined":
		stages = []string{"payment", "payment_failed"}
	case "rider_inside_perimeter":
		stages = []string{"looking_for_rider", "rider_accepted", "arrived", "handed_over"}
	case "cancelled", "timed_out", "failed":
		stages = []string{"looking_for_rider", "ended"}
	}
	for i, stage := range stages {
		order := customerOrder(stage, i+1, now)
		if scenario == "rider_inside_perimeter" && stage == "arrived" {
			order["timer"] = map[string]any{"timer_started_at": nil, "timer_duration_s": nil, "timer_stopped_at": nil, "timer_missed": false}
		}
		if stage == "ended" && scenario != "cancelled" {
			order["ended_reason"] = map[string]string{"timed_out": "timed_out", "failed": "failed"}[scenario]
		}
		events = append(events, streamEvent{kind: "order_update", seq: i + 1, data: order})
		if stage == "rider_accepted" || stage == "timer_running" || stage == "arrived" {
			events = append(events, streamEvent{kind: "rider_position", data: map[string]any{"lat": 12.9716, "lng": 77.5946, "recorded_at": stamp(now)}})
		}
	}
	last := stages[len(stages)-1]
	return events, last == "handed_over" || last == "ended"
}

func riderTimeline(now time.Time) []streamEvent {
	states := []string{"RIDER_ACCEPTED", "PACKED", "TIMER_RUNNING", "ARRIVED", "HANDED_OVER"}
	events := []streamEvent{{kind: "offer", seq: 1, data: sampleOffer("open", now)}}
	for i, state := range states {
		events = append(events, streamEvent{kind: "order_state", seq: i + 2, data: riderOrder(state, i+2, now)})
	}
	return append(events, streamEvent{kind: "restock", seq: len(states) + 2, data: sampleRestock("ready", now, false)})
}

func (s *Server) customerStream(w http.ResponseWriter, r *http.Request) {
	scenario := scenarioOf(r)
	events, ended := customerTimeline(scenario, s.now())
	s.serveStream(w, r, scenario, events, ended)
}

func (s *Server) riderStream(w http.ResponseWriter, r *http.Request) {
	s.serveStream(w, r, scenarioOf(r), riderTimeline(s.now()), false)
}

func scenarioOf(r *http.Request) string {
	if v := r.URL.Query().Get("scenario"); v != "" {
		return v
	}
	return r.Header.Get(scenarioHeader)
}

// serveStream follows the Live streams rules in contracts/README.md.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, scenario string, events []streamEvent, ended bool) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow() //nolint:errcheck
	ctx := r.Context()

	authCtx, cancel := context.WithTimeout(ctx, authWait)
	defer cancel()
	_, raw, err := conn.Read(authCtx)
	if err != nil {
		_ = conn.Close(closeUnauthorized, "no auth frame in time")
		return
	}
	var frame struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Type != "auth" || frame.Token == "" || frame.Token == ExpiredToken {
		_ = conn.Close(closeUnauthorized, "bad or expired token")
		return
	}
	if !s.send(ctx, conn, streamEvent{kind: "auth_ok", data: map[string]any{}}) {
		return
	}

	// Keep reading, so a renewed auth frame is accepted and a closed client is noticed.
	readCtx := conn.CloseRead(ctx)

	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()
	step := time.NewTicker(s.stepFor(r))
	defer step.Stop()
	next, seq, sentState := 0, 0, 0
	for {
		select {
		case <-readCtx.Done():
			return
		case <-beat.C:
			current := seq
			if current == 0 {
				current = 1
			}
			if !s.send(ctx, conn, streamEvent{kind: "heartbeat", seq: current, data: map[string]any{}}) {
				return
			}
		case <-step.C:
			if next >= len(events) {
				if ended {
					_ = conn.Close(closeOrderEnded, "the order has ended")
					return
				}
				continue
			}
			event := events[next]
			next++
			if event.seq > 0 {
				seq = event.seq
				sentState++
			}
			if !s.send(ctx, conn, event) {
				return
			}
			if scenario == "socket_drop" && sentState == 2 {
				_ = conn.CloseNow() // A drop with no close frame, as a real network does.
				return
			}
		}
	}
}

func (s *Server) stepFor(r *http.Request) time.Duration {
	if v := r.URL.Query().Get("step_ms"); v != "" {
		if d, err := time.ParseDuration(v + "ms"); err == nil && d > 0 {
			return d
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
