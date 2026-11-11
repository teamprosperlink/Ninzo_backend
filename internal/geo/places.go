package geo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

const (
	// maxGates is per customer. Saved gates are addresses, and nobody needs more than this.
	maxGates = 20
	// duplicateWithinM treats two pins this close as the same gate.
	duplicateWithinM = 15
)

var (
	errOutOfArea     = errors.New("geo: outside every service area")
	errDuplicateGate = errors.New("geo: this gate is already saved")
	errGateLimit     = errors.New("geo: too many saved gates")
	errPhoneRequired = errors.New("geo: verify a phone first")
	errBlocked       = errors.New("geo: blocked")
)

// ZoneAt returns the active zone holding a point. Zones must not overlap, so the oldest wins if
// they ever do.
func ZoneAt(ctx context.Context, q querier, lat, lng float64) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM zones
		WHERE status = 'active' AND area IS NOT NULL
			AND extensions.ST_Covers(area, extensions.ST_SetSRID(extensions.ST_MakePoint($2, $1), 4326))
		ORDER BY created_at, id LIMIT 1`, lat, lng).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// Service serves gates and the location check.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

// MountCustomer adds listGates, createGate and checkLocation.
func (s *Service) MountCustomer(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/gates", s.listGates)
		g.With(idem).Post("/gates", s.createGate)
		g.Get("/locations/check", s.checkLocation)
	})
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	case errors.Is(err, errPhoneRequired):
		httpserver.WriteError(w, http.StatusConflict, "phone_required", "Verify a phone first.", false)
	case errors.Is(err, errOutOfArea):
		httpserver.WriteError(w, http.StatusBadRequest, "out_of_service_area", "This pin is outside the service area.", false)
	case errors.Is(err, errDuplicateGate):
		httpserver.WriteError(w, http.StatusConflict, "duplicate_gate", "This gate is already saved.", false)
	case errors.Is(err, errGateLimit):
		httpserver.WriteError(w, http.StatusConflict, "gate_limit", "Too many saved gates.", false)
	default:
		s.Log.Error("geo: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

// customerState reads what a gate needs to know about its customer.
func customerState(ctx context.Context, q querier, a identity.Actor) (hasPhone bool, err error) {
	var active bool
	err = q.QueryRow(ctx, `SELECT p.status = 'active', p.phone IS NOT NULL FROM customers c JOIN people p ON p.id = c.person_id
		WHERE c.id = $1`, a.ID).Scan(&active, &hasPhone)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, identity.ErrSessionInvalid
	}
	if err == nil && !active {
		err = errBlocked
	}
	return hasPhone, err
}

type gate struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	EntryNotes *string `json:"entry_notes"`
	Verified   bool    `json:"verified"`
}

func (s *Service) listGates(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	// Saved gates are where someone lives or works, so an ended session must not read them.
	if live, err := identity.SessionLive(r.Context(), s.Pool, actor); err != nil || !live {
		if err == nil {
			err = identity.ErrSessionInvalid
		}
		s.fail(w, r, err)
		return
	}
	if _, err := customerState(r.Context(), s.Pool, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, label, lat, lng, entry_notes, verified FROM gates
		WHERE customer_id = $1 ORDER BY created_at DESC, id`, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	gates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (gate, error) {
		var g gate
		err := row.Scan(&g.ID, &g.Label, &g.Lat, &g.Lng, &g.EntryNotes, &g.Verified)
		return g, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if gates == nil {
		gates = []gate{}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"gates": gates, "server_time": serverTime()})
}

func isBidiControl(c rune) bool {
	return (c >= 0x202A && c <= 0x202E) || (c >= 0x2066 && c <= 0x2069)
}

// cleanText trims text a rider will read, and refuses what could hide or reverse it.
// CleanText trims text and refuses control and bidi characters, as for customers' gates.
func CleanText(raw string, max int, newlines bool) (string, bool) {
	return cleanText(raw, max, newlines)
}

func cleanText(raw string, max int, newlines bool) (string, bool) {
	text := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > max {
		return "", false
	}
	bad := strings.ContainsFunc(text, func(c rune) bool {
		return (unicode.IsControl(c) && !(newlines && c == '\n')) || isBidiControl(c) ||
			(unicode.Is(unicode.Cf, c) && c != 0x200C && c != 0x200D) || c == 0x2028 || c == 0x2029
	})
	return text, !bad
}

func validCoordinate(v *float64, limit float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && math.Abs(*v) <= limit
}

