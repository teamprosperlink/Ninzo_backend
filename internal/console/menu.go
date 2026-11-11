package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// uniqueViolation reports a unique constraint failure, such as a name already in use.
func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// nullOn says whether any of the named fields was sent as an explicit null.
func nullOn(raw map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if v, ok := raw[k]; ok && strings.TrimSpace(string(v)) == "null" {
			return true
		}
	}
	return false
}

// chars counts characters, as the database does, not bytes.
func chars(s string) int { return utf8.RuneCountInString(s) }

func money(paise int) map[string]any {
	return map[string]any{"amount_minor": paise, "currency": "INR"}
}

// MountMenu adds listAdminMenu, createMenuItem, updateMenuItem and getPriceHistory.
func (s *Service) MountMenu(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Use(s.Require("menu.manage", false))
		g.Get("/admin/menu/items", s.listMenu)
		g.With(idem).Post("/admin/menu/items", s.createMenuItem)
		g.With(idem).Patch("/admin/menu/items/{item_id}", s.updateMenuItem)
		g.Get("/admin/menu/items/{item_id}/prices", s.priceHistory)
	})
}

const menuColumns = "id::text, name, description, price_paise, status, sort_order, allergens, photo_url, updated_at"

func scanMenuItem(row pgx.Row) (map[string]any, error) {
	var id, name, status string
	var desc, photo *string
	var price, sort int
	var allergens []string
	var at time.Time
	if err := row.Scan(&id, &name, &desc, &price, &status, &sort, &allergens, &photo, &at); err != nil {
		return nil, err
	}
	if allergens == nil {
		allergens = []string{}
	}
	return map[string]any{"id": id, "name": name, "description": desc, "price": money(price), "status": status, "sort_order": sort,
		"allergens": allergens, "photo_url": photo, "updated_at": at.UTC().Format(time.RFC3339Nano)}, nil
}

func (s *Service) listMenu(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), "SELECT "+menuColumns+" FROM menu_items ORDER BY sort_order, name, id")
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) { return scanMenuItem(row) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "server_time": serverTime()})
}

func validAllergens(list []string) bool {
	if len(list) > 20 {
		return false
	}
	for _, a := range list {
		if strings.TrimSpace(a) == "" || chars(a) > 40 {
			return false
		}
	}
	return true
}

func validPhoto(p string) bool { return strings.HasPrefix(p, "https://") && len(p) <= 1000 }

// priceReason is read by the price_history trigger (migration 0014), so the history says why.
func priceReason(ctx context.Context, tx pgx.Tx, reason string) error {
	_, err := tx.Exec(ctx, "SELECT set_config('ninzo.price_reason', $1, true)", reason)
	return err
}

