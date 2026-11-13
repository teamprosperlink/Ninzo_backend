package profile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
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
	var name, zone, area, phone, email, areaName, areaCity *string
	var areaLat, areaLng *float64
	var created time.Time
	var approved *time.Time
	// approved_at is the newest approval in the audit trail: the command line writes rider.approved,
	// the console writes rider.approve. Null before the first approval.
	err := q.QueryRow(ctx, `SELECT r.name, r.status, p.status, r.zone_id::text, r.preferred_area, r.created_at, p.phone, r.email, r.area_name, r.area_city, r.area_lat, r.area_lng,
			(SELECT max(a.created_at) FROM audit_events a WHERE a.entity_type = 'rider' AND a.entity_id = r.id::text
				AND a.action IN ('rider.approved', 'rider.approve'))
		FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`, id).Scan(&name, &status, &personStatus, &zone, &area, &created, &phone, &email, &areaName, &areaCity, &areaLat, &areaLng, &approved)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, errGone
	case err == nil && personStatus != "active":
		return nil, errBlocked
	case err != nil:
		return nil, err
	}
	view := map[string]any{"id": id, "name": name, "status": status, "zone_id": zone, "preferred_area": area,
		"created_at": created.UTC().Format(time.RFC3339Nano), "approved_at": optionalStamp(approved), "email": email, "area": nil}
	// The rider's own number, as the rider app reads it. It is left out if the account has none, since
	// the contract's phone is never null. No customer's number is ever read here.
	if phone != nil {
		view["phone"] = *phone
	}
	if areaName != nil {
		view["area"] = map[string]any{"name": *areaName, "city": *areaCity, "lat": *areaLat, "lng": *areaLng}
	}
	return view, nil
}

// cleanText trims text and refuses control characters, so a name or area is one readable line.
func cleanText(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// riderArea is the place a rider picked, as the rider app sends it.
type riderArea struct {
	Name string
	City string
	Lat  float64
	Lng  float64
}

// parseArea reads and checks the contract's RiderArea. Anything else is refused.
func parseArea(raw json.RawMessage) (riderArea, bool) {
	var in struct {
		Name *string  `json:"name"`
		City *string  `json:"city"`
		Lat  *float64 `json:"lat"`
		Lng  *float64 `json:"lng"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.Name == nil || in.City == nil || in.Lat == nil || in.Lng == nil {
		return riderArea{}, false
	}
	name, ok1 := cleanText(*in.Name)
	city, ok2 := cleanText(*in.City)
	if !ok1 || !ok2 || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > maxAreaRunes || utf8.RuneCountInString(city) > maxAreaRunes ||
		*in.Lat < -90 || *in.Lat > 90 || *in.Lng < -180 || *in.Lng > 180 {
		return riderArea{}, false
	}
	return riderArea{name, city, *in.Lat, *in.Lng}, true
}

// validEmail accepts a plain address of up to 254 characters, not a name with an address in brackets.
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// updateRider changes the rider's own name, email and the area they would like to work in. The area
// is what an applicant says they want: the rider's zone stays an ops decision. The rider app sends
// area (a picked place), ours sends preferred_area (free text); either way preferred_area and the
// area's name stay the same text, and sending both is refused.
func (s *Service) updateRider(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var raw map[string]json.RawMessage
	bad := func() {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request",
			"Send a name of 1 to 80 characters, a preferred_area of up to 80 or an area, and an email address.", false)
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		bad()
		return
	}
	var name, preferred, email *string
	var place *riderArea
	var setPreferred, setEmail, setArea bool
	for key, value := range raw {
		switch key {
		case "name", "preferred_area":
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				bad() // A number or an object is not text.
				return
			}
			text, ok := cleanText(text)
			if !ok {
				bad()
				return
			}
			if key == "name" {
				if n := utf8.RuneCountInString(text); n < 1 || n > maxNameRunes {
					bad()
					return
				}
				name = &text
			} else {
				if utf8.RuneCountInString(text) > maxAreaRunes {
					bad()
					return
				}
				preferred, setPreferred = &text, true // An empty one clears it.
			}
		case "email":
			setEmail = true
			if string(value) == "null" {
				break // null clears it.
			}
			var text string
			if err := json.Unmarshal(value, &text); err != nil || !validEmail(text) {
				bad()
				return
			}
			email = &text
		case "area":
			setArea = true
			if string(value) == "null" {
				break // null clears it.
			}
			a, ok := parseArea(value)
			if !ok {
				bad()
				return
			}
			place = &a
		}
	}
	if (name == nil && !setPreferred && !setEmail && !setArea) || (setPreferred && setArea) {
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
		var oldName, oldPreferred, oldEmail, oldPlace *string
		if err := tx.QueryRow(ctx, "SELECT name, preferred_area, email, area_name FROM riders WHERE id = $1 FOR UPDATE", actor.ID).
			Scan(&oldName, &oldPreferred, &oldEmail, &oldPlace); err != nil {
			return err
		}
		newName, newPreferred, newEmail := oldName, oldPreferred, oldEmail
		if name != nil {
			newName = name
		}
		if setEmail {
			newEmail = email
		}
		// The area, as columns. They are kept only while they still name preferred_area, so a new
		// preferred_area that is not the stored place's name drops the place.
		areaTouched := false
		var aName, aCity *string
		var aLat, aLng *float64
		switch {
		case place != nil:
			newPreferred = &place.Name
			aName, aCity, aLat, aLng = &place.Name, &place.City, &place.Lat, &place.Lng
			areaTouched = true
		case setArea: // area null: the area goes, and preferred_area with it if it was that area's name.
			areaTouched = true
			if oldPlace != nil {
				newPreferred = nil
			}
		case setPreferred:
			newPreferred = preferred
			if *preferred == "" {
				newPreferred = nil
			}
			areaTouched = oldPlace != nil && (newPreferred == nil || *newPreferred != *oldPlace)
		}
		if _, err := tx.Exec(ctx, `UPDATE riders SET name = $2, preferred_area = $3, email = $4, updated_at = now(),
				area_name = CASE WHEN $9 THEN $5 ELSE area_name END, area_city = CASE WHEN $9 THEN $6 ELSE area_city END,
				area_lat = CASE WHEN $9 THEN $7 ELSE area_lat END, area_lng = CASE WHEN $9 THEN $8 ELSE area_lng END WHERE id = $1`,
			actor.ID, newName, newPreferred, newEmail, aName, aCity, aLat, aLng, areaTouched); err != nil {
			return err
		}
		hasArea := oldPlace != nil
		if areaTouched {
			hasArea = aName != nil
		}
		// The email itself is never written to the audit trail, only whether there is one.
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "rider.updated", EntityType: "rider", EntityID: actor.ID,
			Before: map[string]any{"name": oldName, "preferred_area": oldPreferred, "has_email": oldEmail != nil, "has_area": oldPlace != nil},
			After:  map[string]any{"name": newName, "preferred_area": newPreferred, "has_email": newEmail != nil, "has_area": hasArea}}); err != nil {
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
