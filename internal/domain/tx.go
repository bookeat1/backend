package domain

import "context"

// TxManager runs fn inside a single database transaction. Implemented by
// infrastructure/sqltx.Manager. Nested calls reuse the active transaction.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
	// Detach returns a context with no active transaction, so a statement runs
	// on its own and survives a rollback of the surrounding transaction. Needed
	// for writes that must outlive a failed request — an anti-fraud attempt
	// counter is worthless if every rejected attempt un-happens.
	Detach(ctx context.Context) context.Context
}
