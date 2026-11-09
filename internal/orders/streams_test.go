package orders

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

type live struct {
	*trip
	custSrv, rideSrv *httptest.Server
}

func fastStreams(signer identity.Signer) *Streams {
	return &Streams{Signer: signer, AuthWait: 500 * time.Millisecond, Poll: 50 * time.Millisecond, Heartbeat: 300 * time.Millisecond,
		Position: 100 * time.Millisecond, MaxLife: time.Minute}
}

// withStreams starts an order that is paid and offered, with both streams mounted.
func withStreams(t *testing.T) *live {
	t.Helper()
	x := withRiders(newFixture(t))
	l := &live{trip: &trip{riders: x}}
	l.custSrv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		x.svc.MountCustomerStream(r, fastStreams(x.sessions.Signer))
	}))
	l.rideSrv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		x.svc.MountRiderStream(r, fastStreams(x.sessions.Signer))
	}))
	t.Cleanup(l.custSrv.Close)
	t.Cleanup(l.rideSrv.Close)
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		x.svc.MountRider(r, identity.RequireActor(x.sessions.Signer, identity.TypeRider))
		x.svc.MountTracking(r, identity.RequireActor(x.sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(srv.Close)
	x.srv = srv
	l.order = x.placed(x.chai, 1)
	_, l.tok = x.signIn(x.scooter)
	return l
}

type socket struct {
	t    *testing.T
	conn *websocket.Conn
}

func open(t *testing.T, srv *httptest.Server, path, token string) *socket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if token != "" {
		raw, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	return &socket{t, conn}
}

// next reads frames until one of this type, or the stream closes.
func (s *socket) next(kind string) (map[string]any, websocket.StatusCode) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			return nil, websocket.CloseStatus(err)
		}
		var f map[string]any
		if err := json.Unmarshal(raw, &f); err != nil {
			s.t.Fatal(err)
		}
		if f["server_time"] == nil || f["data"] == nil {
			s.t.Errorf("a frame without server_time or data: %v", f)
		}
		if f["type"] == kind {
			return f, -1
		}
	}
}

func TestTheCustomerStreamFollowsTheOrder(t *testing.T) {
	l := withStreams(t)
	s := open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	if f, code := s.next("auth_ok"); f == nil {
		t.Fatalf("no auth_ok: %d", code)
	}
	beat, _ := s.next("heartbeat")
	if beat == nil || beat["seq"] != float64(1) {
		t.Fatalf("%v", beat)
	}
	l.pay(l.order, "paid")
	up, _ := s.next("order_update")
	if up == nil || up["seq"] != float64(2) || up["data"].(map[string]any)["customer_stage"] != "looking_for_rider" {
		t.Fatalf("%v", up)
	}
	check(t, up["data"].(map[string]any), "Order")
	// A hidden change, an offer going out, sends nothing the customer can see.
	l.dispatch(l.clock.Add(6 * time.Second))
	beat, _ = s.next("heartbeat")
	if beat["seq"] != float64(2) {
		t.Errorf("a hidden change moved seq: %v", beat)
	}
	// Accepted with the minimum wait over: the customer sees the rider, then the rider's position.
	o := l.riders.offer(l.tok)
	l.offer, l.fencing = o["id"].(string), o["fencing_token"].(string)
	l.clock = l.clock.Add(10 * time.Second)
	if code, out := l.call("POST", "/offers/"+l.offer+"/accept", l.tok, l.token()); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	up, _ = s.next("order_update")
	if up == nil || up["data"].(map[string]any)["customer_stage"] != "rider_accepted" {
		t.Fatalf("%v", up)
	}
	l.ready(200)
	l.at(far, 20*time.Second)
	pos, _ := s.next("rider_position")
	if pos == nil || pos["seq"] != nil || pos["data"].(map[string]any)["lat"] != far[0] {
		t.Errorf("%v", pos)
	}
	// A cancel after the wait deadline ends the order: a last update, then 4410.
	l.clock = l.clock.Add(301 * time.Second)
	l.send("POST", "/orders/"+l.order+"/cancel", l.fixture.token, `{}`)
	up, _ = s.next("order_update")
	if up == nil || up["data"].(map[string]any)["customer_stage"] != "ended" {
		t.Fatalf("%v", up)
	}
	if _, code := s.next("never"); code != closeOrderEnded {
		t.Errorf("closed with %d", code)
	}
	// Opening a stream on an ended order closes at once.
	again := open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	if _, code := again.next("auth_ok"); code != closeOrderEnded {
		t.Errorf("%d", code)
	}
}

