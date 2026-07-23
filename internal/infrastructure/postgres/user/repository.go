// Package user is the Postgres implementation of domain.UserRepository.
package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.UserRepository = (*Repository)(nil)

// uniqueViolation is the Postgres SQLSTATE for a unique_violation.
const uniqueViolation = "23505"

const columns = `id, email, phone, full_name, role, is_active, avatar_url,
	preferred_language, city, country_code, birth_date, email_verified_at,
	phone_verified_at, deleted_at, created_at, updated_at`

func (r *Repository) Create(ctx context.Context, u *domain.User) error {
	q := `INSERT INTO users (` + columns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		u.ID, u.Email, u.Phone, u.FullName, string(u.Role), u.IsActive, u.AvatarURL,
		u.PreferredLanguage, u.City, u.CountryCode, u.BirthDate, u.EmailVerifiedAt,
		u.PhoneVerifiedAt, u.DeletedAt, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		// The email and phone columns are UNIQUE; a concurrent insert of the same
		// identity surfaces here as a unique_violation. Map it to ErrAlreadyExists
		// so callers get a clean 409 instead of a 500 (and so signup stays correct
		// under a race that slips past any pre-check).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: user", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return r.getBy(ctx, "id = $1", id)
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	return r.getBy(ctx, "email = $1", email)
}

func (r *Repository) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	return r.getBy(ctx, "phone = $1", phone)
}

func (r *Repository) getBy(ctx context.Context, where string, arg any) (*domain.User, error) {
	q := `SELECT ` + columns + ` FROM users WHERE ` + where
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, q, arg)
	u, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (r *Repository) Update(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = time.Now()
	q := `UPDATE users SET email=$2, phone=$3, full_name=$4, role=$5,
		is_active=$6, avatar_url=$7, preferred_language=$8, city=$9,
		country_code=$10, birth_date=$11, email_verified_at=$12,
		phone_verified_at=$13, updated_at=$14 WHERE id=$1`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		u.ID, u.Email, u.Phone, u.FullName, string(u.Role), u.IsActive, u.AvatarURL,
		u.PreferredLanguage, u.City, u.CountryCode, u.BirthDate, u.EmailVerifiedAt,
		u.PhoneVerifiedAt, u.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: user", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("update user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Delete soft-deletes and anonymizes the user in one UPDATE, scoped to
// deleted_at IS NULL so a repeat call is a harmless zero-row no-op rather than
// re-anonymizing already-scrubbed data. Email and phone are set to NULL, not
// to a placeholder string: both columns are UNIQUE, and Postgres treats NULL
// as distinct from every other NULL, so the freed phone/email can be reused by
// a brand-new signup immediately (see team-memory note on this decision).
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	q := `UPDATE users SET
		deleted_at = now(),
		email = NULL,
		phone = NULL,
		full_name = '',
		avatar_url = NULL,
		city = NULL,
		country_code = NULL,
		birth_date = NULL,
		is_active = false,
		updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Zero rows is ambiguous: missing id vs. already deleted. One follow-up
	// read (never gating a decision to write — the write already ran above)
	// tells them apart, same convention as payments' classifyMiss.
	_, err = r.GetByID(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	// User exists and deleted_at is already set: idempotent no-op success.
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (*domain.User, error) {
	var u domain.User
	var role string
	if err := row.Scan(&u.ID, &u.Email, &u.Phone, &u.FullName, &role,
		&u.IsActive, &u.AvatarURL, &u.PreferredLanguage, &u.City, &u.CountryCode,
		&u.BirthDate, &u.EmailVerifiedAt, &u.PhoneVerifiedAt, &u.DeletedAt,
		&u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	u.Role = domain.Role(role)
	return &u, nil
}
