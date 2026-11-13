package orders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RazorpayAPI is Razorpay's API base URL.
const RazorpayAPI = "https://api.razorpay.com/v1"

// Razorpay takes money through Razorpay (issue 237). Test keys take no real money; live keys do.
// The key secret signs every API call; the webhook secret checks every webhook. Neither is ever
// logged or returned.
type Razorpay struct {
	keyID, keySecret, webhookSecret string
	// BaseURL is RazorpayAPI unless a test points it at a fake server.
	BaseURL string
	Client  *http.Client
}

// NewRazorpay returns Razorpay when RAZORPAY_KEY_ID and RAZORPAY_KEY_SECRET are both set, nil
// when neither is (the test payment is used), and an error when only one is, or when the webhook
// secret is missing: without it no payment could ever be confirmed.
func NewRazorpay(getenv func(string) string) (*Razorpay, error) {
	id, secret, hook := strings.TrimSpace(getenv("RAZORPAY_KEY_ID")), strings.TrimSpace(getenv("RAZORPAY_KEY_SECRET")),
		strings.TrimSpace(getenv("RAZORPAY_WEBHOOK_SECRET"))
	switch {
	case id == "" && secret == "":
		return nil, nil
	case id == "" || secret == "":
		return nil, errors.New("payments: set both RAZORPAY_KEY_ID and RAZORPAY_KEY_SECRET, or neither")
	case hook == "":
		return nil, errors.New("payments: RAZORPAY_WEBHOOK_SECRET is required with Razorpay keys")
	case !strings.HasPrefix(id, "rzp_test_") && !strings.HasPrefix(id, "rzp_live_"):
		return nil, errors.New("payments: RAZORPAY_KEY_ID does not look like a Razorpay key id")
	}
	return &Razorpay{keyID: id, keySecret: secret, webhookSecret: hook, BaseURL: RazorpayAPI,
		Client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Live reports whether these are live keys, which take real money.
func (p *Razorpay) Live() bool { return strings.HasPrefix(p.keyID, "rzp_live_") }

// Name implements Payments.
func (p *Razorpay) Name() string { return "razorpay" }

// KeyID implements Payments: the public key the app opens Checkout with.
func (p *Razorpay) KeyID() string { return p.keyID }

// errRazorpay is a reply Razorpay refused, with its status and its error description.
type errRazorpay struct {
	status int
	desc   string
}

func (e *errRazorpay) Error() string {
	return fmt.Sprintf("razorpay: %d %s", e.status, e.desc)
}

// call sends one API request with basic auth and decodes the reply into out.
func (p *Razorpay) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(p.keyID, p.keySecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("razorpay: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("razorpay: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Description string `json:"description"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &errRazorpay{status: resp.StatusCode, desc: e.Error.Description}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("razorpay: a reply that is not JSON: %w", err)
	}
	return nil
}

// NewOrder implements Payments: one Razorpay order for the amount, in rupees as paise.
func (p *Razorpay) NewOrder(ctx context.Context, amountPaise int) (string, error) {
	if amountPaise <= 0 {
		return "", errors.New("razorpay: the amount must be above zero")
	}
	var reply struct {
		ID     string `json:"id"`
		Amount int    `json:"amount"`
	}
	if err := p.call(ctx, http.MethodPost, "/orders", map[string]any{"amount": amountPaise, "currency": "INR"}, &reply); err != nil {
		return "", err
	}
	if !strings.HasPrefix(reply.ID, "order_") || reply.Amount != amountPaise {
		return "", errors.New("razorpay: the order reply did not match the request")
	}
	return reply.ID, nil
}

// rzRefund is the part of a Razorpay refund we read.
type rzRefund struct {
	ID     string  `json:"id"`
	Amount int     `json:"amount"`
	Status string  `json:"status"`
	Notes  rzNotes `json:"notes"`
}

// rzNotes are Razorpay's notes on an entity. Razorpay sends an object when there are notes and an
// empty array when there are none (a refund made in the dashboard, say), so both are read; values
// that are not strings are left out.
type rzNotes map[string]string

// UnmarshalJSON implements json.Unmarshaler.
func (n *rzNotes) UnmarshalJSON(b []byte) error {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err == nil {
		out := rzNotes{}
		for k, v := range obj {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		*n = out
		return nil
	}
	var arr []any
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*n = rzNotes{}
	return nil
}

// Refund sends money back for one of our refunds. Razorpay has no idempotency key for refunds, so
// each one carries our refund id in its notes, and the payment's refunds are read first: a refund
// already made for this id (a crash after the call, before the commit) is returned, not repeated.
func (p *Razorpay) Refund(ctx context.Context, paymentID, ourRefundID string, amountPaise int) (rzRefund, error) {
	var list struct {
		Items []rzRefund `json:"items"`
	}
	if err := p.call(ctx, http.MethodGet, "/payments/"+paymentID+"/refunds?count=100", nil, &list); err != nil {
		return rzRefund{}, err
	}
	for _, r := range list.Items {
		if r.Notes["refund_id"] == ourRefundID {
			if r.Amount != amountPaise {
				return rzRefund{}, errors.New("razorpay: a refund with our id has a different amount")
			}
			return r, nil
		}
	}
	var made rzRefund
	err := p.call(ctx, http.MethodPost, "/payments/"+paymentID+"/refund", map[string]any{
		"amount": amountPaise, "speed": "normal", "notes": rzNotes{"refund_id": ourRefundID},
	}, &made)
	if err != nil {
		return rzRefund{}, err
	}
	if !strings.HasPrefix(made.ID, "rfnd_") || made.Amount != amountPaise {
		return rzRefund{}, errors.New("razorpay: the refund reply did not match the request")
	}
	return made, nil
}

// CheckRazorpayKeys refuses live keys anywhere but production, so staging and local runs can never
// take real money.
func CheckRazorpayKeys(env string, p *Razorpay) error {
	if p != nil && p.Live() && env != "production" {
		return errors.New("payments: live Razorpay keys are refused outside production; use rzp_test_ keys")
	}
	return nil
}