func TestTheRiderStreamFollowsOffersAndOrders(t *testing.T) {
	l := withStreams(t)
	s := open(t, l.rideSrv, "/riders/me/stream", l.tok)
	if f, code := s.next("auth_ok"); f == nil {
		t.Fatalf("%d", code)
	}
	l.pay(l.order, "paid")
	l.dispatch(l.clock.Add(6 * time.Second))
	f, _ := s.next("offer")
	if f == nil || f["seq"] == nil || f["data"].(map[string]any)["state"] != "open" {
		t.Fatalf("%v", f)
	}
	riderCheck(t, f["data"].(map[string]any), "Offer")
	o := f["data"].(map[string]any)
	l.offer, l.fencing = o["id"].(string), o["fencing_token"].(string)
	l.clock = l.clock.Add(10 * time.Second)
	l.call("POST", "/offers/"+l.offer+"/accept", l.tok, l.token())
	f, _ = s.next("order_state")
	if f == nil || f["data"].(map[string]any)["state"] != "RIDER_ACCEPTED" {
		t.Fatalf("%v", f)
	}
	riderCheck(t, f["data"].(map[string]any), "RiderOrder")
	if _, found := f["data"].(map[string]any)["handover_otp"]; found {
		t.Error("the rider must never see the code")
	}
}

func TestStreamsRefuseWhoShouldNotListen(t *testing.T) {
	l := withStreams(t)
	_, other := l.fixture.customer(true)
	cases := []struct {
		name, path, token string
		srv               *httptest.Server
		want              websocket.StatusCode
	}{
		{"no auth frame", "/orders/" + l.order + "/stream", "", l.custSrv, closeUnauthorized},
		{"a bad token", "/orders/" + l.order + "/stream", "nope", l.custSrv, closeUnauthorized},
		{"a rider's token on the customer stream", "/orders/" + l.order + "/stream", l.tok, l.custSrv, closeUnauthorized},
		{"another customer's order", "/orders/" + l.order + "/stream", other, l.custSrv, closeNotFound},
		{"no such order", "/orders/00000000-0000-0000-0000-000000000000/stream", l.fixture.token, l.custSrv, closeNotFound},
		{"a customer's token on the rider stream", "/riders/me/stream", l.fixture.token, l.rideSrv, closeUnauthorized},
	}
	for _, tc := range cases {
		s := open(t, tc.srv, tc.path, tc.token)
		if _, code := s.next("auth_ok"); code != tc.want {
			t.Errorf("%s: closed with %d, want %d", tc.name, code, tc.want)
		}
	}
	// A session that ends closes the stream at the next heartbeat.
	s := open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	s.next("auth_ok")
	l.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if _, code := s.next("never"); code != closeUnauthorized {
		t.Errorf("an ended session: %d", code)
	}
}

func TestATokenMustBeRenewed(t *testing.T) {
	l := withStreams(t)
	short := identity.Signer{Key: l.sessions.Signer.Key, TTL: 2 * time.Second}
	a, err := l.sessions.Signer.Verify(l.fixture.token, identity.TypeCustomer)
	if err != nil {
		t.Fatal(err)
	}
	token, err := short.Sign(a)
	if err != nil {
		t.Fatal(err)
	}
	s := open(t, l.custSrv, "/orders/"+l.order+"/stream", token)
	if f, code := s.next("auth_ok"); f == nil {
		t.Fatalf("the short token was refused at sign-in: %d", code)
	}
	if _, code := s.next("never"); code != closeUnauthorized {
		t.Errorf("a token not renewed: %d", code)
	}
	// Renewing with a token for another person closes the stream.
	s = open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	s.next("auth_ok")
	_, other := l.fixture.customer(true)
	raw, _ := json.Marshal(map[string]string{"type": "auth", "token": other})
	_ = s.conn.Write(context.Background(), websocket.MessageText, raw)
	if _, code := s.next("never"); code != closeUnauthorized {
		t.Errorf("a renewal for someone else: %d", code)
	}
}

