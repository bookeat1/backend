// Package sqltx provides the database transaction manager. WithinTx injects the
// active *sql.Tx into the context; repositories read it back via From so that
// multiple repos inside one usecase share a single transaction. Nested WithinTx
// calls reuse the existing tx (no double-begin).
package sqltx

import (
	"context"
	"database/sql"
)

// DBTX is the subset of *sql.DB / *sql.Tx that repositories use.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type ctxKey struct{}

// Manager implements domain.TxManager.
type Manager struct{ db *sql.DB }

func NewManager(db *sql.DB) *Manager { return &Manager{db: db} }

// WithinTx runs fn inside one transaction. If a tx is already active on ctx it
// reuses it (fn joins the outer transaction). Commits on nil, rolls back on error.
func (m *Manager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(ctxKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(context.WithValue(ctx, ctxKey{}, tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// From returns the active *sql.Tx from ctx, or the given pool when none is set.
func From(ctx context.Context, pool DBTX) DBTX {
	if tx, ok := ctx.Value(ctxKey{}).(*sql.Tx); ok {
		return tx
	}
	return pool
}
