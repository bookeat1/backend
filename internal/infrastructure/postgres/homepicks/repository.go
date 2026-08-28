// Package homepicks is the Postgres implementation of
// domain.HomePicksRepository — the hand-curated «Выбрали для вас» rail
// (migration 0090).
//
// The table is tiny (a handful of rows per city) and it is read on every open
// of the main screen, so both queries here are deliberately trivial: one
// ordered SELECT of ids, and a delete-then-insert replacement. No partial
// update path exists — see domain.HomePicksRepository.Replace for why.
package homepicks

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const foreignKeyViolation = "23503"

// Repository implements domain.HomePicksRepository.
type Repository struct {
	pool sqltx.Querier
	tx   domain.TxManager
}

// New builds the repository. tx is used to make a replacement atomic: a reader
// must never see the moment between "the old list is gone" and "the new one is
// in", because that moment looks exactly like "nothing is picked" and would
// flip the whole city to the automatic rail.
func New(pool sqltx.Querier, tx domain.TxManager) *Repository {
	return &Repository{pool: pool, tx: tx}
}

var _ domain.HomePicksRepository = (*Repository)(nil)

func (r *Repository) ListIDs(ctx context.Context, city string) ([]uuid.UUID, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		// restaurant_id is the tie-break, not decoration: the unique constraint
		// makes duplicate positions impossible, but an ORDER BY that can tie is
		// an ORDER BY that can reshuffle between two calls, and this list is
		// shown to the same guest twice a minute.
		`SELECT restaurant_id FROM home_picks WHERE city = $1 ORDER BY position, restaurant_id`,
		city)
	if err != nil {
		return nil, fmt.Errorf("list home picks: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list home picks: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list home picks: %w", err)
	}
	return out, nil
}

func (r *Repository) Replace(ctx context.Context, city string, restaurantIDs []uuid.UUID) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := sqltx.From(ctx, r.pool)
		if _, err := q.Exec(ctx, `DELETE FROM home_picks WHERE city = $1`, city); err != nil {
			return fmt.Errorf("clear home picks: %w", err)
		}
		if len(restaurantIDs) == 0 {
			return nil
		}
		positions := make([]int32, len(restaurantIDs))
		for i := range restaurantIDs {
			positions[i] = int32(i + 1)
		}
		// One statement, so the whole list costs one round trip however long it
		// gets. An unknown venue id trips the foreign key and maps to
		// ErrNotFound — the same convention events/reviews use, and the honest
		// answer to "save this list": 404, not a half-written rail.
		if _, err := q.Exec(ctx,
			`INSERT INTO home_picks (city, restaurant_id, position)
			 SELECT $1, t.rid, t.pos FROM unnest($2::uuid[], $3::int[]) AS t(rid, pos)`,
			city, restaurantIDs, positions); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
				return fmt.Errorf("insert home picks: %w", domain.ErrNotFound)
			}
			return fmt.Errorf("insert home picks: %w", err)
		}
		return nil
	})
}
