// Package service starts an API, so each main.go holds only wiring.
package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// Deps is what an API gets to build its endpoints.
type Deps struct {
	Config config.Config
	Log    *slog.Logger
	DB     *pgxpool.Pool
}

// Options describes one API. Only Name is required.
type Options struct {
	Name           string
	Mount          func(chi.Router, Deps)
	Getenv         func(string) string
	LogOutput      io.Writer
	StartupTimeout time.Duration
	// Validate adds this API's own startup checks, and may be nil.
	Validate func(config.Config) error
}

// Run starts the API and blocks until ctx ends. An error means it could not run, and the
// process should exit non-zero so the platform can restart it.
func Run(ctx context.Context, opts Options) error {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.LogOutput == nil {
		opts.LogOutput = os.Stdout
	}
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 30 * time.Second
	}
	cfg, err := config.Load(opts.Getenv)
	if err != nil {
		return err
	}
	if opts.Validate != nil {
		if err := opts.Validate(cfg); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewJSONHandler(opts.LogOutput, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", opts.Name, "env", cfg.Env)
	log.Info("starting", "database_host", cfg.Database.Host(), "database_project", cfg.Database.Ref())

	startup, cancel := context.WithTimeout(ctx, opts.StartupTimeout)
	defer cancel()
	pool, err := connect(startup, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	deps := Deps{Config: cfg, Log: log, DB: pool}
	ready := func(c context.Context) error {
		if err := database.Ping(c, pool); err != nil {
			return fmt.Errorf("database: %s", cfg.Database.Safe(err))
		}
		return nil
	}
	var mount func(chi.Router)
	if opts.Mount != nil {
		mount = func(r chi.Router) { opts.Mount(r, deps) }
	}
	return httpserver.Serve(ctx, cfg.Port, httpserver.NewRouter(log, ready, mount), log)
}

// connect opens the pool and confirms the login is really the service role, beyond what the URL
// says. Local Postgres has no such role in use.
func connect(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := database.Open(ctx, string(cfg.DatabaseURL), cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}
	if !cfg.IsLocal() {
		if err := database.VerifyRole(ctx, pool, dbtarget.ServiceRole); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}
