package bootstrap

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver (goose, etl)
)

// NewDB opens a pgxpool connection pool configured from cfg and verifies
// connectivity with a Ping. It is the primary handle used by the HTTP service
// and repositories. The caller owns Close.
func NewDB(cfg PostgresConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse db config: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		// pgxpool keeps MinConns open; use MaxIdleConns as that floor, clamped to
		// the pool size.
		min := int32(cfg.MaxIdleConns)
		if min > poolCfg.MaxConns {
			min = poolCfg.MaxConns
		}
		poolCfg.MinConns = min
	}
	poolCfg.MaxConnLifetime = cfg.ConnMaxLifetime
	poolCfg.MaxConnIdleTime = cfg.ConnMaxIdleTime

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

// NewSQLDB opens a database/sql pool backed by the pgx stdlib driver. It exists
// only for tools that require a *sql.DB — goose migrations and the one-time ETL.
// The request path uses the pgxpool handle from NewDB instead.
func NewSQLDB(cfg PostgresConfig) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return db, nil
}
