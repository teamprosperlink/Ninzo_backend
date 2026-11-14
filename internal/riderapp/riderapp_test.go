package riderapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// Issue 235, part B2: the rider app's settings, help, feedback and shift plan calls, through the real
// HTTP handlers, with every reply checked against the bundled rider contract.

const riderContract = "rider.openapi.yaml"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	sessions *identity.Sessions
	srv      *httptest.Server
	zone     string
	now      time.Time
	keys     int
	phones   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t), now: time.Now()}
	f.sessions = &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	svc := &Service{Pool: f.pool, Log: quiet, Now: func() time.Time { return f.now }}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(f.srv.Close)
	f.zone = f.id("INSERT INTO zones (name, city, status) VALUES ('Zone', 'Bengaluru', 'active') RETURNING id::text")
	return f
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// rider makes a rider in the zone (none with zone ""), signed in. status is the rider's status.
func (f *fixture) rider(zone, status string) (string, string) {
	f.t.Helper()
	f.phones++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198000002%02d", f.phones))
	id := f.id("INSERT INTO riders (person_id, name, status, zone_id) VALUES ($1, $2, $3, NULLIF($4, '')::uuid) RETURNING id::text", person, fmt.Sprintf("Rider %d", f.phones), status, zone)
	var pair identity.Pair
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.sessions.Start(context.Background(), tx, identity.TypeRider, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return id, pair.AccessToken
}

func (f *fixture) send(method, path, token, key, body string) (int, map[string]any) {
	f.t.Helper()
	if key == "" {
		f.keys++
		key = fmt.Sprintf("riderapp-key-%05d", f.keys)
	}
	req, _ := http.NewRequest(method, f.srv.URL+"/v1"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// zoneSettings stores a new version for the zone with only the named columns set.
func (f *fixture) zoneSettings(zone string, values map[string]int) {
	f.t.Helper()
	cols, marks, args := []string{"zone_id", "version", "reason", "created_by"}, []string{"$1", "$2", "'test'", "'test'"}, []any{zone, f.count("SELECT coalesce(max(version), 0) + 1 FROM zone_settings WHERE zone_id = $1", zone)}
	for name, v := range values {
		cols = append(cols, name)
		args = append(args, v)
		marks = append(marks, fmt.Sprintf("$%d", len(args)))
	}
	f.exec("INSERT INTO zone_settings ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", args...)
}

// reportProblemTypes reads the type values of reportProblem from the bundled contract.
func reportProblemTypes(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := contracts.Dist.ReadFile("dist/" + riderContract)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]struct {
							Enum []string `yaml:"enum"`
						} `yaml:"properties"`
					} `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"requestBody"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			if op.OperationID == "reportProblem" {
				for _, v := range op.RequestBody.Content["application/json"].Schema.Properties["type"].Enum {
					out[v] = true
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("reportProblem has no type values in the contract")
	}
	return out
}

func choiceCodes(t *testing.T, list any) []string {
	t.Helper()
	var codes []string
	for _, c := range list.([]any) {
		codes = append(codes, c.(map[string]any)["code"].(string))
	}
	return codes
}

func intAt(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s is not a number: %v", key, m[key])
	}
	return int(v)
}

func TestSettingsComeFromTheConsolesZoneSettings(t *testing.T) {
	f := newFixture(t)
	_, inZone := f.rider(f.zone, "active")
	code, out := f.send("GET", "/riders/me/settings", inZone, "", "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "RiderSettingsResponse", out)
	s := out["settings"].(map[string]any)
	// The global row: the console's defaults, the migration's numbers for the new settings.
	want := map[string]int{"timer_length_s": 90, "max_break_s": 1800, "arrive_m": 100, "almost_there_m": 350, "away_from_spot_m": 400,
		"min_phone_battery_pct": 20, "min_scooter_battery_pct": 30, "low_scooter_battery_pct": 15, "break_extension_s": 900, "not_at_gate_after_s": 30,
		"handover_code_length": 4, "verification_within_s": 86400, "scooter_capacity_qty": 24}
	for key, v := range want {
		if got := intAt(t, s, key); got != v {
			t.Errorf("%s = %d, want %d", key, got, v)
		}
	}
	if cap := s["cancel_pay_cap"].(map[string]any); cap["amount_minor"] != float64(4000) || cap["currency"] != "INR" {
		t.Errorf("cancel_pay_cap %v", cap)
	}

	// The zone's own settings win, and a break option is never longer than max_break_s.
	f.zoneSettings(f.zone, map[string]int{"timer_length_s": 120, "max_break_s": 600, "min_phone_battery_pct": 35, "cancel_pay_cap_paise": 2500,
		"arrival_perimeter_m": 120, "straight_line_speed_kmh": 22})
	_, out = f.send("GET", "/riders/me/settings", inZone, "", "")
	contracttest.Check(t, riderContract, "RiderSettingsResponse", out)
	s = out["settings"].(map[string]any)
	for key, v := range map[string]int{"timer_length_s": 120, "max_break_s": 600, "min_phone_battery_pct": 35, "arrive_m": 120, "ride_speed_kmh": 22} {
		if got := intAt(t, s, key); got != v {
			t.Errorf("after a zone change: %s = %d, want %d", key, got, v)
		}
	}
	if s["cancel_pay_cap"].(map[string]any)["amount_minor"] != float64(2500) {
		t.Errorf("cancel_pay_cap %v", s["cancel_pay_cap"])
	}
	for _, o := range s["break_options_s"].([]any) {
		if int(o.(float64)) > 600 {
			t.Errorf("break option %v is longer than max_break_s 600", o)
		}
	}
	if len(s["break_options_s"].([]any)) != 2 {
		t.Errorf("break options %v", s["break_options_s"])
	}
	// Another zone, and a rider with no zone, keep the global settings.
	other := f.id("INSERT INTO zones (name, city, status) VALUES ('Other', 'Bengaluru', 'active') RETURNING id::text")
	for _, zone := range []string{other, ""} {
		_, token := f.rider(zone, "active")
		_, out = f.send("GET", "/riders/me/settings", token, "", "")
		contracttest.Check(t, riderContract, "RiderSettingsResponse", out)
		if got := intAt(t, out["settings"].(map[string]any), "timer_length_s"); got != 90 {
			t.Errorf("zone %q: timer %d, want the global 90", zone, got)
		}
	}
}

// A settings row written without the new columns falls back to the documented numbers, and the numbers
// the order flow shares are null when the zone has none.
func TestSettingsFallBackToTheDocumentedNumbers(t *testing.T) {
	s := Settings(geo.Effective{Values: map[string]*int{}})
	for key, want := range map[string]int{"max_break_s": 1800, "away_from_spot_m": 400, "scooter_capacity_qty": 24, "min_phone_battery_pct": 20, "min_scooter_battery_pct": 30,
		"low_scooter_battery_pct": 15, "break_extension_s": 900, "not_at_gate_after_s": 30, "shift_ending_soon_before_s": 900, "stock_request_ask_before_s": 3600,
		"stock_request_reminder_before_s": 1800, "verification_within_s": 86400, "handover_code_length": 4} {
		if s[key] != want {
			t.Errorf("%s = %v, want %d", key, s[key], want)
		}
	}
	if s["cancel_pay_cap"].(map[string]any)["amount_minor"] != 4000 {
		t.Errorf("cancel_pay_cap %v", s["cancel_pay_cap"])
	}
	for _, key := range []string{"timer_length_s", "arrive_m", "almost_there_m", "low_stock_qty", "ride_speed_kmh"} {
		if s[key] != nil {
			t.Errorf("%s = %v, want null when the zone has none", key, s[key])
		}
	}
	if got := len(s["break_options_s"].([]int)); got != 4 {
		t.Errorf("break options %v", s["break_options_s"])
	}
}

func TestSettingsChoicesAreValid(t *testing.T) {
	f := newFixture(t)
	_, token := f.rider(f.zone, "active")
	_, out := f.send("GET", "/riders/me/settings", token, "", "")
	contracttest.Check(t, riderContract, "RiderSettingsResponse", out)
	s := out["settings"].(map[string]any)
	// Every problem type is one reportProblem accepts, because the app sends the code straight into it.
	valid := reportProblemTypes(t)
	problems := choiceCodes(t, s["problem_types"])
	if len(problems) == 0 {
		t.Fatal("no problem types")
	}
	for _, code := range problems {
		if !valid[code] {
			t.Errorf("problem type %q is not a reportProblem type", code)
		}
	}
	// The checklist is what startShift keeps, and every code is the short shape startShift accepts.
	if got := choiceCodes(t, s["checklist"]); strings.Join(got, ",") != "helmet,phone_battery,scooter_battery,location" {
		t.Errorf("checklist %v", got)
	}
	// Codes never repeat, and no label is empty.
	for _, key := range []string{"problem_types", "checklist", "skip_reasons"} {
		seen := map[string]bool{}
		for _, c := range s[key].([]any) {
			m := c.(map[string]any)
			if seen[m["code"].(string)] || m["label"] == "" {
				t.Errorf("%s has a repeated code or an empty label: %v", key, m)
			}
			seen[m["code"].(string)] = true
		}
	}
	// No customer or contact data in the settings: a customer with a number and a gate exists, and none of it shows.
	person := f.id("INSERT INTO people (phone) VALUES ('+919811110000') RETURNING id::text")
	customer := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	f.exec("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Secret Gate Label', 12.93, 77.62)", customer, f.zone)
	_, out = f.send("GET", "/riders/me/settings", token, "", "")
	raw, _ := json.Marshal(out)
	for _, bad := range []string{customer, "9811110000", "Secret Gate Label", "+91", "\"phone\":", "address"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("settings hold %q: %s", bad, raw)
		}
	}
}

func TestSettingsAndHelpAreReadAfterSignInOnly(t *testing.T) {
	f := newFixture(t)
	_, suspendedToken := f.rider(f.zone, "suspended")
	blocked, blockedToken := f.rider(f.zone, "active")
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM riders WHERE id = $1)", blocked)
	for _, path := range []string{"/riders/me/settings", "/riders/me/help", "/riders/me/shift-plan"} {
		if code, out := f.send("GET", path, "", "", ""); code != 401 {
			t.Errorf("%s without a token: %d %v", path, code, out)
		}
		if code, out := f.send("GET", path, suspendedToken, "", ""); code != 200 {
			t.Errorf("%s for a suspended rider: %d %v", path, code, out)
		}
		if code, out := f.send("GET", path, blockedToken, "", ""); code != 403 || out["code"] != "account_blocked" {
			t.Errorf("%s for a blocked person: %d %v", path, code, out)
		}
	}
	for _, path := range []string{"/riders/me/feedback", "/riders/me/shift-plan/reply"} {
		if code, out := f.send("POST", path, "", "", `{}`); code != 401 {
			t.Errorf("%s without a token: %d %v", path, code, out)
		}
	}
}

func TestHelpIsEmptyUntilArticlesExistAndShowsOnlyActiveOnesInOrder(t *testing.T) {
	f := newFixture(t)
	_, token := f.rider(f.zone, "active")
	code, out := f.send("GET", "/riders/me/help", token, "", "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "RiderHelp", out)
	if faq, ok := out["faq"].([]any); !ok || len(faq) != 0 {
		t.Fatalf("nothing is seeded, so faq must be an empty list: %v", out)
	}
	if n := f.count("SELECT count(*) FROM help_articles"); n != 0 {
		t.Fatalf("the migration seeded %d help articles", n)
	}
	user := f.id("INSERT INTO console_users (login_id, name, role, password_hash) VALUES ('ops.help', 'Ops', 'ops', 'x') RETURNING id::text")
	add := func(slug, q, a string, sort int, active bool) string {
		return f.id(`INSERT INTO help_articles (audience, slug, question, answer, sort, active, updated_by) VALUES ('rider', $1, $2, $3, $4, $5, $6) RETURNING id::text`,
			slug, q, a, sort, active, user)
	}
	late := add("late", "Alpha question", "Late answer", 50, true)
	early := add("early", "Zulu question", "Early answer", 10, true)
	add("hidden", "Hidden question", "Hidden answer", 1, false)
	_, out = f.send("GET", "/riders/me/help", token, "", "")
	contracttest.Check(t, riderContract, "RiderHelp", out)
	faq := out["faq"].([]any)
	if len(faq) != 2 || faq[0].(map[string]any)["id"] != early || faq[1].(map[string]any)["id"] != late {
		t.Fatalf("the active articles in sort order: %v", faq)
	}
	if first := faq[0].(map[string]any); first["question"] != "Zulu question" || first["answer"] != "Early answer" || len(first) != 3 {
		t.Errorf("an article has exactly id, question and answer: %v", first)
	}
	// The database refuses what the console refuses.
	for name, sql := range map[string]string{
		"a bad slug":      `INSERT INTO help_articles (audience, slug, question, answer, updated_by) VALUES ('rider', 'Bad Slug', 'q', 'a', $1)`,
		"an empty answer": `INSERT INTO help_articles (audience, slug, question, answer, updated_by) VALUES ('rider', 'ok', 'q', '', $1)`,
		"another reader":  `INSERT INTO help_articles (audience, slug, question, answer, updated_by) VALUES ('customer', 'ok', 'q', 'a', $1)`,
		"the same slug":   `INSERT INTO help_articles (audience, slug, question, answer, updated_by) VALUES ('rider', 'late', 'q', 'a', $1)`,
	} {
		if _, err := f.pool.Exec(context.Background(), sql, user); err == nil {
			t.Errorf("the database accepted %s", name)
		}
	}
}

func TestFeedbackIsStoredWithinTheLengthLimit(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider(f.zone, "active")
	code, out := f.send("POST", "/riders/me/feedback", token, "", `{"message":"  The map is hard to read at night.  "}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "FeedbackResponse", out)
	var stored, who string
	if err := f.pool.QueryRow(context.Background(), "SELECT message, rider_id::text FROM rider_feedback WHERE id = $1", out["feedback_id"]).Scan(&stored, &who); err != nil {
		t.Fatal(err)
	}
	if stored != "The map is hard to read at night." || who != rider {
		t.Errorf("stored %q for %s", stored, who)
	}
	// The audit row holds the length, never the words.
	var after string
	if err := f.pool.QueryRow(context.Background(), "SELECT after::text FROM audit_events WHERE action = 'rider.feedback_sent'").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after, "map") || !strings.Contains(after, "33") {
		t.Errorf("audit after: %s", after)
	}
	// 2000 characters is the limit, counted as characters and not bytes.
	if code, out := f.send("POST", "/riders/me/feedback", token, "", `{"message":"`+strings.Repeat("é", 2000)+`"}`); code != 201 {
		t.Errorf("2000 characters: %d %v", code, out)
	}
	for name, body := range map[string]string{
		"2001 characters": `{"message":"` + strings.Repeat("a", 2001) + `"}`, "empty": `{"message":""}`, "spaces only": `{"message":"   \n "}`,
		"missing": `{}`, "null": `{"message":null}`, "a number": `{"message":5}`, "a list": `{"message":["a"]}`, "not json": `message`,
		"a control character": `{"message":"a\u0000b"}`, "two objects": `{"message":"a"}{"message":"b"}`,
	} {
		if code, out := f.send("POST", "/riders/me/feedback", token, "", body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM rider_feedback"); n != 2 {
		t.Errorf("%d messages stored, want 2", n)
	}
	// A replay of the same key stores one message, and the same key with other words is refused.
	if code, first := f.send("POST", "/riders/me/feedback", token, "replay-key-0001", `{"message":"Once"}`); code != 201 {
		t.Fatalf("%d %v", code, first)
	} else if code, again := f.send("POST", "/riders/me/feedback", token, "replay-key-0001", `{"message":"Once"}`); code != 201 || again["feedback_id"] != first["feedback_id"] {
		t.Errorf("replay: %d %v, first %v", code, again, first)
	}
	if code, out := f.send("POST", "/riders/me/feedback", token, "replay-key-0001", `{"message":"Twice"}`); code != 422 {
		t.Errorf("the same key with other words: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM rider_feedback WHERE message = 'Once'"); n != 1 {
		t.Errorf("%d copies of the replayed message", n)
	}
}

// istDay is the zone's local day of a moment, as the plan calls read it.
func istDay(at time.Time) string {
	return at.In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")
}

func TestTheShiftPlanIsNullUntilOpsMakeOneAndTheReplyIsRecorded(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider(f.zone, "active")
	other, otherToken := f.rider(f.zone, "active")
	_ = other
	code, out := f.send("GET", "/riders/me/shift-plan", token, "", "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftPlanResponse", out)
	if v, ok := out["plan"]; !ok || v != nil {
		t.Fatalf("with no plan, plan is null: %v", out)
	}
	// No plan to answer: 404.
	if code, out := f.send("POST", "/riders/me/shift-plan/reply", token, "", `{"reply":"coming"}`); code != 404 || out["code"] != "not_found" {
		t.Errorf("a reply with no plan: %d %v", code, out)
	}
	user := f.id("INSERT INTO console_users (login_id, name, role, password_hash) VALUES ('ops.plan', 'Ops', 'ops', 'x') RETURNING id::text")
	plan := f.id(`INSERT INTO shift_plans (rider_id, plan_date, starts_at, ends_at, created_by) VALUES ($1, $2::date, $3, $4, $5) RETURNING id::text`,
		rider, istDay(f.now), f.now.Add(time.Hour), f.now.Add(7*time.Hour), user)
	// Yesterday's and tomorrow's plans are not today's.
	f.exec(`INSERT INTO shift_plans (rider_id, plan_date, starts_at, ends_at, created_by) VALUES ($1, $2::date, $3, $4, $5)`,
		rider, istDay(f.now.Add(-24*time.Hour)), f.now.Add(-23*time.Hour), f.now.Add(-17*time.Hour), user)
	f.exec(`INSERT INTO shift_plans (rider_id, plan_date, starts_at, ends_at, created_by) VALUES ($1, $2::date, $3, $4, $5)`,
		rider, istDay(f.now.Add(24*time.Hour)), f.now.Add(25*time.Hour), f.now.Add(31*time.Hour), user)

	_, out = f.send("GET", "/riders/me/shift-plan", token, "", "")
	contracttest.Check(t, riderContract, "ShiftPlanResponse", out)
	p := out["plan"].(map[string]any)
	if p["id"] != plan || p["date"] != istDay(f.now) || p["stock_reply"] != "pending" || p["replied_at"] != nil || p["packing"] != "not_started" || p["ready_at"] != nil || p["node_id"] != nil {
		t.Fatalf("today's plan: %v", p)
	}
	// Another rider sees no plan, and cannot answer this one.
	if _, out := f.send("GET", "/riders/me/shift-plan", otherToken, "", ""); out["plan"] != nil {
		t.Errorf("another rider sees %v", out["plan"])
	}
	if code, _ := f.send("POST", "/riders/me/shift-plan/reply", otherToken, "", `{"reply":"coming"}`); code != 404 {
		t.Errorf("another rider's reply: %d", code)
	}

	// The reply is recorded, and can change until packing starts.
	code, out = f.send("POST", "/riders/me/shift-plan/reply", token, "", `{"reply":"coming"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftPlanResponse", out)
	p = out["plan"].(map[string]any)
	if p["stock_reply"] != "coming" || p["replied_at"] == nil {
		t.Fatalf("after coming: %v", p)
	}
	first := p["replied_at"]
	f.now = f.now.Add(5 * time.Minute)
	_, out = f.send("POST", "/riders/me/shift-plan/reply", token, "", `{"reply":"coming"}`)
	if got := out["plan"].(map[string]any); got["replied_at"] != first {
		t.Errorf("the same answer again moved replied_at from %v to %v", first, got["replied_at"])
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift_plan.replied'"); n != 1 {
		t.Errorf("%d audit rows after the same answer twice, want 1", n)
	}
	_, out = f.send("POST", "/riders/me/shift-plan/reply", token, "", `{"reply":"not_today"}`)
	if got := out["plan"].(map[string]any); got["stock_reply"] != "not_today" || got["replied_at"] == first {
		t.Errorf("changed to not_today: %v", got)
	}
	var stored string
	if err := f.pool.QueryRow(context.Background(), "SELECT stock_reply FROM shift_plans WHERE id = $1", plan).Scan(&stored); err != nil || stored != "not_today" {
		t.Errorf("stored %q, %v", stored, err)
	}
	for name, body := range map[string]string{"pending": `{"reply":"pending"}`, "other": `{"reply":"maybe"}`, "empty": `{"reply":""}`, "missing": `{}`, "a number": `{"reply":1}`, "not json": `x`} {
		if code, out := f.send("POST", "/riders/me/shift-plan/reply", token, "", body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Replay: the same key and body replies the same way, and writes nothing more.
	before := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift_plan.replied'")
	code, a := f.send("POST", "/riders/me/shift-plan/reply", token, "reply-key-0001", `{"reply":"coming"}`)
	_, b := f.send("POST", "/riders/me/shift-plan/reply", token, "reply-key-0001", `{"reply":"coming"}`)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if code != 200 || string(aj) != string(bj) {
		t.Errorf("replay differs:\n%s\n%s", aj, bj)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift_plan.replied'"); n != before+1 {
		t.Errorf("replay wrote %d audit rows, want 1", n-before)
	}
	if code, out := f.send("POST", "/riders/me/shift-plan/reply", token, "reply-key-0001", `{"reply":"not_today"}`); code != 422 {
		t.Errorf("the same key with another answer: %d %v", code, out)
	}
	// Once the hub starts packing, the answer is closed (409), and nothing changes.
	f.exec("UPDATE shift_plans SET packing = 'packing', ready_at = $2 WHERE id = $1", plan, f.now.Add(time.Hour))
	if code, out := f.send("POST", "/riders/me/shift-plan/reply", token, "", `{"reply":"not_today"}`); code != 409 || out["code"] != "shift_plan_closed" {
		t.Errorf("after packing started: %d %v", code, out)
	}
	_, out = f.send("GET", "/riders/me/shift-plan", token, "", "")
	contracttest.Check(t, riderContract, "ShiftPlanResponse", out)
	if got := out["plan"].(map[string]any); got["stock_reply"] != "coming" || got["packing"] != "packing" || got["ready_at"] == nil {
		t.Errorf("a closed plan: %v", got)
	}
	// A plan is a note: replying starts, ends and schedules nothing.
	for _, table := range []string{"shifts", "outbox_events", "scheduled_tasks"} {
		if n := f.count("SELECT count(*) FROM " + table); n != 0 {
			t.Errorf("%s has %d rows after plan replies", table, n)
		}
	}
	// The database keeps a reply and its time together, and one plan for a rider and day.
	if _, err := f.pool.Exec(context.Background(), "UPDATE shift_plans SET replied_at = NULL WHERE id = $1", plan); err == nil {
		t.Error("the database accepted a reply with no time")
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO shift_plans (rider_id, plan_date, starts_at, ends_at, created_by) VALUES ($1, $2::date, $3, $4, $5)`,
		rider, istDay(f.now), f.now, f.now.Add(time.Hour), user); err == nil {
		t.Error("the database accepted a second plan for one rider and day")
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO shift_plans (rider_id, plan_date, starts_at, ends_at, created_by) VALUES ($1, '2099-01-01', $2, $3, $4)`,
		rider, f.now, f.now.Add(25*time.Hour), user); err == nil {
		t.Error("the database accepted a plan longer than 24 hours")
	}
}
