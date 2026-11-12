package geo

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

func gateJSON(extra string) string {
	return `{"label":"Home","lat":12.935,"lng":77.625` + extra + `}`
}

func TestAGateKeepsItsLandmarkAndType(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	code, out := f.send("POST", "/gates", token, gateJSON(`,"landmark":"  Opposite the pharmacy ","gate_type":"side_gate"`))
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	g := out["gate"].(map[string]any)
	if g["landmark"] != "Opposite the pharmacy" || g["gate_type"] != "side_gate" {
		t.Errorf("created: %v", g)
	}
	_, list := f.send("GET", "/gates", token, "")
	if got := list["gates"].([]any)[0].(map[string]any); got["landmark"] != "Opposite the pharmacy" || got["gate_type"] != "side_gate" {
		t.Errorf("listed: %v", got)
	}
	if n := f.count(`SELECT count(*) FROM gates WHERE id = $1 AND landmark = 'Opposite the pharmacy' AND gate_type = 'side_gate'`, g["id"]); n != 1 {
		t.Error("the gate row does not hold them")
	}
	if n := f.count(`SELECT count(*) FROM gate_history WHERE gate_id = $1 AND version = 1 AND landmark = 'Opposite the pharmacy' AND gate_type = 'side_gate'`, g["id"]); n != 1 {
		t.Error("the first version does not hold them")
	}
	// The landmark is where someone lives, so the audit row does not hold it.
	if n := f.count(`SELECT count(*) FROM audit_events WHERE action = 'gate.created' AND after::text ILIKE '%pharmacy%'`); n != 0 {
		t.Error("the landmark was kept in the audit log for good")
	}
}

func TestAGateWithoutThemHasNull(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	code, out := f.send("POST", "/gates", token, gateJSON(``))
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	g := out["gate"].(map[string]any)
	if v, ok := g["landmark"]; !ok || v != nil {
		t.Errorf("landmark: %v %v", v, ok)
	}
	if v, ok := g["gate_type"]; !ok || v != nil {
		t.Errorf("gate_type: %v %v", v, ok)
	}
	_, list := f.send("GET", "/gates", token, "")
	if got := list["gates"].([]any)[0].(map[string]any); got["landmark"] != nil || got["gate_type"] != nil {
		t.Errorf("listed: %v", got)
	}
	if n := f.count(`SELECT count(*) FROM gates WHERE landmark IS NULL AND gate_type IS NULL`); n != 1 {
		t.Error("the gate row holds something that was not sent")
	}
}

func TestLandmarkAndGateTypeAreValidated(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	for _, extra := range []string{
		`,"landmark":""`, `,"landmark":"   "`, `,"landmark":"` + strings.Repeat("a", 81) + `"`, `,"landmark":null`, `,"landmark":5`,
		`,"landmark":"a\nb"`, `,"landmark":"a‮b"`,
		`,"gate_type":""`, `,"gate_type":"Main_Gate"`, `,"gate_type":"main gate"`, `,"gate_type":"1gate"`, `,"gate_type":"main-gate"`,
		`,"gate_type":"_gate"`, `,"gate_type":"` + strings.Repeat("a", 31) + `"`, `,"gate_type":null`, `,"gate_type":3`, `,"gate_type":"gate\n"`,
	} {
		if code, out := f.send("POST", "/gates", token, gateJSON(extra)); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", extra, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM gates"); n != 0 {
		t.Error("a refused gate was stored")
	}
	// The edges are accepted: 80 characters of landmark, 30 of type, an open-list word, and a Kannada landmark counted in characters.
	for i, extra := range []string{
		`,"landmark":"` + strings.Repeat("a", 80) + `"`, `,"gate_type":"` + strings.Repeat("a", 30) + `"`, `,"gate_type":"x"`,
		`,"gate_type":"back_gate"`, `,"landmark":"` + strings.Repeat("ಮ", 80) + `"`,
	} {
		_, tk := f.customer(fmt.Sprintf("+91980000010%d", i))
		if code, out := f.send("POST", "/gates", tk, gateJSON(extra)); code != 201 {
			t.Errorf("%.40s: %d %v", extra, code, out)
		}
	}
	// The database refuses what the API would not, too.
	if _, err := f.s.Pool.Exec(context.Background(), "UPDATE gates SET gate_type = 'Bad Type'"); err == nil {
		t.Error("the database took a bad gate type")
	}
	if _, err := f.s.Pool.Exec(context.Background(), "UPDATE gates SET landmark = ''"); err == nil {
		t.Error("the database took an empty landmark")
	}
}

func TestErasingACustomerClearsTheLandmark(t *testing.T) {
	f := newFixture(t)
	id, token := f.customer("+919800000001")
	_, out := f.send("POST", "/gates", token, gateJSON(`,"landmark":"Behind the temple","gate_type":"tower"`))
	gid := out["gate"].(map[string]any)["id"].(string)
	if err := database.InTx(context.Background(), f.s.Pool, func(tx pgx.Tx) error { return RedactGates(context.Background(), tx, id) }); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM gates WHERE id = $1 AND landmark IS NULL AND gate_type = 'tower'`, gid); n != 1 {
		t.Error("the gate still holds the landmark")
	}
	if n := f.count(`SELECT count(*) FROM gate_history WHERE gate_id = $1 AND landmark IS NULL AND redacted_at IS NOT NULL`, gid); n != 1 {
		t.Error("the history still holds the landmark")
	}
}
