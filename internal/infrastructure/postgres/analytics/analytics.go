// Package analytics is the Postgres persistence for the Amplitude analytics
// worker: a read-only SourceReader over the existing booking_outbox /
// payment_outbox tables, and the CursorStore over analytics_cursor. It writes
// nothing to the outboxes themselves (it never touches their published_at) —
// only its own cursor rows.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/infrastructure/sqltx"
	uc "backend-core/internal/usecase/analytics"
)

// sourceTable maps a source name to its physical table. The map is the ONLY
// place a source string becomes a table name, so the query never interpolates
// caller input — an unknown source is rejected, never concatenated.
var sourceTable = map[uc.SourceName]string{
	uc.SourceBookingOutbox: "booking_outbox",
	uc.SourcePaymentOutbox: "payment_outbox",
}

// SourceReader reads outbox rows after a cursor. Implements uc.SourceReader.
type SourceReader struct{ pool sqltx.Querier }

// NewSourceReader builds the read-only outbox reader.
func NewSourceReader(pool sqltx.Querier) *SourceReader { return &SourceReader{pool: pool} }

var _ uc.SourceReader = (*SourceReader)(nil)

func (r *SourceReader) ListSince(ctx context.Context, source uc.SourceName, after uc.Cursor, limit int) ([]uc.SourceRow, error) {
	table, ok := sourceTable[source]
	if !ok {
		return nil, fmt.Errorf("analytics: unknown source %q", source)
	}
	if limit <= 0 {
		limit = 100
	}
	// Row-value comparison so (created_at, id) is a single deterministic cursor:
	// two rows sharing a created_at are still walked exactly once.
	q := `SELECT id, event_type, payload, created_at
	        FROM ` + table + `
	       WHERE (created_at, id) > ($1, $2)
	       ORDER BY created_at, id
	       LIMIT $3`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, after.CreatedAt, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("analytics: list %s: %w", source, err)
	}
	defer rows.Close()
	var out []uc.SourceRow
	for rows.Next() {
		var row uc.SourceRow
		var payload []byte
		if err := rows.Scan(&row.ID, &row.EventType, &payload, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("analytics: scan %s: %w", source, err)
		}
		row.Payload = payload
		out = append(out, row)
	}
	return out, rows.Err()
}

// CursorStore persists the per-source high-water mark. Implements uc.CursorStore.
type CursorStore struct{ pool sqltx.Querier }

// NewCursorStore builds the cursor repository over analytics_cursor.
func NewCursorStore(pool sqltx.Querier) *CursorStore { return &CursorStore{pool: pool} }

var _ uc.CursorStore = (*CursorStore)(nil)

func (s *CursorStore) Get(ctx context.Context, source uc.SourceName) (uc.Cursor, error) {
	var c uc.Cursor
	err := sqltx.From(ctx, s.pool).QueryRow(ctx,
		`SELECT last_created_at, last_id FROM analytics_cursor WHERE source=$1`,
		string(source)).Scan(&c.CreatedAt, &c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No cursor row yet: the zero cursor sorts before every outbox row, so
		// the worker ships from the beginning. In a migrated database the row
		// is seeded to now(), so this only happens in tests / a bare table.
		return uc.Cursor{}, nil
	}
	if err != nil {
		return uc.Cursor{}, fmt.Errorf("analytics: get cursor %s: %w", source, err)
	}
	return c, nil
}

func (s *CursorStore) Save(ctx context.Context, source uc.SourceName, c uc.Cursor) error {
	if c.ID == uuid.Nil {
		return fmt.Errorf("analytics: refusing to save cursor %s with nil id", source)
	}
	_, err := sqltx.From(ctx, s.pool).Exec(ctx,
		`INSERT INTO analytics_cursor (source, last_created_at, last_id, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (source) DO UPDATE
		   SET last_created_at = EXCLUDED.last_created_at,
		       last_id         = EXCLUDED.last_id,
		       updated_at      = EXCLUDED.updated_at`,
		string(source), c.CreatedAt, c.ID, time.Now())
	if err != nil {
		return fmt.Errorf("analytics: save cursor %s: %w", source, err)
	}
	return nil
}
