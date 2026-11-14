package orders

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// BenchmarkWebhookSignature is the check every Razorpay webhook passes before anything else, on a
// typical 2 KB payload.
func BenchmarkWebhookSignature(b *testing.B) {
	body := []byte(`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_A","order_id":"order_A","amount":9900,` + strings.Repeat(`"x":"y",`, 240) + `"status":"captured"}}}}`)
	secret := "a-webhook-secret-for-the-benchmark"
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	sig := hex.EncodeToString(m.Sum(nil))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if !validSignature(body, sig, secret) {
			b.Fatal("signature refused")
		}
	}
}
