package audit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

const actor = "11111111-1111-1111-1111-111111111111"

func count(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecordStoresTheEvent(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		return Record(ctx, tx, Event{
			ActorType: "rider", ActorID: actor, Action: "shift.start", EntityType: "shift", EntityID: "s1",
			Before: map[string]any{"open": false}, After: map[string]any{"open": true}, Reason: "start of day",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var actorType, id, action, entity, entityID, before, after, reason string
	err = pool.QueryRow(ctx, `SELECT actor_type, actor_id::text, action, entity_type, entity_id, before::text, after::text, reason FROM audit_events`).
		Scan(&actorType, &id, &action, &entity, &entityID, &before, &after, &reason)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join([]string{actorType, id, action, entity, entityID, before, after, reason}, "|")
	want := `rider|` + actor + `|shift.start|shift|s1|{"open": false}|{"open": true}|start of day`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSystemActionsHaveNoActor(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		return Record(ctx, tx, Event{ActorType: "system", Action: "refund.retry", EntityType: "refund", EntityID: "r1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var noActor, noBefore, noReason bool
	if err := pool.QueryRow(ctx, "SELECT actor_id IS NULL, before IS NULL, reason IS NULL FROM audit_events").Scan(&noActor, &noBefore, &noReason); err != nil {
		t.Fatal(err)
	}
	if !noActor || !noBefore || !noReason {
		t.Errorf("empty values must be stored as NULL: %v %v %v", noActor, noBefore, noReason)
	}
}

func TestRecordRollsBackWithTheChange(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	failed := errors.New("the change failed")
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := Record(ctx, tx, Event{ActorType: "system", Action: "a", EntityType: "e", EntityID: "1"}); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("got %v", err)
	}
	if n := count(t, pool); n != 0 {
		t.Errorf("%d audit rows survived a rollback", n)
	}
}

func TestRecordRefusesSecretsAndMissingFields(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	cases := map[string]Event{
		"a password in after":    {ActorType: "system", Action: "a", EntityType: "e", EntityID: "1", After: map[string]any{"password": "P1"}},
		"a token in before":      {ActorType: "system", Action: "a", EntityType: "e", EntityID: "1", Before: map[string]any{"session": map[string]any{"refresh_token": "T1"}}},
		"a handover code":        {ActorType: "system", Action: "a", EntityType: "e", EntityID: "1", After: []any{map[string]any{"handover_otp": "4821"}}},
		"a rider with no ID":     {ActorType: "rider", Action: "a", EntityType: "e", EntityID: "1"},
		"no actor type":          {Action: "a", EntityType: "e", EntityID: "1"},
		"no action":              {ActorType: "system", EntityType: "e", EntityID: "1"},
		"no entity type":         {ActorType: "system", Action: "a", EntityID: "1"},
		"no entity ID":           {ActorType: "system", Action: "a", EntityType: "e"},
		"an actor that is no ID": {ActorType: "rider", ActorID: "not-a-uuid", Action: "a", EntityType: "e", EntityID: "1"},
	}
	for name, event := range cases {
		err := database.InTx(ctx, pool, func(tx pgx.Tx) error { return Record(ctx, tx, event) })
		if err == nil {
			t.Errorf("%s must be refused", name)
			continue
		}
		for _, secret := range []string{"P1", "T1", "4821"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error shows the secret: %v", name, err)
			}
		}
	}
	if n := count(t, pool); n != 0 {
		t.Errorf("%d rows were stored", n)
	}
}

func TestTheServiceCannotChangeAuditRows(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		return Record(ctx, tx, Event{ActorType: "system", Action: "a", EntityType: "e", EntityID: "1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"UPDATE audit_events SET action = 'b'", "DELETE FROM audit_events"} {
		if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: got %v", stmt, err)
		}
	}
}
