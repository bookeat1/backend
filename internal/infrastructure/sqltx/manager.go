// Package sqltx provides the database transaction manager over pgx. WithinTx
// injects the active pgx.Tx into the context; repositories read it back via From
// so that multiple repos inside one usecase share a single transaction. Nested
// WithinTx calls reuse the existing tx (no double-begin).
package sqltx

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of *pgxpool.Pool / pgx.Tx that repositories use. Both a
// pool and an active transaction satisfy it, so From can return either.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type ctxKey struct{}

// Manager implements domain.TxManager over a pgxpool.Pool.
type Manager struct{ pool *pgxpool.Pool }

// NewManager builds a transaction manager bound to pool.
func NewManager(pool *pgxpool.Pool) *Manager { return &Manager{pool: pool} }

// Detach strips the active transaction from ctx: statements run on the pool and
// are unaffected by a rollback of the surrounding transaction. WithinTx called
// on a detached context starts a fresh transaction.
func (m *Manager) Detach(ctx context.Context) context.Context {
	if ctx.Value(ctxKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, nil)
}

// WithinTx runs fn inside one transaction. If a tx is already active on ctx it
// reuses it (fn joins the outer transaction). Commits on nil, rolls back on
// error. Rollbacks use a background context so cleanup still runs even if ctx
// is already cancelled.
func (m *Manager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.Background())
			panic(p)
		}
	}()
	if err := fn(context.WithValue(ctx, ctxKey{}, tx)); err != nil {
		_ = tx.Rollback(context.Background())
		return err
	}
	return tx.Commit(ctx)
}

// From returns the active pgx.Tx from ctx, or the given pool when none is set.
func From(ctx context.Context, pool Querier) Querier {
	if tx, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok {
		return tx
	}
	return pool
}
