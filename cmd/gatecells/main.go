// Command gatecells builds the road-time cell sets around gates (issue 196).
//
//	gatecells -local|-remote build -road-data <id> -gate-history <id>
//	gatecells -local|-remote build-all -road-data <id>
//	gatecells -local|-remote export -gate-history <id> > tools/osrm/data/tiers.geojson  (git-ignored)
//
// export writes the gate version's newest build as GeoJSON, tiered with its zone's settings in force,
// for viewing (issue 203). The cells give the gate's location away: for the owner and ops only,
// never to commit or share.
//
// build-all builds every current gate version (the newest version of each gate, not erased) that
// has no build on that road data yet. The search radius is the gate zone's max_search_radius_m.
// ROUTING_ENGINE_URL names the OSRM to ask; DATABASE_URL the database. -local refuses anything
// but this machine; -remote refuses anything but Supabase, so use it only with the owner's okay.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/gatecells"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

const usage = `usage: gatecells -local|-remote build -road-data <id> -gate-history <id>
       gatecells -local|-remote build-all -road-data <id>
       gatecells -local|-remote export -gate-history <id>`

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, nil, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gatecells:", err)
		os.Exit(1)
	}
}

// run does the work. router is for tests; nil means a client on ROUTING_ENGINE_URL.
func run(ctx context.Context, args []string, getenv func(string) string, router gatecells.Router, out io.Writer) error {
	if len(args) < 2 || (args[0] != "-local" && args[0] != "-remote") || (args[1] != "build" && args[1] != "build-all" && args[1] != "export") {
		return errors.New(usage)
	}
	remote, command := args[0] == "-remote", args[1]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	roadData := fs.String("road-data", "", "")
	gateHistory := fs.String("gate-history", "", "")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return errors.New(usage)
	}
	// build takes both ids, build-all the road data only, export the gate version only.
	export := command == "export"
	if export == (*roadData != "") || (*roadData != "" && !uuidRe.MatchString(*roadData)) {
		return errors.New(usage)
	}
	if (command == "build-all") == (*gateHistory != "") || (*gateHistory != "" && !uuidRe.MatchString(*gateHistory)) {
		return errors.New(usage)
	}

	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	target, err := dbtarget.Parse(dsn)
	if err != nil {
		return err
	}
	if remote {
		err = target.CheckRemote()
	} else {
		err = target.CheckLocal()
	}
	if err != nil {
		return err
	}
	if router == nil && !export {
		url := getenv("ROUTING_ENGINE_URL")
		if url == "" {
			return errors.New("ROUTING_ENGINE_URL is not set: cell sets need real road times")
		}
		// Builds ask large tables; give OSRM longer than a live call gets.
		c, err := routing.New(routing.Config{BaseURL: url, Profile: getenv("ROUTING_PROFILE"), Timeout: 30 * time.Second})
		if err != nil {
			return err
		}
		router = c
	}
	// export writes only GeoJSON to out, so there the target goes to stderr.
	info := out
	if export {
		info = os.Stderr
	}
	fmt.Fprintf(info, "target: host %s, project %q\n", target.Host(), target.Ref())

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	pool, err := database.Open(ctx, dsn, 2)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	defer pool.Close()
	if export {
		return exportTiers(ctx, pool, *gateHistory, target, out)
	}

	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM road_data_versions WHERE id = $1)", *roadData).Scan(&exists); err != nil {
		return errors.New(target.Safe(err))
	}
	if !exists {
		return errors.New("no such road data version; register the build first (cmd/roaddata)")
	}

	ids := []string{*gateHistory}
	if command == "build-all" {
		if ids, err = unbuilt(ctx, pool, *roadData); err != nil {
			return errors.New(target.Safe(err))
		}
		fmt.Fprintf(out, "%d gate versions to build\n", len(ids))
	}
	g := hexgrid.Bengaluru()
	// One gate that cannot be built (say, outside the road data) must not stop the rest: report
	// each failure, carry on, and fail at the end.
	built, failed := 0, 0
	var lastErr error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cells, reachable, err := buildOne(ctx, pool, router, g, id, *roadData)
		if err != nil {
			failed++
			lastErr = fmt.Errorf("gate version %s: %s", id, target.Safe(err))
			fmt.Fprintf(out, "failed %s: %s\n", id, target.Safe(err))
			continue
		}
		built++
		fmt.Fprintf(out, "built %s: %d cells, %d reachable\n", id, cells, reachable)
	}
	fmt.Fprintf(out, "%d built, %d failed\n", built, failed)
	if failed > 0 && len(ids) == 1 {
		return lastErr // one gate: say why
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d gate versions failed", failed, len(ids))
	}
	return nil
}

