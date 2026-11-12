// Command roaddata records the OSRM road data builds in use (issue 190).
//
//	roaddata -local|-remote register tools/osrm/data/bengaluru/build.json
//	roaddata -local|-remote list
//
// -local refuses anything but this machine; -remote refuses anything but Supabase, so use it only
// with the owner's okay. DATABASE_URL names the database.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/roaddata"
)

const usage = `usage: roaddata -local|-remote register <build.json>
       roaddata -local|-remote list`

// maxBuildJSON is far above a real build.json (under 1 KB).
const maxBuildJSON = 64 << 10

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.ReadFile, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "roaddata:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, readFile func(string) ([]byte, error), out io.Writer) error {
	if len(args) < 2 || (args[0] != "-local" && args[0] != "-remote") {
		return errors.New(usage)
	}
	remote, command := args[0] == "-remote", args[1]
	var v roaddata.Version
	switch {
	case command == "register" && len(args) == 3:
		data, err := readFile(args[2])
		if err != nil {
			return err
		}
		if len(data) > maxBuildJSON {
			return errors.New("build.json is too large")
		}
		// Check the file before touching any database.
		if v, err = roaddata.Parse(data); err != nil {
			return err
		}
	case command == "list" && len(args) == 2:
	default:
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
	fmt.Fprintf(out, "target: host %s, project %q\n", target.Host(), target.Ref())

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := database.Open(ctx, dsn, 2)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	defer pool.Close()

	if command == "register" {
		var id string
		var created bool
		err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			id, created, err = roaddata.Register(ctx, tx, v)
			return err
		})
		if err != nil {
			return errors.New(target.Safe(err))
		}
		if created {
			fmt.Fprintf(out, "registered %s (%s, %s, map of %s): %s\n", v.Name, v.ProfileVersion, v.Algorithm, v.OSMTimestamp.UTC().Format(time.RFC3339), id)
		} else {
			fmt.Fprintf(out, "already registered: %s\n", id)
		}
		return nil
	}
	rows, err := roaddata.List(ctx, pool)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no road data registered")
	}
	for _, r := range rows {
		fmt.Fprintf(out, "%s  %-12s %-10s %s  map %s  built %s\n", r.ID, r.Name, r.ProfileVersion, r.Algorithm,
			r.OSMTimestamp.UTC().Format(time.RFC3339), r.BuiltAt.UTC().Format(time.RFC3339))
	}
	return nil
}
