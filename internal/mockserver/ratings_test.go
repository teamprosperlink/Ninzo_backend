package mockserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// Issue 265, part A: customer ratings in the customer, rider and console mocks.

func init() {
	if _, taken := sampleBodies["rateOrder"]; taken {
		panic("a sample body would replace another one: rateOrder")
	}
	sampleBodies["rateOrder"] = `{"stars":4,"reason":"late"}`
}

var ratingCalls int

// ratingCall sends one request to an app's mock. A write carries a fresh Idempotency-Key unless one is given.
func ratingCall(t *testing.T, srv *httptest.Server, prefix, method, path, scenario, body, idem string) (int, map[string]any, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+prefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		if idem == "" {
			ratingCalls++
			idem = fmt.Sprintf("rating-fresh-%04d", ratingCalls)
		}
		req.Header.Set("Idempotency-Key", idem)
	}
	if scenario != "" {
		req.Header.Set(scenarioHeader, scenario)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: not JSON: %s", method, path, raw)
	}
	return resp.StatusCode, out, resp.Header
}

func orderIn(out map[string]any) map[string]any {
	order, _ := out["order"].(map[string]any)
	return order
}

func TestRateOrderAcceptsAHandedOverOrder(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out, _ := ratingCall(t, srv, "/customer/v1", "POST", "/orders/ord_mock0001/rating", "handed_over", `{"stars":4,"reason":"late"}`, "")
	rating, _ := orderIn(out)["rating"].(map[string]any)
	if status != 200 || rating["stars"] != float64(4) || rating["reason"] != "late" || rating["rated_at"] == nil || orderIn(out)["can_rate"] != false {
		t.Errorf("rated: %d %v", status, out)
	}
	// The reason is optional, and then it is null.
	status, out, _ = ratingCall(t, srv, "/customer/v1", "POST", "/orders/ord_mock0001/rating", "handed_over", `{"stars":5}`, "")
	rating, _ = orderIn(out)["rating"].(map[string]any)
	if status != 200 || rating["stars"] != float64(5) || rating["reason"] != nil {
		t.Errorf("no reason: %d %v", status, out)
	}
	// The order is the customer's own view, so it holds no rider id.
	for _, hidden := range []string{"rider_id", "customer_id", "rider_rating"} {
		if _, has := orderIn(out)[hidden]; has {
			t.Errorf("the order shows %s", hidden)
		}
	}
}

func TestRateOrderRefusals(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	const path = "/orders/ord_mock0001/rating"
	for _, tc := range []struct {
		name, path, scenario, body string
		status                     int
		code                       string
	}{
		{"not handed over (the default order is still looking)", path, "", `{"stars":4}`, 409, "not_delivered"},
		{"not handed over (timer running)", path, "timer_running", `{"stars":4}`, 409, "not_delivered"},
		{"ended without delivery", path, "cancelled", `{"stars":4}`, 409, "not_delivered"},
		{"already rated", path, "already_rated", `{"stars":4}`, 409, "already_rated"},
		{"window closed", path, "rating_window_closed", `{"stars":4}`, 409, "rating_window_closed"},
		{"another customer's order", "/orders/ord_missing/rating", "handed_over", `{"stars":4}`, 404, "not_found"},
		{"stars 0", path, "handed_over", `{"stars":0}`, 400, "invalid_request"},
		{"stars 6", path, "handed_over", `{"stars":6}`, 400, "invalid_request"},
		{"stars 4.5", path, "handed_over", `{"stars":4.5}`, 400, "invalid_request"},
		{"stars as text", path, "handed_over", `{"stars":"4"}`, 400, "invalid_request"},
		{"no stars", path, "handed_over", `{"reason":"late"}`, 400, "invalid_request"},
		{"reason outside the list", path, "handed_over", `{"stars":2,"reason":"smelly"}`, 400, "invalid_request"},
		{"reason null", path, "handed_over", `{"stars":2,"reason":null}`, 400, "invalid_request"},
		{"free text", path, "handed_over", `{"stars":2,"comment":"slow rider"}`, 400, "invalid_request"},
	} {
		status, out, _ := ratingCall(t, srv, "/customer/v1", "POST", tc.path, tc.scenario, tc.body, "")
		if status != tc.status || out["code"] != tc.code {
			t.Errorf("%s: got %d %v, want %d %s", tc.name, status, out["code"], tc.status, tc.code)
		}
	}
}