// unbuilt lists the current gate versions with no live build on this road data.
func unbuilt(ctx context.Context, pool *pgxpool.Pool, roadData string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT h.id::text FROM gate_history h
		WHERE h.redacted_at IS NULL
		  AND h.version = (SELECT max(version) FROM gate_history x WHERE x.gate_id = h.gate_id)
		  AND NOT EXISTS (SELECT 1 FROM gate_cell_builds b
		                  WHERE b.gate_history_id = h.id AND b.road_data_version_id = $1 AND b.redacted_at IS NULL)
		ORDER BY h.created_at, h.id`, roadData)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func buildOne(ctx context.Context, pool *pgxpool.Pool, router gatecells.Router, g *hexgrid.Grid, gateHistoryID, roadData string) (int, int, error) {
	var lat, lng float64
	var zone string
	var redacted bool
	err := pool.QueryRow(ctx, `SELECT h.lat, h.lng, g.zone_id::text, h.redacted_at IS NOT NULL
		FROM gate_history h JOIN gates g ON g.id = h.gate_id WHERE h.id = $1`, gateHistoryID).Scan(&lat, &lng, &zone, &redacted)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, errors.New("no such gate version")
	}
	if err != nil {
		return 0, 0, err
	}
	if redacted {
		return 0, 0, errors.New("the gate version is erased")
	}
	s, err := geo.LoadSettings(ctx, pool, zone)
	if err != nil {
		return 0, 0, err
	}
	radius, ok := s.Get("max_search_radius_m")
	if !ok {
		return 0, 0, errors.New("max_search_radius_m is not set")
	}
	cells, err := gatecells.Build(ctx, router, g, hexgrid.Point{Lat: lat, Lng: lng}, radius)
	if err != nil {
		return 0, 0, err
	}
	reachable := 0
	for _, c := range cells {
		if c.Reachable {
			reachable++
		}
	}
	err = database.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := gatecells.Store(ctx, tx, gateHistoryID, roadData, g, radius, hexgrid.Point{Lat: lat, Lng: lng}, cells)
		return err
	})
	return len(cells), reachable, err
}

// exportTiers writes a gate version's newest build, tiered with its zone's settings, as GeoJSON.
func exportTiers(ctx context.Context, pool *pgxpool.Pool, gateHistoryID string, target dbtarget.Target, out io.Writer) error {
	var zone string
	var erased bool
	err := pool.QueryRow(ctx, `SELECT g.zone_id::text, h.redacted_at IS NOT NULL
		FROM gate_history h JOIN gates g ON g.id = h.gate_id WHERE h.id = $1`, gateHistoryID).Scan(&zone, &erased)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("no such gate version")
	}
	if err != nil {
		return errors.New(target.Safe(err))
	}
	if erased {
		return errors.New("the gate version is erased; there is nothing to export")
	}
	s, err := geo.LoadSettings(ctx, pool, zone)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	g := hexgrid.Bengaluru()
	t, err := gatecells.Tiers(ctx, pool, g, gateHistoryID, s)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	b, err := gatecells.GeoJSON(g, t)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", b)
	return err
}
