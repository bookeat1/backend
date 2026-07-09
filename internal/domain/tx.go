package domain

import "context"

// TxManager runs fn inside a single database transaction. Implemented by
// infrastructure/sqltx.Manager. Nested calls reuse the active transaction.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}