func TestRateOrderReplaysAndRefusesAChangedBody(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	const path = "/orders/ord_mock0001/rating"
	first, a, _ := ratingCall(t, srv, "/customer/v1", "POST", path, "handed_over", `{"stars":3}`, "rating-replay-0001")
	second, b, header := ratingCall(t, srv, "/customer/v1", "POST", path, "handed_over", `{"stars":3}`, "rating-replay-0001")
	if first != 200 || second != 200 || header.Get(replayHeader) != "true" || fmt.Sprint(a["order"]) != fmt.Sprint(b["order"]) {
		t.Errorf("replay: %d %d %q", first, second, header.Get(replayHeader))
	}
	status, out, _ := ratingCall(t, srv, "/customer/v1", "POST", path, "handed_over", `{"stars":1}`, "rating-replay-0001")
	if status != 422 || out["code"] != "idempotency_key_reused" {
		t.Errorf("changed body: %d %v", status, out)
	}
}

func TestOrderShowsRatingAndCanRate(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, tc := range []struct {
		scenario string
		rated    bool
		canRate  bool
	}{
		{"", false, false}, {"timer_running", false, false}, {"cancelled", false, false},
		{"handed_over", false, true}, {"already_rated", true, false}, {"rating_window_closed", false, false},
	} {
		_, out, _ := ratingCall(t, srv, "/customer/v1", "GET", "/orders/ord_mock0001", tc.scenario, "", "")
		order := orderIn(out)
		rating, has := order["rating"]
		canRate, isBool := order["can_rate"].(bool)
		if !has || !isBool || (rating != nil) != tc.rated || canRate != tc.canRate {
			t.Errorf("%q: rating %v, can_rate %v", tc.scenario, rating, order["can_rate"])
		}
	}
	// The list shows the same two fields on every order.
	_, out, _ := ratingCall(t, srv, "/customer/v1", "GET", "/orders", "", "", "")
	for _, raw := range out["orders"].([]any) {
		o := raw.(map[string]any)
		if _, has := o["rating"]; !has || o["can_rate"] == nil {
			t.Errorf("listed order lacks rating or can_rate: %v", o)
		}
	}
	// An order rated in the sample keeps its reason and a time.
	_, out, _ = ratingCall(t, srv, "/customer/v1", "GET", "/orders/ord_mock0001", "already_rated", "", "")
	if r, _ := orderIn(out)["rating"].(map[string]any); r["stars"] != float64(4) || r["reason"] != "late" || r["rated_at"] == nil {
		t.Errorf("the sample rating: %v", r)
	}
}

func TestRiderSeesOnlyTheAverageAndCount(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, out, _ := ratingCall(t, srv, "/rider/v1", "GET", "/riders/me", "", "", "")
	rating, _ := out["rider"].(map[string]any)["rating"].(map[string]any)
	if rating["average"] != 4.6 || rating["count"] != float64(38) || len(rating) != 2 {
		t.Errorf("rating = %v, want only average 4.6 and count 38", rating)
	}
	_, out, _ = ratingCall(t, srv, "/rider/v1", "GET", "/riders/me", "rider_unrated", "", "")
	rating, _ = out["rider"].(map[string]any)["rating"].(map[string]any)
	if v, has := rating["average"]; !has || v != nil || rating["count"] != float64(0) {
		t.Errorf("unrated rider: %v", rating)
	}
}

func TestListRatingsFollowsOrdersRead(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for scenario, allowed := range map[string]bool{"": true, "role_ops": true, "role_support": true, "role_hub_manager": false, "no_permission": false} {
		status, out := consoleCall(t, srv, "GET", "/admin/ratings", "", scenario)
		if allowed && status != 200 {
			t.Errorf("as %q: %d %v", scenario, status, out)
		}
		if !allowed && (status != 403 || out["missing_permission"] != "orders.read") {
			t.Errorf("as %q: %d %v, want 403 missing_permission orders.read", scenario, status, out)
		}
	}
}

