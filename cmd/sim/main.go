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
	"strconv"
	"strings"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/placement"
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
	fleetN := fs.Int("fleet", 0, "fleet mode: the first N rider points, busy while serving (0: every rider free for every order)")
	rate := fs.Float64("orders-per-hour", 20, "fleet mode: Poisson arrival rate")
	hours := fs.Float64("hours", 3, "fleet mode: hours simulated")
	pack := fs.Float64("pack", 60, "fleet mode: packing seconds")
	handover := fs.Float64("handover", 60, "fleet mode: handover seconds")
	place := fs.Bool("place", false, "fleet mode: place the riders by maximum expected covering on road times instead of the fixed points")
	busyQ := fs.Float64("q", 0.2, "with -place: the busy share for expected covering")
	hotspots := fs.String("hotspots", "", "uneven demand: lat,lng,share,radius_m;... (share of all gates from each; the rest uniform)")
	placeFrom := fs.String("place-from", "draws", "with -place: fit on draws (gates drawn from the scenario's demand), uniform (as if demand were even) or forecast (a 4-week synthetic history per cell, forecast v1)")
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
	if *hotspots != "" {
		hs, err := parseHotspots(*hotspots)
		if err != nil {
			return err
		}
		sc.Hotspots = hs
	}
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
	if *fleetN > 0 {
		if *fleetN > len(sc.Riders) {
			return fmt.Errorf("fleet must be 1 to %d for this zone", len(sc.Riders))
		}
		if *rate <= 0 || *rate > 600 || *hours <= 0 || *hours > 24 {
			return errors.New("orders-per-hour must be above 0 and at most 600, hours above 0 and at most 24")
		}
		if *place {
			// Placed on demand drawn with a seed of its own (1000 + seed), so the evaluation seeds
			// below never judge placement on the demand it was fitted to.
			o := sim.PlaceOptions{N: *fleetN, Q: *busyQ, Capacity: 1, GridRows: 6, GridCols: 8, Demand: 150, DemandSeed: 1000 + *seed}
			var pts []routing.Point
			var plan placement.Plan
			var err error
			switch *placeFrom {
			case "draws":
				pts, plan, err = sim.Place(ctx, r, sc, o)
			case "uniform":
				even := sc
				even.Hotspots = nil
				pts, plan, err = sim.Place(ctx, r, even, o)
			case "forecast":
				// Four past weeks of 150 gates each, seeds 2000 + 4·seed to 2003 + 4·seed: never the
				// evaluation seeds, and no two fits share a week.
				cells, w, cerr := sim.CellDemand(ctx, r, sc, 4, 150, 2000+4*(*seed), 0.8)
				if cerr != nil {
					return cerr
				}
				pts, plan, err = sim.PlaceOnCells(ctx, r, sc, cells, w, o)
			default:
				return errors.New("place-from must be draws, uniform or forecast")
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "placed %d riders: %.0f%% of placement demand in reach, %.0f%% expected at q %.2f\n", *fleetN, 100*plan.Covered, 100*plan.Expected, *busyQ)
			sc.Riders = pts
		}
		return runFleet(ctx, r, sc, *fleetN, *rate, *hours, *pack, *handover, *seed, *seeds, out)
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

// runFleet is fleet mode: riders get busy, orders arrive over time (#214). Counts pool every seed.
func runFleet(ctx context.Context, r sim.Router, sc sim.Scenario, n int, rate, hours, pack, handover float64, seed uint64, seeds int, out io.Writer) error {
	type pooled struct{ orders, served, busy, reach, stuck, inTime int }
	totals := map[string]*pooled{sim.RuleToday: {}, sim.RuleRoad: {}}
	busyShare := map[string]float64{}
	sc.Riders = sc.Riders[:n]
	for i := range seeds {
		sc.Seed = seed + uint64(i)
		results, err := sim.RunFleet(ctx, r, sim.FleetScenario{Scenario: sc, OrdersPerHour: rate, Hours: hours, PackS: pack, HandoverS: handover})
		if err != nil {
			return err
		}
		for _, res := range results {
			p := totals[res.Rule]
			p.orders += res.Orders
			p.served += res.Served
			p.busy += res.RefusedBusy
			p.reach += res.RefusedReach
			p.stuck += res.Stuck
			p.inTime += res.InTime
			for _, b := range res.BusyShare {
				busyShare[res.Rule] += b / float64(n*seeds)
			}
		}
	}
	fmt.Fprintf(out, "%s, %d riders, %.0f orders an hour for %.1f h x %d seeds from %d; pack %.0f s, handover %.0f s; timer %d s, sweet spot %d s\n",
		sc.Name, n, rate, hours, seeds, seed, pack, handover, sc.Setting.TimerS, sc.Setting.SweetSpotS)
	fmt.Fprintf(out, "%-6s %7s %7s %11s %12s %6s %21s %9s\n", "rule", "orders", "served", "busy-refused", "reach-refused", "stuck", "in time / order", "busy")
	for _, rule := range []string{sim.RuleToday, sim.RuleRoad} {
		p := totals[rule]
		lo, hi := sim.Wilson(p.inTime, p.orders)
		per := 0.0
		if p.orders > 0 {
			per = float64(p.inTime) / float64(p.orders)
		}
		fmt.Fprintf(out, "%-6s %7d %7d %11d %12d %6d %5.1f%% (%4.1f-%5.1f%%) %8.0f%%\n",
			rule, p.orders, p.served, p.busy, p.reach, p.stuck, 100*per, 100*lo, 100*hi, 100*busyShare[rule])
	}
	return nil
}

// parseHotspots reads lat,lng,share,radius_m;... .
func parseHotspots(s string) ([]sim.Hotspot, error) {
	var out []sim.Hotspot
	for _, part := range strings.Split(s, ";") {
		f := strings.Split(strings.TrimSpace(part), ",")
		if len(f) != 4 {
			return nil, fmt.Errorf("hotspot %q: want lat,lng,share,radius_m", part)
		}
		var v [4]float64
		for i, x := range f {
			n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, fmt.Errorf("hotspot %q: %v", part, err)
			}
			v[i] = n
		}
		out = append(out, sim.Hotspot{Lat: v[0], Lng: v[1], Share: v[2], RadiusM: v[3]})
	}
	return out, nil
}
