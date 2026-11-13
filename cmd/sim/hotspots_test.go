package main

import (
	"bytes"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/sim"
)

func TestParseHotspots(t *testing.T) {
	hs, err := parseHotspots("12.9355,77.6245,0.35,200; 12.928, 77.636, 0.25, 150")
	if err != nil {
		t.Fatal(err)
	}
	want := []sim.Hotspot{{Lat: 12.9355, Lng: 77.6245, Share: 0.35, RadiusM: 200}, {Lat: 12.928, Lng: 77.636, Share: 0.25, RadiusM: 150}}
	if len(hs) != 2 || hs[0] != want[0] || hs[1] != want[1] {
		t.Fatalf("%+v", hs)
	}
	for _, bad := range []string{"12.9,77.6,0.3", "12.9,77.6,x,100", "12.9,77.6,0.3,100;"} {
		if _, err := parseHotspots(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPlaceFromMustBeKnown(t *testing.T) {
	// The choice is checked before any road is asked for, so no OSRM is needed.
	err := run(t.Context(), []string{"-osrm", "http://127.0.0.1:1", "-fleet", "4", "-place", "-place-from", "guess"}, nil, &bytes.Buffer{})
	if err == nil || err.Error() != "place-from must be draws, uniform or forecast" {
		t.Fatalf("%v", err)
	}
}
