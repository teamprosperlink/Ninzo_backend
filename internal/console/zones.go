package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// badSetting is a settings change refused before anything is stored: 400 invalid_setting.
type badSetting struct{ message string }

func (b *badSetting) Error() string { return "console: invalid setting: " + b.message }

// settingsLock is the lock geo.ChangeSettings takes; a version check holds it first, so the
// version cannot move between the check and the change.
const settingsLock = "SELECT pg_advisory_xact_lock(hashtextextended('zone_settings', 5))"

// MountZones adds listZones, the settings views and requests, and requestKillSwitch.
func (s *Service) MountZones(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("zones.read", false)).Get("/admin/zones", s.listZones)
		g.With(s.Require("settings.read", false)).Get("/admin/settings", s.getSettings)
		g.With(s.Require("settings.read", false)).Get("/admin/zones/{zone_id}/settings", s.getSettings)
		g.With(s.Require("settings.request", false), idem).Put("/admin/settings", s.requestSettings)
		g.With(s.Require("settings.request", false), idem).Put("/admin/zones/{zone_id}/settings", s.requestSettings)
		g.With(s.Require("zones.kill_switch", false), idem).Post("/admin/zones/{zone_id}/kill-switch", s.requestKillSwitch)
	})
}

func (s *Service) listZones(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), "SELECT id::text, name, city, status = 'active', is_test FROM zones ORDER BY name, id")
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	var id, name, city string
	var on, test bool
	if _, err := pgx.ForEachRow(rows, []any{&id, &name, &city, &on, &test}, func() error {
		out = append(out, map[string]any{"id": id, "name": name, "city": city, "offer_enabled": on, "is_test": test})
		return nil
	}); err != nil {
		s.unavailable(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"zones": out, "server_time": serverTime()})
}

// zoneExists checks a zone id from the path; an empty one means the global settings.
func zoneExists(ctx context.Context, q querier, zone string) error {
	if zone == "" {
		return nil
	}
	if !idPattern.MatchString(zone) {
		return errNotFound
	}
	var ok bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM zones WHERE id = $1)", zone).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errNotFound
	}
	return nil
}

// SettingsView is the contract's SettingsView for the global settings (zone "") or one zone.
func SettingsView(ctx context.Context, q querier, zone string) (map[string]any, error) {
	if err := zoneExists(ctx, q, zone); err != nil {
		return nil, err
	}
	eff, err := geo.LoadSettings(ctx, q, zone)
	if err != nil {
		return nil, err
	}
	own, version, err := geo.OwnSettings(ctx, q, zone)
	if err != nil {
		return nil, err
	}
	list := []map[string]any{}
	for _, st := range geo.Settings {
		list = append(list, map[string]any{"key": st.Name, "unit": st.Unit, "min": st.Min, "max": st.Max, "value": own[st.Name],
			"effective": eff.Values[st.Name], "required": st.Required, "description": st.About})
	}
	view := map[string]any{"zone_id": nil, "global_version": eff.GlobalVersion, "zone_version": nil, "settings": list}
	if zone != "" {
		view["zone_id"], view["zone_version"] = zone, version
	}
	return view, nil
}

func (s *Service) getSettings(w http.ResponseWriter, r *http.Request) {
	// One snapshot, so a value and the version beside it always come from the same moment.
	var v map[string]any
	err := pgx.BeginTxFunc(r.Context(), s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		v, err = SettingsView(r.Context(), tx, chi.URLParam(r, "zone_id"))
		return err
	})
	if err != nil {
		s.write(w, r, 0, nil, err)
		return
	}
	v["server_time"] = serverTime()
	httpserver.WriteJSON(w, http.StatusOK, v)
}

// settingsChange is what a settings approval holds (contract: SettingsApprovalChange).
type settingsChange struct {
	Kind        string        `json:"kind"`
	ZoneID      *string       `json:"zone_id"`
	BaseVersion int           `json:"base_version"`
	Diffs       []settingDiff `json:"diffs"`
}

