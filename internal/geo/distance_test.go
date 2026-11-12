package geo

import (
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// routing repeats this package's Metres so it stays free of HTTP and database code; the two must
// agree exactly. (Moved here from internal/routing in #196: geo now depends on routing through
// gatecells, so the check lives on this side.)
func TestMetresMatchesTheRoutingPackage(t *testing.T) {
	rider, gate := routing.Point{Lat: 12.97840, Lng: 77.64080}, routing.Point{Lat: 12.97194, Lng: 77.64115}
	for _, p := range [][2]routing.Point{{rider, gate}, {{Lat: 0, Lng: 0}, {Lat: 0, Lng: 180}}, {{Lat: -33.9, Lng: 18.4}, {Lat: 51.5, Lng: -0.1}}, {gate, gate}} {
		a, b := routing.Metres(p[0], p[1]), Metres(p[0].Lat, p[0].Lng, p[1].Lat, p[1].Lng)
		if a != b {
			t.Fatalf("%v: routing %f, geo %f", p, a, b)
		}
	}
}