func (s *Service) createMenuItem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string   `json:"name"`
		Description *string  `json:"description"`
		PricePaise  int      `json:"price_paise"`
		SortOrder   *int     `json:"sort_order"`
		Allergens   []string `json:"allergens"`
		PhotoURL    *string  `json:"photo_url"`
		Reason      string   `json:"reason"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	body.Name = strings.TrimSpace(body.Name)
	if err != nil || body.Name == "" || chars(body.Name) > 80 || body.PricePaise < 100 || body.PricePaise > 1000000 ||
		(body.Description != nil && chars(*body.Description) > 500) || (body.SortOrder != nil && (*body.SortOrder < 0 || *body.SortOrder > 10000)) ||
		!validAllergens(body.Allergens) || (body.PhotoURL != nil && !validPhoto(*body.PhotoURL)) || (body.Reason != "" && !validReason(body.Reason)) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields against the contract.", false)
		return
	}
	sort := 100
	if body.SortOrder != nil {
		sort = *body.SortOrder
	}
	if body.Allergens == nil {
		body.Allergens = []string{}
	}
	reason := body.Reason
	if reason == "" {
		reason = "the first price"
	}
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if err := priceReason(ctx, tx, reason); err != nil {
			return nil, err
		}
		// Added hidden: nobody can order it until someone shows it.
		item, err := scanMenuItem(tx.QueryRow(ctx, `INSERT INTO menu_items (name, description, price_paise, sort_order, allergens, photo_url, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'hidden') RETURNING `+menuColumns, body.Name, body.Description, body.PricePaise, sort, body.Allergens, body.PhotoURL))
		if uniqueViolation(err) {
			return nil, &refusal{"name_taken", "An item already has that name."}
		}
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "menu.created", EntityType: "menu_item",
			EntityID: item["id"].(string), After: map[string]any{"name": body.Name, "price_paise": body.PricePaise}, Reason: reason}); err != nil {
			return nil, err
		}
		return map[string]any{"item": item}, nil
	})
}

func (s *Service) updateMenuItem(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a JSON object.", false)
		return
	}
	var body struct {
		Name        *string  `json:"name"`
		Description *string  `json:"description"`
		PricePaise  *int     `json:"price_paise"`
		Status      *string  `json:"status"`
		SortOrder   *int     `json:"sort_order"`
		Allergens   []string `json:"allergens"`
		PhotoURL    *string  `json:"photo_url"`
		Reason      string   `json:"reason"`
	}
	buf, _ := json.Marshal(raw)
	if err := json.Unmarshal(buf, &body); err != nil || nullOn(raw, "name", "price_paise", "status", "sort_order", "allergens", "reason") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "A field has the wrong type, or is null where it cannot be.", false)
		return
	}
	_, setDesc := raw["description"]
	_, setPhoto := raw["photo_url"]
	_, setAllergens := raw["allergens"]
	bad := !validReason(body.Reason) || (body.Name != nil && (strings.TrimSpace(*body.Name) == "" || chars(*body.Name) > 80)) ||
		(body.Description != nil && chars(*body.Description) > 500) || (body.PricePaise != nil && (*body.PricePaise < 100 || *body.PricePaise > 1000000)) ||
		(body.Status != nil && *body.Status != "active" && *body.Status != "hidden") || (body.SortOrder != nil && (*body.SortOrder < 0 || *body.SortOrder > 10000)) ||
		(setAllergens && (body.Allergens == nil || !validAllergens(body.Allergens))) || (body.PhotoURL != nil && !validPhoto(*body.PhotoURL))
	if bad {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields against the contract.", false)
		return
	}
	id := chi.URLParam(r, "item_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		before, err := scanMenuItem(tx.QueryRow(ctx, "SELECT "+menuColumns+" FROM menu_items WHERE id = $1 FOR UPDATE", id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if err := priceReason(ctx, tx, body.Reason); err != nil {
			return nil, err
		}
		var name *string
		if body.Name != nil {
			n := strings.TrimSpace(*body.Name)
			name = &n
		}
		after, err := scanMenuItem(tx.QueryRow(ctx, `UPDATE menu_items SET name = coalesce($2, name),
				description = CASE WHEN $3 THEN $4 ELSE description END, price_paise = coalesce($5, price_paise), status = coalesce($6, status),
				sort_order = coalesce($7, sort_order), allergens = CASE WHEN $8 THEN $9::text[] ELSE allergens END,
				photo_url = CASE WHEN $10 THEN $11 ELSE photo_url END, updated_at = now()
			WHERE id = $1 RETURNING `+menuColumns, id, name, setDesc, body.Description, body.PricePaise, body.Status, body.SortOrder,
			setAllergens, body.Allergens, setPhoto, body.PhotoURL))
		if uniqueViolation(err) {
			return nil, &refusal{"name_taken", "An item already has that name."}
		}
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "menu.updated", EntityType: "menu_item", EntityID: id,
			Before: before, After: after, Reason: body.Reason}); err != nil {
			return nil, err
		}
		return map[string]any{"item": after}, nil
	})
}

func (s *Service) priceHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "item_id")
	var exists bool
	if idPattern.MatchString(id) {
		if err := s.Pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM menu_items WHERE id = $1)", id).Scan(&exists); err != nil {
			s.unavailable(w, r, err)
			return
		}
	}
	if !exists {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT price_paise, reason, created_at FROM price_history WHERE item_id = $1 ORDER BY created_at DESC, id DESC", id)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	var price int
	var reason string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&price, &reason, &at}, func() error {
		out = append(out, map[string]any{"price": money(price), "reason": reason, "created_at": at.UTC().Format(time.RFC3339Nano)})
		return nil
	}); err != nil {
		s.unavailable(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"item_id": id, "prices": out, "server_time": serverTime()})
}
