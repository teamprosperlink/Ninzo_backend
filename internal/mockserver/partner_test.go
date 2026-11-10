package mockserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
	"github.com/teamprosperlink/Ninzo_backend/internal/contractbundle"
)

// Sample bodies for the partner operations that the customer and rider ones do not cover.
func init() {
	for id, body := range map[string]string{
		"scanHubLot":          `{"lot_code":"LOT-B-0101","scanned_at":"2026-11-10T10:00:00Z"}`,
		"unscanHubLot":        `{"lot_code":"LOT-B-0101"}`,
		"recordShortfall":     `{"action":"partial","lines":[{"item_id":"item_chai","qty_missing":6}]}`,
		"markHubRequestReady": `{"lot_numbers":["LOT-B-0101","LOT-C-0201"]}`,
		"quarantineLot":       `{"reason":"temperature","note":"9 C at the bench"}`,
	} {
		if _, taken := sampleBodies[id]; taken {
			panic("a partner sample body would replace another app's: " + id)
		}
		sampleBodies[id] = body
	}
}

func partnerContract(t *testing.T) *apiContract {
	return loadContract(t, "partner", "partner.openapi.yaml", "/partner/v1")
}

// partnerBodies replaces the shared sendEventBatch body with hub events for the partner calls.
func partnerRequest(t *testing.T, srv *httptest.Server, c *apiContract, o op, scenario, key string) (int, []byte) {
	t.Helper()
	if o.id == "sendEventBatch" {
		saved := sampleBodies[o.id]
		sampleBodies[o.id] = `{"events":[{"event_id":"evt-hub-0001","type":"scan","device_time":"2026-11-10T10:00:00Z","payload":{"request_id":"rst_mock01","lot_code":"LOT-B-0101","scanned_at":"2026-11-10T10:00:00Z"}}]}`
		defer func() { sampleBodies[o.id] = saved }()
	}
	status, body, _ := doRequest(t, srv, c, o, scenario, key)
	return status, body
}

func TestEveryPartnerOperationAndScenarioMatchesTheContract(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	names := []string{""}
	for name := range partnerScenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	checked, ok200 := 0, 0
	for _, o := range c.operations() {
		for i, scenario := range names {
			status, body := partnerRequest(t, srv, c, o, scenario, fmt.Sprintf("key-%s-%d", o.id, i))
			if status == http.StatusNotFound && !isDeclaredNotFound(body) {
				t.Errorf("%s: no route for this operation", o.id)
				continue
			}
			ref := c.responseRef(t, o, status)
			if ref == "" {
				continue
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%s: not JSON: %v", o.id, err)
			}
			if err := c.schema(t, ref).Validate(inst); err != nil {
				t.Errorf("%s, scenario %q, status %d: %v", o.id, scenario, status, err)
			}
			checked++
			if status < 300 {
				ok200++
			}
		}
	}
	if checked < 400 || ok200 < 300 {
		t.Errorf("only %d responses checked (%d successes), so this test proves little", checked, ok200)
	}
	t.Logf("%d partner responses validated against the contract", checked)
}

