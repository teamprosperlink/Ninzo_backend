package fcm

import (
	"encoding/json"
	"testing"
	"time"
)

// BenchmarkMessage builds and encodes one push, as Push does per device.
func BenchmarkMessage(b *testing.B) {
	payload := map[string]any{"customer_id": "8d3c1f2e-7a51-4c1b-9b2f-0a4e5d6c7b8a", "order_id": "1f2e3d4c-5b6a-4978-8877-665544332211", "stage": "arrived", "seq": 7}
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(map[string]any{"message": Message("ios", "token", "push.customer", payload, now)}); err != nil {
			b.Fatal(err)
		}
	}
}
