// Package foodieprofile is the Postgres implementation of
// domain.FoodieProfileRepository: the multi-value picks (cuisines/diets/
// allergies) of the "Фуди-профиль" wizard (mobile PR #222, migration 0109).
// Budget is a single column on users (domain.User.FoodieBudgetTier),
// handled by the user repository instead.
package foodieprofile

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.FoodieProfileRepository = (*Repository)(nil)

// table names a single (id_column, table) pair Replace/Get loop over — the
// three axes are structurally identical (user_id + one varchar id column),
// only the table/column name differs.
type table struct {
	name     string
	idColumn string
}

var (
	cuisinesTable  = table{name: "user_foodie_cuisines", idColumn: "cuisine_id"}
	dietsTable     = table{name: "user_foodie_diets", idColumn: "diet_id"}
	allergiesTable = table{name: "user_foodie_allergies", idColumn: "allergy_id"}
)

func (r *Repository) Get(ctx context.Context, userID uuid.UUID) (domain.FoodieProfilePreferences, error) {
	cuisines, err := r.listIDs(ctx, cuisinesTable, userID)
	if err != nil {
		return domain.FoodieProfilePreferences{}, err
	}
	diets, err := r.listIDs(ctx, dietsTable, userID)
	if err != nil {
		return domain.FoodieProfilePreferences{}, err
	}
	allergies, err := r.listIDs(ctx, allergiesTable, userID)
	if err != nil {
		return domain.FoodieProfilePreferences{}, err
	}
	return domain.FoodieProfilePreferences{Cuisines: cuisines, Diets: diets, Allergies: allergies}, nil
}

// Replace deletes the user's existing picks in all three tables and inserts
// prefs. Each table's delete+inserts are separate statements, not atomic on
// their own: callers MUST run this inside a domain.TxManager.WithinTx (as
// usecase/users.ReplaceFoodieProfile does) so a partial failure never leaves
// one axis replaced and another untouched.
func (r *Repository) Replace(ctx context.Context, userID uuid.UUID, prefs domain.FoodieProfilePreferences) error {
	if err := r.replaceIDs(ctx, cuisinesTable, userID, prefs.Cuisines); err != nil {
		return err
	}
	if err := r.replaceIDs(ctx, dietsTable, userID, prefs.Diets); err != nil {
		return err
	}
	if err := r.replaceIDs(ctx, allergiesTable, userID, prefs.Allergies); err != nil {
		return err
	}
	return nil
}

func (r *Repository) listIDs(ctx context.Context, t table, userID uuid.UUID) ([]string, error) {
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE user_id = $1`, t.idColumn, t.name)
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", t.name, err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list %s: %w", t.name, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", t.name, err)
	}
	return ids, nil
}

func (r *Repository) replaceIDs(ctx context.Context, t table, userID uuid.UUID, ids []string) error {
	deleteQ := fmt.Sprintf(`DELETE FROM %s WHERE user_id = $1`, t.name)
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, deleteQ, userID); err != nil {
		return fmt.Errorf("replace %s: %w", t.name, err)
	}
	insertQ := fmt.Sprintf(
		`INSERT INTO %s (user_id, %s, created_at) VALUES ($1,$2,now())
		 ON CONFLICT (user_id, %s) DO NOTHING`,
		t.name, t.idColumn, t.idColumn)
	for _, id := range ids {
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx, insertQ, userID, id); err != nil {
			return fmt.Errorf("replace %s: %w", t.name, err)
		}
	}
	return nil
}
