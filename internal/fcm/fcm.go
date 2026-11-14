// Package fcm sends pushes through Firebase Cloud Messaging, HTTP v1 (issue 252). Sender
// implements orders.Pusher, so it takes the place of the log stand-in with nothing else changed.
//
// A push carries only what the app needs to open a screen: the outbox payload (ids, stage, kind)
// as data, and a fixed title and body chosen here by its stage or kind. Never names, phones or
// addresses. Rider offers go at high priority (time-sensitive on iOS); the rest at normal. A newer
// push about the same order, or the same kind, replaces the older one on the device.
package fcm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Scope is the OAuth scope for sending.
const Scope = "https://www.googleapis.com/auth/firebase.messaging"

// Devices is what the sender needs to drop a token FCM no longer knows.
type Devices interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Sender sends pushes for one Firebase project with a service account.
type Sender struct {
	project, email, tokenURI string
	key                      *rsa.PrivateKey
	// Devices is where a dead token is deleted; set it before the first push.
	Devices Devices

	// BaseURL is FCM's address; tests point it at a fake server.
	BaseURL string
	Client  *http.Client
	Now     func() time.Time

	mu      sync.Mutex
	access  string
	expires time.Time
}

type account struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// New reads a service account key (the JSON file Firebase gives). Every field it needs must be
// there, and the token address must be Google's https one or a test server's.
func New(serviceAccount []byte) (*Sender, error) {
	var a account
	if err := json.Unmarshal(serviceAccount, &a); err != nil {
		return nil, errors.New("fcm: the service account is not JSON")
	}
	if a.Type != "service_account" || a.ProjectID == "" || a.ClientEmail == "" || a.PrivateKey == "" || a.TokenURI == "" {
		return nil, errors.New("fcm: the service account needs type service_account, project_id, client_email, private_key and token_uri")
	}
	u, err := url.Parse(a.TokenURI)
	if err != nil || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return nil, errors.New("fcm: token_uri must be https")
	}
	block, _ := pem.Decode([]byte(a.PrivateKey))
	if block == nil {
		return nil, errors.New("fcm: private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return nil, errors.New("fcm: private_key is not an RSA key")
		}
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("fcm: private_key is not an RSA key")
	}
	return &Sender{project: a.ProjectID, email: a.ClientEmail, tokenURI: a.TokenURI, key: key,
		BaseURL: "https://fcm.googleapis.com", Client: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}, nil
}

// FromEnv builds a sender from FCM_SERVICE_ACCOUNT_JSON. Unset gives nil and no error, so the log
// stand-in stays; set but unusable is an error, so the API does not start half configured.
func FromEnv(getenv func(string) string) (*Sender, error) {
	raw := strings.TrimSpace(getenv("FCM_SERVICE_ACCOUNT_JSON"))
	if raw == "" {
		return nil, nil
	}
	return New([]byte(raw))
}

// Project is the Firebase project pushes go to.
func (s *Sender) Project() string { return s.project }

// token returns an access token, getting a new one a minute before the old one expires.
func (s *Sender) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if s.access != "" && now.Before(s.expires.Add(-time.Minute)) {
		return s.access, nil
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iss": s.email, "scope": Scope, "aud": s.tokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	unsigned := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {unsigned + "." + enc.EncodeToString(sig)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm: token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("fcm: token refused (%d)", resp.StatusCode)
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	s.access, s.expires = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	return s.access, nil
}

func (s *Sender) forget() {
	s.mu.Lock()
	s.access = ""
	s.mu.Unlock()
}

// Push implements orders.Pusher: one message to one device. A token FCM reports as unregistered is
// deleted and counts as sent; any other failure is returned, so the outbox tries again.
func (s *Sender) Push(ctx context.Context, platform, token, topic string, payload map[string]any) error {
	access, err := s.token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"message": Message(platform, token, topic, payload)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/v1/projects/"+url.PathEscape(s.project)+"/messages:send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusNotFound || unregistered(reply) {
		if s.Devices == nil {
			return errors.New("fcm: no device store to drop an unregistered token")
		}
		if _, err := s.Devices.Exec(ctx, "DELETE FROM devices WHERE push_token = $1", token); err != nil {
			return err
		}
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.forget()
	}
	return fmt.Errorf("fcm: send refused (%d)", resp.StatusCode)
}

// unregistered says whether an FCM error reply names the UNREGISTERED error code.
func unregistered(reply []byte) bool {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(reply, &e) != nil {
		return false
	}
	for _, d := range e.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			return true
		}
	}
	return false
}

type text struct{ title, body string }

// Texts shown on the device, by topic and then the payload's kind or stage. They name nobody.
var texts = map[string]map[string]text{
	"push.customer": {
		"payment_failed":   {"Payment didn't go through", "Open Ninzo to try again."},
		"no_scooter":       {"No scooter could take your order", "Your refund is on its way."},
		"rider_accepted":   {"Your order is on its way", "A rider has your order."},
		"timer_running":    {"Your rider is close", "The timer has started."},
		"arrived":          {"Your rider has arrived", "Meet them at your gate."},
		"rider_waiting":    {"Your rider is waiting at the gate", "Meet them at your gate."},
		"handed_over":      {"Enjoy your food", "Your order is complete."},
		"ended":            {"Your order has ended", "Open Ninzo for details."},
		"available":        {"Food is available again", "Your gate is in reach now."},
		"ticket_reply":     {"Support replied", "Open Ninzo to read the reply."},
		"refund_processed": {"Refund processed", "Your refund has been sent to your payment method."},
	},
	"push.rider": {
		"offer":      {"New order", "Open Ninzo to see it."},
		"relocation": {"Move suggestion", "More orders are expected at another spot."},
		"restock":    {"Restock update", "Open Ninzo for details."},
		"shift":      {"Your break is over", "Offers can come to you again."},
	},
}

var fallback = text{"Ninzo", "You have an update."}

// what is the payload's kind if it has one, else its stage.
func what(payload map[string]any) string {
	if k, _ := payload["kind"].(string); k != "" {
		return k
	}
	s, _ := payload["stage"].(string)
	return s
}

// Message builds the FCM v1 message for one device.
func Message(platform, token, topic string, payload map[string]any) map[string]any {
	kind := what(payload)
	t, ok := texts[topic][kind]
	if !ok {
		t = fallback
	}
	data := map[string]string{"topic": topic}
	for k, v := range payload {
		switch v := v.(type) {
		case string:
			data[k] = v
		case nil:
		default:
			b, _ := json.Marshal(v)
			data[k] = string(b)
		}
	}
	urgent := topic == "push.rider" && kind == "offer"
	collapse := topic + ":" + kind
	if id, _ := payload["order_id"].(string); id != "" {
		collapse = "order:" + id
	}
	if len(collapse) > 64 { // apns-collapse-id is at most 64 bytes
		sum := sha256.Sum256([]byte(collapse))
		collapse = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	android := map[string]any{"priority": "normal", "collapse_key": collapse}
	aps := map[string]any{}
	headers := map[string]string{"apns-priority": "5", "apns-collapse-id": collapse}
	if urgent {
		android["priority"] = "high"
		headers["apns-priority"] = "10"
		aps["interruption-level"] = "time-sensitive"
		aps["sound"] = "default"
	}
	m := map[string]any{
		"token":        token,
		"notification": map[string]string{"title": t.title, "body": t.body},
		"data":         data,
	}
	switch platform {
	case "android":
		m["android"] = android
	case "ios":
		m["apns"] = map[string]any{"headers": headers, "payload": map[string]any{"aps": aps}}
	}
	return m
}
