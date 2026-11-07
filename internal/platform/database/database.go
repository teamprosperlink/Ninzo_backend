// Package database opens the connection pool. Its errors never hold the password.
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
)

const (
	connectTimeout = 5 * time.Second
	// PingTimeout must outlast a whole new connection. A new connection from another region can take
	// 2 seconds or more, so a 2-second limit often failed, and hid the database's real error.
	PingTimeout = connectTimeout + 3*time.Second
)

// Open creates a small pool and waits for the database to answer, until ctx ends. A database
// that is only briefly away should not stop a service from starting.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	target, err := dbtarget.Parse(url)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("the database URL is not valid")
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 1 // Otherwise the first request after a quiet spell pays for a whole new connection.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New(target.Safe(err))
	}
	for {
		err = Ping(ctx, pool)
		if err == nil {
			return pool, nil
		}
		if isPermanent(err) {
			pool.Close()
			return nil, fmt.Errorf("the database refused the connection: %s", target.Safe(err))
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("the database did not answer: %s", target.Safe(err))
		case <-time.After(time.Second):
		}
	}
}

// isPermanent reports a wrong password, an unknown role or an unknown database. Trying again
// cannot fix these, and repeated failed sign-ins can get a client banned by a pooler.
func isPermanent(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "28") || pgErr.Code == "3D000"
}

// InTx runs fn in a transaction: it commits if fn returns nil, and rolls back on an error or a
// panic. Never call an outside service inside fn, because that holds a connection and row locks.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if v := recover(); v != nil {
			_ = rollback(tx)
			panic(v)
		}
	}()
	if err := fn(tx); err != nil {
		_ = rollback(tx)
		return err
	}
	return tx.Commit(ctx)
}

// rollback runs on its own context, because the request's may have ended.
func rollback(tx pgx.Tx) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tx.Rollback(ctx)
}

// Ping checks the database answers, within a short time.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, PingTimeout)
	defer cancel()
	return pool.Ping(ctx)
}

// VerifyRole stops a service that signed in as the wrong role, and checks the powers the
// service depends on. Migrations keep the postgres login, and services never use it.
func VerifyRole(ctx context.Context, pool *pgxpool.Pool, want string) error {
	var got string
	var bypass, tooMuch bool
	err := pool.QueryRow(ctx, `SELECT current_user, rolbypassrls, rolsuper OR rolcreatedb OR rolcreaterole
		FROM pg_roles WHERE rolname = current_user`).Scan(&got, &bypass, &tooMuch)
	if err != nil {
		return errors.New("could not check the database role")
	}
	if got != want {
		return fmt.Errorf("connected as %s, but services must use %s", got, want)
	}
	if !bypass {
		return fmt.Errorf("%s cannot bypass Row Level Security", got)
	}
	if tooMuch {
		return fmt.Errorf("%s has more power than a service should", got)
	}
	return nil
}
