package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue 235, part B3. Words in the contracts that the code and the app developers rely on.
func TestQuarantineAndGateCheckTextsSayWhatTheServerDoes(t *testing.T) {
	read := func(file string) string {
		raw, err := os.ReadFile(filepath.Join(contractsDir, file))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	console, rider := read("console/openapi.yaml"), read("rider/openapi.yaml")
	for _, want := range []string{
		// Only rider quarantines are on an order's timeline.
		"Hub quarantines do not, because those lots were at the hub",
		"source stock",
	} {
		if !strings.Contains(console, want) {
			t.Errorf("the console contract no longer says %q", want)
		}
	}
	if strings.Contains(console, "The same events show on an order's timeline") {
		t.Error("the console contract says hub quarantines show on an order's timeline")
	}
	for _, want := range []string{
		// A lot that an order holds part of.
		"the units no order holds are taken out of sale and off the scooter at once",
		"the units an order holds stay with that order",
		"the rider reports a problem on that order",
		// The QR the rider app reads, and a typed code.
		"For method qr the value is NZG|<gate id>",
		"For method code, no gate has a typed code, so checked is false and matched is true",
	} {
		if !strings.Contains(strings.ToLower(rider), strings.ToLower(want)) {
			t.Errorf("the rider contract no longer says %q", want)
		}
	}
}
