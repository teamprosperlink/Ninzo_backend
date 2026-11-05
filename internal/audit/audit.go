// Package audit records writes in the append-only audit_events table.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/sensitive"
)

// ActorSystem is the actor type for work no person started.
const ActorSystem = "system"

// Event is one audited change. Before and After are JSON-encoded, and must hold no secrets.
type Event struct {
	ActorType  string // Such as customer, rider, staff or system.
	ActorID    string // Empty for system actions.
	Action     string
	EntityType string
	EntityID   string
	Before     any
	After      any
	Reason     string
}

// Record writes the event. It takes a transaction, so a change and its record commit or fail
// together, and code cannot audit outside one. A secret in the event is refused, which fails the
// transaction: losing an action is safer than keeping a password in a table nobody can edit.
func Record(ctx context.Context, tx pgx.Tx, e Event) error {
	if e.ActorType == "" || e.Action == "" || e.EntityType == "" || e.EntityID == "" {
		return errors.New("audit: actor type, action, entity type and entity ID are required")
	}
	if e.ActorID == "" && e.ActorType != ActorSystem {
		return errors.New("audit: an actor ID is required, except for system actions")
	}
	before, err := encode(e.Before)
	if err != nil {
		return fmt.Errorf("audit: before: %w", err)
	}
	after, err := encode(e.After)
	if err != nil {
		return fmt.Errorf("audit: after: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (actor_type, actor_id, action, entity_type, entity_id, before, after, reason)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7, NULLIF($8, ''))`,
		e.ActorType, e.ActorID, e.Action, e.EntityType, e.EntityID, before, after, e.Reason)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

func encode(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if found := sensitive.Find(raw); len(found) > 0 {
		return nil, fmt.Errorf("refusing secret fields: %s", strings.Join(found, ", "))
	}
	return raw, nil
}
