// Package user is the Postgres implementation of domain.UserRepository.
package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.DBTX }

func New(pool sqltx.DBTX) *Repository { return &Repository{pool: pool} }

const columns = `id, email, phone, full_name, role, is_active, avatar_url,
	preferred_language, city, email_verified_at, phone_verified_at,
	created_at, updated_at`

func (r *Repository) Create(ctx context.Context, u *domain.User) error {
	q := `INSERT INTO users (` + columns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q,
		u.ID, u.Email, u.Phone, u.FullName, string(u.Role), u.IsActive, u.AvatarURL,
		u.PreferredLanguage, u.City, u.EmailVerifiedAt, u.PhoneVerifiedAt,
		u.CreatedAt, u.UpdatedAt)
	if err != nil {
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
	row := sqltx.From(ctx, r.pool).QueryRowContext(ctx, q, arg)
	u, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
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
		email_verified_at=$10, phone_verified_at=$11, updated_at=$12 WHERE id=$1`
	res, err := sqltx.From(ctx, r.pool).ExecContext(ctx, q,
		u.ID, u.Email, u.Phone, u.FullName, string(u.Role), u.IsActive, u.AvatarURL,
		u.PreferredLanguage, u.City, u.EmailVerifiedAt, u.PhoneVerifiedAt, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (*domain.User, error) {
	var u domain.User
	var role string
	if err := row.Scan(&u.ID, &u.Email, &u.Phone, &u.FullName, &role,
		&u.IsActive, &u.AvatarURL, &u.PreferredLanguage, &u.City, &u.EmailVerifiedAt,
		&u.PhoneVerifiedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	u.Role = domain.Role(role)
	return &u, nil
}
