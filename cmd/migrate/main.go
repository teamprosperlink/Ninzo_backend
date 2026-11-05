// Command migrate applies the SQL files in migrations/ to the database in DATABASE_URL.
//
//	migrate -local  status|up|down   refuses anything but this machine
//	migrate -remote status|up|down   refuses anything but Ninzo's Supabase project
//
// One of the two is required, so the target is never a guess. status changes nothing.
// On -remote, down also needs MIGRATE_ALLOW_DOWN=yes.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/migrations"
)

const usage = "usage: migrate -local|-remote status|up|down"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 || (args[0] != "-local" && args[0] != "-remote") {
		return errors.New(usage)
	}
	remote, command := args[0] == "-remote", args[1]
	if command != "status" && command != "up" && command != "down" {
		return errors.New(usage)
	}

	dsn := os.Getenv("DATABASE_URL")
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
	if remote && command == "down" && os.Getenv("MIGRATE_ALLOW_DOWN") != "yes" {
		return errors.New("refusing to go down on Supabase. Set MIGRATE_ALLOW_DOWN=yes if you are sure")
	}
	fmt.Printf("target: host %s, project %q\n", target.Host(), target.Ref())

	if err := migrate(command, dsn); err != nil {
		return errors.New(target.Safe(err))
	}
	return nil
}

func migrate(command, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch command {
	case "status":
		return status(ctx, db, provider)
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			return err
		}
		for _, r := range results {
			fmt.Println("applied", r.Source.Path)
		}
		fmt.Printf("%d applied\n", len(results))
	case "down":
		result, err := provider.Down(ctx)
		if err != nil {
			return err
		}
		fmt.Println("reverted", result.Source.Path)
	}
	return nil
}

// status changes nothing. Goose's own status creates its version table, so on an empty
// database this does not call it.
func status(ctx context.Context, db *sql.DB, provider *goose.Provider) error {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.goose_db_version') IS NOT NULL").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		for _, s := range provider.ListSources() {
			fmt.Printf("%-10s %s\n", goose.StatePending, s.Path)
		}
		return nil
	}
	statuses, err := provider.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		fmt.Printf("%-10s %s\n", s.State, s.Source.Path)
	}
	return nil
}
