package profile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/notify"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

const maxAreaRunes = 80

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// readRiderView is the rider as the contract's Rider (getRider and updateRider). A blocked person
// is refused; a suspended rider is still shown, so the app can say why they cannot work.
func readRiderView(ctx context.Context, q queryRower, id string) (map[string]any, error) {
	var status, personStatus string
	var name, zone, area *string
	var created time.Time
	var approved *time.Time
	// approved_at is the newest approval in the audit trail: the command line writes rider.approved,
	// the console writes rider.approve. Null before the first approval.
	err := q.QueryRow(ctx, `SELECT r.name, r.status, p.status, r.zone_id::text, r.preferred_area, r.created_at,
			(SELECT max(a.created_at) FROM audit_events a WHERE a.entity_type = 'rider' AND a.entity_id = r.id::text
				AND a.action IN ('rider.approved', 'rider.approve'))
		FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`, id).Scan(&name, &status, &personStatus, &zone, &area, &created, &approved)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, errGone
	case err == nil && personStatus != "active":
		return nil, errBlocked
	case err != nil:
		return nil, err
	}
	return map[string]any{"id": id, "name": name, "status": status, "zone_id": zone, "preferred_area": area,
		"created_at": created.UTC().Format(time.RFC3339Nano), "approved_at": optionalStamp(approved)}, nil
}

// cleanText trims text and refuses control characters, so a name or area is one readable line.
func cleanText(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// updateRider changes the rider's own name and the area they would like to work in. The area is
// free text, what an applicant says they want: the rider's zone stays an ops decision.
func (s *Service) updateRider(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var raw map[string]json.RawMessage
	bad := func() {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a name of 1 to 80 characters and a preferred_area of up to 80.", false)
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		bad()
		return
	}
	var name, area *string
	for key, value := range raw {
		if key != "name" && key != "preferred_area" {
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			bad() // A number, a null or an object is not text.
			return
		}
		text, ok := cleanText(text)
		if !ok {
			bad()
			return
		}
		switch key {
		case "name":
			if n := utf8.RuneCountInString(text); n < 1 || n > maxNameRunes {
				bad()
				return
			}
			name = &text
		case "preferred_area":
			if utf8.RuneCountInString(text) > maxAreaRunes {
				bad()
				return
			}
			area = &text // An empty one clears it.
		}
	}
	if name == nil && area == nil {
		bad()
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := requireActive(ctx, tx, actor); err != nil {
			return err
		}
		var oldName, oldArea *string
		if err := tx.QueryRow(ctx, "SELECT name, preferred_area FROM riders WHERE id = $1 FOR UPDATE", actor.ID).Scan(&oldName, &oldArea); err != nil {
			return err
		}
		newName, newArea := oldName, oldArea
		if name != nil {
			newName = name
		}
		if area != nil {
			newArea = area
			if *area == "" {
				newArea = nil
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE riders SET name = $2, preferred_area = $3, updated_at = now() WHERE id = $1", actor.ID, newName, newArea); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "rider.updated", EntityType: "rider", EntityID: actor.ID,
			Before: map[string]any{"name": oldName, "preferred_area": oldArea}, After: map[string]any{"name": newName, "preferred_area": newArea}}); err != nil {
			return err
		}
		rider, err := readRiderView(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		reply = map[string]any{"rider": rider, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// The cursor is the last notification's time and id, so a new one never moves a page.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixMicro(), 10) + "|" + id))
}

func decodeCursor(raw string) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", false
	}
	micros, id, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(micros, 10, 64)
	if !ok || err != nil || !idPattern.MatchString(id) {
		return time.Time{}, "", false
	}
	return time.UnixMicro(n), id, true
}

// listNotifications lists what was pushed to the rider, newest first, kept 30 days.
func (s *Service) listNotifications(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "limit is from 1 to 100.", false)
			return
		}
		limit = n
	}
	after, afterID := time.Now().Add(time.Hour), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var ok bool
		if after, afterID, ok = decodeCursor(raw); !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That cursor is not valid.", false)
			return
		}
	}
	if !s.requireLive(w, r, actor) {
		return
	}
	if _, err := readRiderView(r.Context(), s.Pool, actor.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, kind, title, body, order_id::text, created_at FROM rider_notifications
		WHERE rider_id = $1 AND created_at > now() - make_interval(days => $5) AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $4`, actor.ID, after, afterID, limit+1, notify.Retention)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type row struct {
		id, kind, title, body string
		order                 *string
		at                    time.Time
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.kind, &x.title, &x.body, &x.order, &x.at)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var next any
	if len(found) > limit {
		found = found[:limit]
		last := found[len(found)-1]
		next = encodeCursor(last.at, last.id)
	}
	out := make([]map[string]any, 0, len(found))
	for _, x := range found {
		out = append(out, map[string]any{"id": x.id, "kind": x.kind, "title": x.title, "body": x.body, "order_id": x.order,
			"created_at": x.at.UTC().Format(time.RFC3339Nano)})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"notifications": out, "next_cursor": next, "server_time": serverTime()})
}