type settingDiff struct {
	Key    string `json:"key"`
	Before *int   `json:"before"`
	After  *int   `json:"after"`
}

// currentVersion is the version a change is based on: the global one, or the zone's own (0 if none).
func currentVersion(ctx context.Context, q querier, zone string) (int, error) {
	_, version, err := geo.OwnSettings(ctx, q, zone)
	return version, err
}

// tryChange runs geo.ChangeSettings in a savepoint and undoes it: a change that would leave any
// zone invalid is refused when it is asked for, not only when it is approved.
func tryChange(ctx context.Context, tx pgx.Tx, zone string, changes []geo.Change, by string) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT try_settings"); err != nil {
		return err
	}
	_, err := geo.ChangeSettings(ctx, tx, zone, changes, "a trial of a requested change", by)
	if _, rerr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT try_settings"); rerr != nil {
		return rerr
	}
	if errors.Is(err, geo.ErrInvalidSetting) {
		return &badSetting{strings.TrimPrefix(err.Error(), "geo: invalid setting: ")}
	}
	return err
}

func (s *Service) requestSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseVersion *int            `json:"base_version"`
		Changes     map[string]*int `json:"changes"`
		Reason      string          `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BaseVersion == nil || *body.BaseVersion < 0 || len(body.Changes) == 0 || len(body.Changes) > 50 || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send changes, base_version and a reason.", false)
		return
	}
	zone := chi.URLParam(r, "zone_id")
	s.change(w, r, http.StatusAccepted, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if err := zoneExists(ctx, tx, zone); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(body.Changes))
		for k := range body.Changes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		args := make([]string, 0, len(keys))
		for _, k := range keys {
			v := ""
			if body.Changes[k] != nil {
				v = fmt.Sprint(*body.Changes[k])
			}
			args = append(args, k+"="+v)
		}
		changes, err := geo.ParseChanges(args)
		if err != nil {
			return nil, &badSetting{strings.TrimPrefix(err.Error(), "geo: invalid setting: ")}
		}
		if _, err := tx.Exec(ctx, settingsLock); err != nil {
			return nil, err
		}
		own, version, err := geo.OwnSettings(ctx, tx, zone)
		if err != nil {
			return nil, err
		}
		if version != *body.BaseVersion {
			return nil, &refusal{"stale_version", "The settings changed since you read them. Read them again."}
		}
		if err := tryChange(ctx, tx, zone, changes, "console:"+u.ID); err != nil {
			return nil, err
		}
		c := settingsChange{Kind: "settings", BaseVersion: version}
		target, summary := "global", "Global settings: "
		if zone != "" {
			c.ZoneID, target, summary = &zone, zone, "Zone settings: "
		}
		names := []string{}
		for _, ch := range changes {
			if same(own[ch.Name], ch.Value) {
				continue
			}
			c.Diffs = append(c.Diffs, settingDiff{Key: ch.Name, Before: own[ch.Name], After: ch.Value})
			names = append(names, ch.Name)
		}
		if len(c.Diffs) == 0 {
			return nil, &refusal{"no_change", "Every value is already that way."}
		}
		id, err := RequestChange(ctx, tx, u, "settings", target, summary+strings.Join(names, ", "), c, body.Reason, s.clock())
		if err != nil {
			return nil, err
		}
		v, err := ApprovalView(ctx, tx, id)
		return map[string]any{"approval": v}, err
	})
}

func same(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// ApplySettings is the settings kind's Applier: it applies only if the version is still the one
// the change was asked against, and the result is still valid in every zone.
func ApplySettings(ctx context.Context, tx pgx.Tx, raw json.RawMessage, by string, _ time.Time) (string, error) {
	var c settingsChange
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	zone := ""
	if c.ZoneID != nil {
		zone = *c.ZoneID
	}
	if _, err := tx.Exec(ctx, settingsLock); err != nil {
		return "", err
	}
	version, err := currentVersion(ctx, tx, zone)
	if err != nil {
		return "", err
	}
	if version != c.BaseVersion {
		return "stale_version", nil
	}
	changes := make([]geo.Change, 0, len(c.Diffs))
	for _, d := range c.Diffs {
		changes = append(changes, geo.Change{Name: d.Key, Value: d.After})
	}
	if _, err := geo.ChangeSettings(ctx, tx, zone, changes, "approved in the console", "console:"+by); err != nil {
		if errors.Is(err, geo.ErrInvalidSetting) {
			return "invalid_setting", nil
		}
		return "", err
	}
	return "", nil
}

func (s *Service) requestKillSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OfferEnabled *bool  `json:"offer_enabled"`
		Reason       string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OfferEnabled == nil || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send offer_enabled and a reason.", false)
		return
	}
	zone := chi.URLParam(r, "zone_id")
	s.change(w, r, http.StatusAccepted, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(zone) {
			return nil, errNotFound
		}
		var on bool
		var name string
		err := tx.QueryRow(ctx, "SELECT status = 'active', name FROM zones WHERE id = $1", zone).Scan(&on, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if on == *body.OfferEnabled {
			return nil, &refusal{"no_change", "The zone is already that way."}
		}
		if *body.OfferEnabled {
			if code, err := canTurnOn(ctx, tx, zone); err != nil || code != "" {
				if err != nil {
					return nil, err
				}
				return nil, &refusal{code, "This zone cannot serve: it has no area, or its area overlaps another active zone."}
			}
		}
		word := map[bool]string{true: "on", false: "off"}
		id, err := RequestChange(ctx, tx, u, "kill_switch", zone, name+": the offer "+word[*body.OfferEnabled],
			map[string]any{"kind": "kill_switch", "zone_id": zone, "offer_enabled_before": on, "offer_enabled_after": *body.OfferEnabled}, body.Reason, s.clock())
		if err != nil {
			return nil, err
		}
		v, err := ApprovalView(ctx, tx, id)
		return map[string]any{"approval": v}, err
	})
}

// canTurnOn says why a zone cannot be turned on: no area to serve (zone_has_no_area), or an area
// that overlaps another active zone, which would take over that zone's points (zone_overlaps).
func canTurnOn(ctx context.Context, q querier, zone string) (string, error) {
	var hasArea, overlaps bool
	// Sharing a border is fine; sharing any ground is not.
	if err := q.QueryRow(ctx, `SELECT z.area IS NOT NULL, z.area IS NOT NULL AND EXISTS (SELECT 1 FROM zones o
			WHERE o.id <> z.id AND o.status = 'active' AND o.area IS NOT NULL
				AND extensions.ST_Intersects(o.area, z.area) AND NOT extensions.ST_Touches(o.area, z.area))
		FROM zones z WHERE z.id = $1`, zone).Scan(&hasArea, &overlaps); err != nil {
		return "", err
	}
	if !hasArea {
		return "zone_has_no_area", nil
	}
	if overlaps {
		return "zone_overlaps", nil
	}
	return "", nil
}

// ApplyKillSwitch is the kill_switch kind's Applier. Off pauses the zone, so no new order starts
// there; orders already running finish.
func ApplyKillSwitch(ctx context.Context, tx pgx.Tx, raw json.RawMessage, by string, now time.Time) (string, error) {
	var c struct {
		Zone   string `json:"zone_id"`
		Before bool   `json:"offer_enabled_before"`
		After  bool   `json:"offer_enabled_after"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	if c.After {
		// Checked again: the zones may have changed while the request waited.
		if code, err := canTurnOn(ctx, tx, c.Zone); err != nil || code != "" {
			return code, err
		}
	}
	status := map[bool]string{true: "active", false: "paused"}
	tag, err := tx.Exec(ctx, "UPDATE zones SET status = $2, updated_at = $4 WHERE id = $1 AND status = $3", c.Zone, status[c.After], status[c.Before], now)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "stale_version", nil
	}
	return "", audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: by, Action: "zone.kill_switch", EntityType: "zone", EntityID: c.Zone,
		Before: map[string]any{"offer_enabled": c.Before}, After: map[string]any{"offer_enabled": c.After}})
}
