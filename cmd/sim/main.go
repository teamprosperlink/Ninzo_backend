// Command sim runs the geo simulator (issue 205) on the test zones and prints how each pick rule
// does against the timer:
//
//	sim -osrm http://localhost:5001 -zone koramangala -orders 200 -seed 1
//
// It needs OSRM serving the test-zone or Bengaluru data (task geo:osrm:up). Settings are the
// starting values unless given: -timer, -zone-radius, -perimeter, -sweet-spot, -speed, -search.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
	"github.com/teamprosperlink/Ninzo_backend/internal/sim"
)

// Riders wait at six points inside each test zone (cmd/testdata): the centre and five around it,
// as a pilot of 4 to 8 scooters would.
var scenarios = map[string]sim.Scenario{
	"koramangala": {
		Name: "Koramangala (test)",
		Area: sim.Box{MinLat: 12.925, MinLng: 77.612, MaxLat: 12.945, MaxLng: 77.640},
		Riders: []routing.Point{{Lat: 12.9350, Lng: 77.6260}, {Lat: 12.9290, Lng: 77.6180}, {Lat: 12.9290, Lng: 77.6340},
			{Lat: 12.9410, Lng: 77.6180}, {Lat: 12.9410, Lng: 77.6340}, {Lat: 12.9350, Lng: 77.6150}},
	},
	"indiranagar": {
		Name: "Indiranagar (test)",
		Area: sim.Box{MinLat: 12.965, MinLng: 77.632, MaxLat: 12.985, MaxLng: 77.650},
		Riders: []routing.Point{{Lat: 12.9750, Lng: 77.6410}, {Lat: 12.9690, Lng: 77.6360}, {Lat: 12.9690, Lng: 77.6460},
			{Lat: 12.9810, Lng: 77.6360}, {Lat: 12.9810, Lng: 77.6460}, {Lat: 12.9750, Lng: 77.6480}},
	},
}

func main() {
	if err := run(context.Background(), os.Args[1:], nil, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sim:", err)
		os.Exit(1)
	}
}

// run is main, with a router for tests (nil: a client on -osrm).
func run(ctx context.Context, args []string, r sim.Router, out io.Writer) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	osrm := fs.String("osrm", "", "OSRM base URL")
	zone := fs.String("zone", "koramangala", "koramangala or indiranagar")
	orders := fs.Int("orders", 200, "orders to simulate")
	seed := fs.Uint64("seed", 1, "first random seed")
	seeds := fs.Int("seeds", 1, "simulated days: seeds seed, seed+1, ...")
	var s sim.Settings
	fs.IntVar(&s.TimerS, "timer", 90, "timer_length_s")
	fs.IntVar(&s.ZoneRadiusM, "zone-radius", 350, "zone_radius_m")
	fs.IntVar(&s.PerimeterM, "perimeter", 100, "arrival_perimeter_m")
	fs.IntVar(&s.SweetSpotS, "sweet-spot", 80, "sweet_spot_budget_s")
	fs.IntVar(&s.StraightKmh, "speed", 15, "straight_line_speed_kmh")
	fs.IntVar(&s.MaxSearchM, "search", 1500, "max_search_radius_m")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: sim -osrm <url> [-zone koramangala|indiranagar] [-orders N] [-seed S] [-seeds K] [settings]")
	}
	sc, ok := scenarios[*zone]
	if !ok {
		return fmt.Errorf("unknown zone %q", *zone)
	}
	if *seeds < 1 || *seeds > 50 {
		return errors.New("seeds must be 1 to 50")
	}
	if *orders < 1 || *orders > 10000 {
		return errors.New("orders must be 1 to 10000")
	}
	sc.Orders, sc.Seed, sc.Setting = *orders, *seed, s
	if r == nil {
		if *osrm == "" {
			return errors.New("-osrm is required: the simulator runs on real roads")
		}
		c, err := routing.New(routing.Config{BaseURL: *osrm, Profile: "scooter", Timeout: 10 * time.Second})
		if err != nil {
			return err
		}
		r = c
	}
	// Several seeds are several simulated days on the same riders; counts are pooled across them
	// and each share gets a 95% interval, so a difference is not read into noise.
	type pooled struct{ orders, served, refused, stuck, inTime int }
	totals := map[string]*pooled{sim.RuleToday: {}, sim.RuleRoad: {}}
	var first []sim.Result
	for i := range *seeds {
		sc.Seed = *seed + uint64(i)
		results, err := sim.Run(ctx, r, sc)
		if err != nil {
			return err
		}
		if i == 0 {
			first = results
		}
		for _, res := range results {
			p := totals[res.Rule]
			p.orders += res.Orders
			p.served += res.Served
			p.refused += res.Refused
			p.stuck += res.Stuck
			p.inTime += res.InTime
		}
	}
	fmt.Fprintf(out, "%s, %d orders x %d seeds from %d; timer %d s, zone %d m, perimeter %d m, sweet spot %d s\n",
		sc.Name, sc.Orders, *seeds, *seed, s.TimerS, s.ZoneRadiusM, s.PerimeterM, s.SweetSpotS)
	fmt.Fprintf(out, "%-6s %7s %7s %6s %21s %21s %10s %10s\n", "rule", "served", "refused", "stuck",
		"in time / served", "in time / order", "timed p50", "timed p90")
	for _, res := range first {
		p := totals[res.Rule]
		lo, hi := sim.Wilson(p.inTime, p.served)
		olo, ohi := sim.Wilson(p.inTime, p.orders)
		share, per := 0.0, 0.0
		if p.served > 0 {
			share = float64(p.inTime) / float64(p.served)
		}
		if p.orders > 0 {
			per = float64(p.inTime) / float64(p.orders)
		}
		fmt.Fprintf(out, "%-6s %7d %7d %6d %5.1f%% (%4.1f-%5.1f%%) %5.1f%% (%4.1f-%5.1f%%) %9.0fs %9.0fs\n",
			res.Rule, p.served, p.refused, p.stuck, 100*share, 100*lo, 100*hi, 100*per, 100*olo, 100*ohi, res.TimedP50, res.TimedP90)
	}
	if *seeds > 1 {
		fmt.Fprintln(out, "(percentiles are from the first seed; counts and 95% intervals pool every seed)")
	}
	return nil
}