func (s *Service) createGate(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Label      string   `json:"label"`
		Lat        *float64 `json:"lat"`
		Lng        *float64 `json:"lng"`
		EntryNotes *string  `json:"entry_notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validCoordinate(body.Lat, 90) || !validCoordinate(body.Lng, 180) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send label, lat and lng.", false)
		return
	}
	label, ok := cleanText(body.Label, 80, false)
	if !ok || label == "" {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That label is not valid.", false)
		return
	}
	var notes *string
	if body.EntryNotes != nil {
		text, ok := cleanText(*body.EntryNotes, 500, true)
		if !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Those entry notes are not valid.", false)
			return
		}
		if text != "" {
			notes = &text
		}
	}
	lat, lng := *body.Lat, *body.Lng
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		// Takes the actor lock, so two taps cannot both pass the duplicate and limit checks.
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		hasPhone, err := customerState(r.Context(), tx, actor)
		if err != nil {
			return err
		}
		if !hasPhone {
			return errPhoneRequired
		}
		zone, found, err := ZoneAt(r.Context(), tx, lat, lng)
		if err != nil {
			return err
		}
		if !found {
			return errOutOfArea
		}
		var count int
		var duplicate bool
		err = tx.QueryRow(r.Context(), `SELECT count(*), coalesce(bool_or(extensions.ST_DWithin(
				extensions.ST_SetSRID(extensions.ST_MakePoint(lng, lat), 4326)::extensions.geography,
				extensions.ST_SetSRID(extensions.ST_MakePoint($3, $2), 4326)::extensions.geography, $4)), false)
			FROM gates WHERE customer_id = $1`, actor.ID, lat, lng, duplicateWithinM).Scan(&count, &duplicate)
		if err != nil {
			return err
		}
		if duplicate {
			return errDuplicateGate
		}
		if count >= maxGates {
			return errGateLimit
		}
		g := gate{Label: label, Lat: lat, Lng: lng, EntryNotes: notes}
		// Verified because it is inside an area we serve (owner, decision 0012). An order checks the
		// zone again, so pausing an area stops its gates without changing them.
		if err := tx.QueryRow(r.Context(), `INSERT INTO gates (customer_id, zone_id, label, lat, lng, entry_notes, verified)
			VALUES ($1, $2, $3, $4, $5, $6, true) RETURNING id::text, verified`, actor.ID, zone, label, lat, lng, notes).Scan(&g.ID, &g.Verified); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO gate_history (gate_id, version, label, lat, lng, entry_notes, verified, reason)
			VALUES ($1, 1, $2, $3, $4, $5, $6, 'saved by the customer')`, g.ID, label, lat, lng, notes, g.Verified); err != nil {
			return err
		}
		// Not the pin or the label: audit rows are kept for good, and these say where someone lives.
		if err := audit.Record(r.Context(), tx, audit.Event{
			ActorType: identity.TypeCustomer, ActorID: actor.ID, Action: "gate.created", EntityType: "gate", EntityID: g.ID,
			After: map[string]any{"zone_id": zone},
		}); err != nil {
			return err
		}
		reply = map[string]any{"gate": g, "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusCreated, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

// checkLocation says whether a pin is served, and saves nothing, so a customer can look before
// giving a phone (decision 0011).
func (s *Service) checkLocation(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if live, err := identity.SessionLive(r.Context(), s.Pool, actor); err != nil || !live {
		if err == nil {
			err = identity.ErrSessionInvalid
		}
		s.fail(w, r, err)
		return
	}
	lat, err1 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lng, err2 := strconv.ParseFloat(r.URL.Query().Get("lng"), 64)
	if err1 != nil || err2 != nil || !validCoordinate(&lat, 90) || !validCoordinate(&lng, 180) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send lat and lng.", false)
		return
	}
	zone, found, err := ZoneAt(r.Context(), s.Pool, lat, lng)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var zoneID any
	if found {
		zoneID = zone
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"in_service_area": found, "zone_id": zoneID, "server_time": serverTime()})
}

// RedactGates removes a customer's gate labels and notes, and blurs their pins to about 1 km, in
// the gates and their history. It is for erasing a customer's data. Orders keep pointing at the rows.
func RedactGates(ctx context.Context, tx pgx.Tx, customerID string) error {
	if _, err := tx.Exec(ctx, `UPDATE gates SET label = '(removed)', entry_notes = NULL,
			lat = round(lat::numeric, 2)::double precision, lng = round(lng::numeric, 2)::double precision, updated_at = now()
		WHERE customer_id = $1`, customerID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE gate_history h SET label = '(removed)', entry_notes = NULL,
			lat = round(h.lat::numeric, 2)::double precision, lng = round(h.lng::numeric, 2)::double precision, redacted_at = now()
		FROM gates g WHERE g.id = h.gate_id AND g.customer_id = $1 AND h.redacted_at IS NULL`, customerID)
	return err
}
