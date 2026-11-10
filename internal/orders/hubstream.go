package orders

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
)

// HubFeed is what the hub's live channel reads, given by the hub package so neither imports the other.
type HubFeed struct {
	// Hub returns the staff member's hub, and false if they may not follow it.
	Hub func(ctx context.Context, staff string) (string, bool, error)
	// Seq returns the hub's seq.
	Seq func(ctx context.Context, hub string) (int64, error)
	// View returns one request as hub staff see it.
	View func(ctx context.Context, id string) (map[string]any, error)
}

// MountHubStream adds the hub's live channel: new and changed restock requests at the staff
// member's hub (contracts/partner/stream.asyncapi.yaml).
func (s *Service) MountHubStream(r chi.Router, st *Streams, feed HubFeed) {
	st.defaults()
	r.Get("/hub/stream", func(w http.ResponseWriter, r *http.Request) { s.hubStream(w, r, st, feed) })
}

func (s *Service) hubStream(w http.ResponseWriter, r *http.Request, st *Streams, feed HubFeed) {
	var hub string
	seen := map[string]int64{} // Each request's seq as last sent.
	// current reads the requests the channel follows: those in progress, and any changed lately.
	current := func(ctx context.Context) (map[string]int64, map[string]string, error) {
		rows, err := s.Pool.Query(ctx, `SELECT id::text, seq, state FROM restock_requests WHERE hub_id = $1
			AND state <> 'suggested' AND (state IN ('confirmed', 'hub_accepted', 'packing', 'ready', 'partial', 'picked_up') OR updated_at > now() - interval '1 hour')
			AND NOT (source = 'suggested' AND state IN ('expired', 'cancelled') AND accepted_by IS NULL AND slot_at IS NULL)`, hub)
		if err != nil {
			return nil, nil, err
		}
		seqs, states := map[string]int64{}, map[string]string{}
		var id, state string
		var seq int64
		_, err = pgx.ForEachRow(rows, []any{&id, &seq, &state}, func() error {
			seqs[id], states[id] = seq, state
			return nil
		})
		return seqs, states, err
	}
	s.serve(w, r, st, watcher{
		actorType: identity.TypeStaff,
		admit: func(ctx context.Context, a identity.Actor) (websocket.StatusCode, error) {
			h, ok, err := feed.Hub(ctx, a.ID)
			if err != nil {
				return 0, err
			}
			if !ok {
				return closeForbidden, nil
			}
			hub = h
			return 0, nil
		},
		seq: func(ctx context.Context) (int64, error) { return feed.Seq(ctx, hub) },
		// The app fetches the queue after auth_ok, so nothing is sent here; what it has is noted.
		initial: func(ctx context.Context, _ int64) ([]frame, error) {
			seqs, _, err := current(ctx)
			for id, seq := range seqs {
				seen[id] = seq
			}
			return nil, err
		},
		changed: func(ctx context.Context, seq int64) ([]frame, websocket.StatusCode, error) {
			seqs, states, err := current(ctx)
			if err != nil {
				return nil, 0, err
			}
			var out []frame
			for id, at := range seqs {
				last, known := seen[id]
				if known && at <= last {
					continue
				}
				v, err := feed.View(ctx, id)
				if err != nil {
					return nil, 0, err
				}
				kind := "request_changed"
				if !known && states[id] == "confirmed" {
					kind = "request_new"
				}
				// The frame carries the hub's seq; the request in it carries its own.
				out = append(out, frame{kind: kind, seq: max(seq, 1), data: v})
				seen[id] = at
			}
			return out, 0, nil
		},
	})
}