func TestThePositionStaysHiddenDuringTheMinimumWait(t *testing.T) {
	l := withStreams(t)
	l.pay(l.order, "paid")
	l.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", l.order, time.Now().Add(time.Hour))
	l.dispatch(l.clock.Add(6 * time.Second))
	o := l.riders.offer(l.tok)
	l.offer, l.fencing = o["id"].(string), o["fencing_token"].(string)
	l.clock = l.clock.Add(10 * time.Second)
	l.call("POST", "/offers/"+l.offer+"/accept", l.tok, l.token())
	l.ready(200)
	s := open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	s.next("auth_ok")
	l.at(far, 20*time.Second)
	// Several position ticks pass, then a heartbeat: no position may come before it.
	time.Sleep(400 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		if f["type"] == "rider_position" {
			t.Fatal("the rider's position was sent during the minimum wait")
		}
		if f["type"] == "heartbeat" {
			break
		}
	}
}

func TestTheStreamStartsWithTheCurrentState(t *testing.T) {
	l := withStreams(t)
	s := open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	s.next("auth_ok")
	up, _ := s.next("order_update")
	if up == nil || up["seq"] != float64(1) || up["data"].(map[string]any)["customer_stage"] != "payment" {
		t.Errorf("%v", up)
	}
}

func TestAShiftChangeSendsNoStaleEvent(t *testing.T) {
	l := withStreams(t)
	l.pay(l.order, "paid")
	l.dispatch(l.clock.Add(6 * time.Second))
	s := open(t, l.rideSrv, "/riders/me/stream", l.tok)
	s.next("auth_ok")
	if f, _ := s.next("offer"); f == nil {
		t.Fatal("no live offer at connect")
	}
	// Something else the rider sees changes, such as a stock scan.
	l.exec("UPDATE rider_state SET seq = seq + 1")
	want := float64(l.count("SELECT seq::int FROM rider_state"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		if f["type"] == "offer" || f["type"] == "order_state" {
			t.Fatalf("an old offer was sent for a new seq: %v", f)
		}
		if f["type"] == "heartbeat" && f["seq"] == want {
			break
		}
	}
}

func TestAHandedOverOrderEndsTheStream(t *testing.T) {
	l := withStreams(t)
	l.pay(l.order, "paid")
	l.dispatch(l.clock.Add(6 * time.Second))
	o := l.riders.offer(l.tok)
	l.offer, l.fencing = o["id"].(string), o["fencing_token"].(string)
	l.clock = l.clock.Add(10 * time.Second)
	l.call("POST", "/offers/"+l.offer+"/accept", l.tok, l.token())
	l.ready(200)
	s := open(t, l.custSrv, "/orders/"+l.order+"/stream", l.fixture.token)
	s.next("auth_ok")
	l.at(atGate, 20*time.Second)
	l.at(atGateToo, 3*time.Second)
	body := `{"otp":"` + l.otp() + `","quantities":[{"item_id":"` + l.chai + `","qty":1}],"client_time":"` + l.clock.UTC().Format(time.RFC3339) + `"}`
	if code, out := l.call("POST", "/orders/"+l.order+"/handover", l.tok, body); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for {
		f, code := s.next("order_update")
		if f == nil {
			if code != closeOrderEnded {
				t.Errorf("closed with %d", code)
			}
			break
		}
		if f["data"].(map[string]any)["customer_stage"] == "handed_over" {
			if _, code := s.next("never"); code != closeOrderEnded {
				t.Errorf("a handed-over order's stream closed with %d", code)
			}
			break
		}
	}
}