func TestEveryPartnerScenarioChangesSomething(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	volatile := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.]+Z`)
	stream := func(name string) string {
		return fmt.Sprint(len(hubStoryFor(name, time.Unix(0, 0)).events))
	}
	base := map[string]string{}
	ops := c.operations()
	for _, o := range ops {
		status, body := partnerRequest(t, srv, c, o, "", "base-"+o.id)
		base[o.id] = fmt.Sprint(status) + volatile.ReplaceAllString(string(body), "T")
	}
	for name := range partnerScenarios {
		if name == "socket_drop" || name == "token_expiring" {
			continue // Checked by the stream tests.
		}
		changed := stream(name) != stream("")
		for _, o := range ops {
			status, body := partnerRequest(t, srv, c, o, name, "s-"+name+"-"+o.id)
			if fmt.Sprint(status)+volatile.ReplaceAllString(string(body), "T") != base[o.id] {
				changed = true
				break
			}
		}
		if !changed {
			t.Errorf("partner scenario %q changes nothing", name)
		}
	}
}

// TestHubGetAndStreamTellTheSameStory checks the queue GET returns is what the live events add up to.
func TestHubGetAndStreamTellTheSameStory(t *testing.T) {
	now := time.Now()
	names := []string{""}
	for name := range partnerScenarios {
		names = append(names, name)
	}
	for _, name := range names {
		story := hubStoryFor(name, now)
		last := 0
		latest := map[string]map[string]any{}
		for _, e := range story.events {
			if e.seq != last+1 {
				t.Errorf("%q: seq jumped from %d to %d", name, last, e.seq)
			}
			last = e.seq
			r := e.data.(map[string]any)
			if r["seq"] != e.seq {
				t.Errorf("%q: the request's seq %v is not the event's %d", name, r["seq"], e.seq)
			}
			latest[r["id"].(string)] = r
		}
		if story.seq != last {
			t.Errorf("%q: the hub seq is %d, and the stream ends at %d", name, story.seq, last)
		}
		open := 0
		for _, r := range latest {
			if r["state"] != "cancelled" {
				open++
			}
		}
		if len(story.requests) != open {
			t.Errorf("%q: GET lists %d requests, the stream leaves %d open", name, len(story.requests), open)
		}
		for _, r := range story.requests {
			if fmt.Sprint(r) != fmt.Sprint(latest[r["id"].(string)]) {
				t.Errorf("%q: GET shows an older version of %v", name, r["id"])
			}
		}
	}
}

func TestTheDefaultQueueHasThreeRequests(t *testing.T) {
	story := hubStoryFor("", time.Now())
	states := []string{}
	for _, r := range story.requests {
		states = append(states, r["state"].(string))
	}
	if strings.Join(states, ",") != "confirmed,packing,ready" {
		t.Errorf("the KT fixture is a queue of three: new, packing and ready; got %v", states)
	}
}

func TestPickListIsSoonestExpiryFirst(t *testing.T) {
	for _, line := range pickList("rst_mock01", "", time.Now())["lines"].([]map[string]any) {
		lots := line["lots"].([]map[string]any)
		for i := 1; i < len(lots); i++ {
			if lots[i]["expires_at"].(string) < lots[i-1]["expires_at"].(string) {
				t.Errorf("%s: lot %v expires before %v but is listed after it", line["name"], lots[i]["lot_code"], lots[i-1]["lot_code"])
			}
		}
	}
}

func TestPartnerBrunoBodiesAreAccepted(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	raw, err := contracts.Dist.ReadFile("dist/partner.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, err := contractbundle.Bruno("partner", raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range c.operations() {
		content, ok := files["partner/"+o.id+".bru"]
		if !ok {
			t.Errorf("no Bruno request for %s", o.id)
			continue
		}
		if o.node["requestBody"] == nil {
			continue
		}
		_, after, found := strings.Cut(content, "body:json {\n")
		if !found {
			t.Errorf("%s has no body", o.id)
			continue
		}
		body := strings.TrimSuffix(strings.TrimSpace(after), "}")
		if !json.Valid([]byte(body)) {
			t.Errorf("%s: body is not JSON: %s", o.id, body)
			continue
		}
		saved := sampleBodies[o.id]
		sampleBodies[o.id] = body
		status, data, _ := doRequest(t, srv, c, o, "", "bruno-"+o.id)
		sampleBodies[o.id] = saved
		if status == 400 {
			t.Errorf("%s: mock refused the Bruno body: %s", o.id, data)
		}
	}
}

func TestPartnerDocsAndFilesAreServed(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, path := range []string{"/docs/partner", "/contracts/partner.openapi.yaml", "/contracts/partner.stream.asyncapi.yaml"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	resp, err := srv.Client().Get(srv.URL + "/scenarios")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	if p, _ := listed["partner"].(map[string]any); len(p) != len(partnerScenarios) {
		t.Errorf("/scenarios lists %d partner scenarios, want %d", len(p), len(partnerScenarios))
	}
}

func TestOnlyAHubLeadQuarantines(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	for _, o := range c.operations() {
		if o.id != "quarantineLot" {
			continue
		}
		if status, _ := partnerRequest(t, srv, c, o, "", "lead-quarantine"); status != 200 {
			t.Errorf("a hub lead: %d", status)
		}
		if status, body := partnerRequest(t, srv, c, o, "not_hub_lead", "staff-quarantine"); status != 403 || !bytes.Contains(body, []byte("lot.quarantine")) {
			t.Errorf("hub staff: %d %s", status, body)
		}
		return
	}
	t.Fatal("no quarantineLot operation")
}

func TestReadyTwiceChangesNothing(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	for _, o := range c.operations() {
		if o.id != "markHubRequestReady" {
			continue
		}
		_, first := partnerRequest(t, srv, c, o, "already_ready", "ready-first")
		_, again := partnerRequest(t, srv, c, o, "already_ready", "ready-again")
		strip := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.]+Z`)
		if strip.ReplaceAllString(string(first), "") != strip.ReplaceAllString(string(again), "") || !bytes.Contains(first, []byte(`"state":"ready"`)) {
			t.Errorf("ready twice must return the same ready request:\n%s\n%s", first, again)
		}
		// Ready on a request already ready changes nothing, so its seq is the one GET shows.
		r, _ := hubRequestFor(&call{scenario: "already_ready", now: time.Now()}, "mock_id")
		if !bytes.Contains(first, []byte(fmt.Sprintf(`"seq":%d,`, r["seq"]))) {
			t.Errorf("ready again raised the seq past %v: %s", r["seq"], first)
		}
		return
	}
	t.Fatal("no markHubRequestReady operation")
}

func TestAnEventWithNoTypeIsRejectedNotDropped(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/partner/v1/hub/events", strings.NewReader(`{"events":[{"event_id":"evt-no-type-1","payload":{}}]}`))
	req.Header.Set("Authorization", "Bearer mock-access-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || len(out["rejected"].([]any)) != 1 {
		t.Errorf("%d %v", resp.StatusCode, out)
	}
}

func TestADuplicateScanChangesNothing(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	for _, o := range c.operations() {
		if o.id != "scanHubLot" {
			continue
		}
		_, body := partnerRequest(t, srv, c, o, "duplicate_scan", "duplicate-scan-1")
		r, _ := hubRequestFor(&call{scenario: "duplicate_scan", now: time.Now()}, "mock_id")
		if !bytes.Contains(body, []byte(fmt.Sprintf(`"seq":%d,`, r["seq"]))) || !bytes.Contains(body, []byte(`"duplicate":true`)) {
			t.Errorf("a duplicate scan raised the seq or changed the request: %s", body)
		}
		return
	}
	t.Fatal("no scanHubLot operation")
}

func TestATempAlertNamesTheLot(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := partnerContract(t)
	for _, o := range c.operations() {
		if o.id != "scanHubLot" {
			continue
		}
		status, body := partnerRequest(t, srv, c, o, "temp_alert", "temp-alert-1")
		if status != 409 || !bytes.Contains(body, []byte(`"lot_id":"lot_LOT-C-0203"`)) {
			t.Errorf("the app needs the lot_id to quarantine: %d %s", status, body)
		}
		return
	}
	t.Fatal("no scanHubLot operation")
}
