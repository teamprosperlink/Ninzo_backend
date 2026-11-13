// Command placement builds a zone's placement plans (issue 224) from recorded demand and OSRM road
// times, and prints a summary. DATABASE_URL names the database: this machine only, unless -remote,
// which refuses anything but Supabase; use it only with the owner's okay.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/placementplan"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "placement:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("placement", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	zone := fs.String("zone", "", "the zone id")
	osrm := fs.String("osrm", "", "OSRM base URL (default ROUTING_ENGINE_URL)")
	remote := fs.Bool("remote", false, "write to Supabase (staging), with the owner's okay; without it only this machine is allowed")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *zone == "" {
		return errors.New("usage: placement -zone <zone id> [-osrm <url>] [-remote]")
	}
	url := *osrm
	if url == "" {
		url = getenv("ROUTING_ENGINE_URL")
	}
	if url == "" {
		return errors.New("an OSRM URL is needed (-osrm or ROUTING_ENGINE_URL): plans are built on real roads")
	}
	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	target, err := dbtarget.Parse(dsn)
	if err != nil {
		return err
	}
	if *remote {
		err = target.CheckRemote()
	} else {
		err = target.CheckLocal()
	}
	if err != nil {
		return err
	}
	r, err := routing.New(routing.Config{BaseURL: url, Profile: getenv("ROUTING_PROFILE"), Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	res, err := placementplan.Build(ctx, pool, r, *zone, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "build %s: %d nodes, %d demand cells, up to %d scooters, %d plans\n", res.BuildID, res.Nodes, res.Cells, res.MaxN, res.PlanCount)
	return nil
}
