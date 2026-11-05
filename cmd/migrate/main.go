// Command migrate applies the SQL files in migrations/ to the database in DATABASE_URL.
//
//	migrate status
//	migrate up
//	migrate down   (one step, and only on localhost unless MIGRATE_ALLOW_DOWN=yes)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/teamprosperlink/Ninzo_backend/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: migrate status|up|down")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	host, err := hostOf(dsn)
	if err != nil {
		return err
	}
	fmt.Println("database host:", host) // Never print the URL: it holds the password.

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	files, err := fs.Sub(migrations.FS, ".")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch args[0] {
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			fmt.Printf("%-10s %s\n", s.State, s.Source.Path)
		}
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
		if !isLocal(host) && os.Getenv("MIGRATE_ALLOW_DOWN") != "yes" {
			return fmt.Errorf("refusing to go down on %s. Set MIGRATE_ALLOW_DOWN=yes if you are sure", host)
		}
		result, err := provider.Down(ctx)
		if err != nil {
			return err
		}
		fmt.Println("reverted", result.Source.Path)
	default:
		return fmt.Errorf("unknown command %q: use status, up or down", args[0])
	}
	return nil
}

func hostOf(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL is not a valid URL")
	}
	return u.Hostname(), nil
}

func isLocal(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || strings.HasPrefix(host, "::1")
}