func ratingIDs(out map[string]any) string {
	var ids []string
	rows, _ := out["ratings"].([]any)
	for _, raw := range rows {
		ids = append(ids, raw.(map[string]any)["order_id"].(string))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestListRatingsFilters(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, tc := range []struct{ query, want string }{
		{"", "ord_mock0001,ord_mock0007,ord_mock0008"},
		{"?stars=2", "ord_mock0008"},
		{"?stars=5", "ord_mock0007"},
		{"?rider_id=rdr_mock0001", "ord_mock0001,ord_mock0008"},
		{"?zone_id=zone_mock02", "ord_mock0008"},
		{"?rider_id=rdr_mock0001&zone_id=zone_mock01", "ord_mock0001"},
		{"?stars=3", ""},
		{"?limit=10", "ord_mock0001,ord_mock0007,ord_mock0008"},
	} {
		status, out := consoleCall(t, srv, "GET", "/admin/ratings"+tc.query, "", "")
		if status != 200 || ratingIDs(out) != tc.want {
			t.Errorf("%q: %d %v, want %q", tc.query, status, ratingIDs(out), tc.want)
		}
	}
	// A time filter: from keeps newer rows, to keeps older rows.
	_, all := consoleCall(t, srv, "GET", "/admin/ratings", "", "")
	rows := all["ratings"].([]any)
	middle := rows[1].(map[string]any)["rated_at"].(string)
	if _, out := consoleCall(t, srv, "GET", "/admin/ratings?from="+middle, "", ""); ratingIDs(out) != "ord_mock0001,ord_mock0007" {
		t.Errorf("from: %v", ratingIDs(out))
	}
	if _, out := consoleCall(t, srv, "GET", "/admin/ratings?to="+middle, "", ""); ratingIDs(out) != "ord_mock0008" {
		t.Errorf("to: %v", ratingIDs(out))
	}
	for _, bad := range []string{"?stars=0", "?stars=6", "?stars=x", "?from=yesterday", "?to=1", "?limit=0", "?limit=101"} {
		if status, out := consoleCall(t, srv, "GET", "/admin/ratings"+bad, "", ""); status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%q: %d %v", bad, status, out)
		}
	}
	if _, out := consoleCall(t, srv, "GET", "/admin/ratings", "", "ratings_empty"); len(out["ratings"].([]any)) != 0 || out["next_cursor"] != nil {
		t.Errorf("ratings_empty: %v", out)
	}
}

func TestRatingRowsHoldNoCustomerDetails(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, out := consoleCall(t, srv, "GET", "/admin/ratings", "", "")
	want := "order_id,order_number,rated_at,reason,rider,stars,zone_id"
	for _, raw := range out["ratings"].([]any) {
		row := raw.(map[string]any)
		var keys []string
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != want {
			t.Errorf("row fields %v, want %s", keys, want)
		}
		if reason, ok := row["reason"].(string); ok && !ratingReasons[reason] {
			t.Errorf("reason %q is outside the fixed list", reason)
		}
	}
}

func TestTimelineAndRiderReviewShowRatings(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, tl := consoleCall(t, srv, "GET", "/admin/orders/ord_mock0001/timeline", "", "")
	found := false
	for _, raw := range tl["events"].([]any) {
		e := raw.(map[string]any)
		if e["source"] == "rating" && e["kind"] == "rating.given" {
			found = true
			detail := e["detail"].(map[string]any)
			if detail["stars"] != float64(4) || detail["reason"] != "late" || len(detail) != 2 {
				t.Errorf("rating detail %v", detail)
			}
		}
	}
	if !found {
		t.Errorf("the timeline has no rating.given event: %v", tl["events"])
	}
	_, rev := consoleCall(t, srv, "GET", "/admin/riders/review", "", "")
	first := rev["riders"].([]any)[0].(map[string]any)["rating"].(map[string]any)
	if v, has := first["average"]; !has || v != nil || first["count"] != float64(0) {
		t.Errorf("an applicant's rating: %v", first)
	}
	_, rev = consoleCall(t, srv, "GET", "/admin/riders/review", "", "rider_verification_failed")
	riders := rev["riders"].([]any)
	last := riders[len(riders)-1].(map[string]any)["rating"].(map[string]any)
	if len(riders) != 2 || last["average"] != 3.8 || last["count"] != float64(12) {
		t.Errorf("an active rider's rating: %v", riders)
	}
}
