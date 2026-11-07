// Package catalog holds the menu: items, prices and their history. The console changes it later, so
// nothing about the menu is written in code.
package catalog

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Money is an amount in paise, as the contract's Money.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// Item is one menu item, as the contract's MenuItem.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	Price       Money    `json:"price"`
	Allergens   []string `json:"allergens"`
	PhotoURL    *string  `json:"photo_url"`
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

const itemColumns = "id::text, name, description, price_paise, currency, allergens, photo_url"

func scanItem(row pgx.CollectableRow) (Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.Name, &it.Description, &it.Price.AmountMinor, &it.Price.Currency, &it.Allergens, &it.PhotoURL)
	if it.Allergens == nil {
		it.Allergens = []string{}
	}
	return it, err
}

// Items returns the items on sale, in menu order. Hidden items are left out.
func Items(ctx context.Context, q querier) ([]Item, error) {
	rows, err := q.Query(ctx, "SELECT "+itemColumns+" FROM menu_items WHERE status = 'active' ORDER BY sort_order, name, id")
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, scanItem)
	if items == nil {
		items = []Item{}
	}
	return items, err
}

// Service serves the menu.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

// MountCustomer adds listMenuItems and getMenuItem.
func (s *Service) MountCustomer(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/menu/items", s.list)
		g.Get("/menu/items/{item_id}", s.get)
	})
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Service) failed(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("catalog: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
	httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	items, err := Items(r.Context(), s.Pool)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "server_time": serverTime()})
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "item_id")
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "No such item.", false)
		return
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+itemColumns+" FROM menu_items WHERE id = $1 AND status = 'active'", id)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	it, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "No such item.", false)
		return
	}
	if err != nil {
		s.failed(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"item": it, "server_time": serverTime()})
}
