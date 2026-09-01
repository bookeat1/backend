// Package appversion is the Postgres implementation of
// domain.MobileAppPolicyRepository: the per-platform mobile update policy
// (migration 0103).
package appversion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository reads and writes mobile_app_policies.
type Repository struct{ pool sqltx.Querier }

// New builds the repository over a pool or an active transaction.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.MobileAppPolicyRepository = (*Repository)(nil)

const cols = `platform, min_supported_version, min_recommended_version, store_url,
	recommended_title, recommended_title_i18n, recommended_message, recommended_message_i18n,
	required_title, required_title_i18n, required_message, required_message_i18n, updated_at`

// Get reads one platform's policy. A missing row is domain.ErrNotFound and is
// an ordinary state, not a failure: the caller answers "do nothing".
func (r *Repository) Get(ctx context.Context, platform domain.DevicePlatform) (*domain.MobileAppPolicy, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM mobile_app_policies WHERE platform = $1`, string(platform))
	p, err := scanPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return p, err
}

// List returns every configured platform, ordered by platform so the admin
// screen never shows the two rows in two different orders.
func (r *Repository) List(ctx context.Context) ([]domain.MobileAppPolicy, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM mobile_app_policies ORDER BY platform ASC`)
	if err != nil {
		return nil, fmt.Errorf("list mobile app policies: %w", err)
	}
	defer rows.Close()

	out := make([]domain.MobileAppPolicy, 0, 2)
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mobile app policies: %w", err)
	}
	return out, nil
}

// Upsert writes the whole row for one platform.
//
// INSERT ... ON CONFLICT rather than "read, decide, INSERT or UPDATE": the row
// set is seeded by the migration, but a database restored without the seed (or
// a platform added later) must not turn the first save into a 404, and two
// admins saving at once must not race into a duplicate-key error. The caller
// has already merged its patch onto the stored row, so writing every column is
// the intended full write, not a read-modify-write that could revert a field.
func (r *Repository) Upsert(ctx context.Context, p *domain.MobileAppPolicy) error {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO mobile_app_policies (platform, min_supported_version, min_recommended_version, store_url,
			recommended_title, recommended_title_i18n, recommended_message, recommended_message_i18n,
			required_title, required_title_i18n, required_message, required_message_i18n, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
		 ON CONFLICT (platform) DO UPDATE SET
			min_supported_version = EXCLUDED.min_supported_version,
			min_recommended_version = EXCLUDED.min_recommended_version,
			store_url = EXCLUDED.store_url,
			recommended_title = EXCLUDED.recommended_title,
			recommended_title_i18n = EXCLUDED.recommended_title_i18n,
			recommended_message = EXCLUDED.recommended_message,
			recommended_message_i18n = EXCLUDED.recommended_message_i18n,
			required_title = EXCLUDED.required_title,
			required_title_i18n = EXCLUDED.required_title_i18n,
			required_message = EXCLUDED.required_message,
			required_message_i18n = EXCLUDED.required_message_i18n,
			updated_at = now()
		 RETURNING updated_at`,
		string(p.Platform), p.MinSupportedVersion, p.MinRecommendedVersion, p.StoreURL,
		p.RecommendedTitle, i18nToDB(p.RecommendedTitleI18n),
		p.RecommendedMessage, i18nToDB(p.RecommendedMessageI18n),
		p.RequiredTitle, i18nToDB(p.RequiredTitleI18n),
		p.RequiredMessage, i18nToDB(p.RequiredMessageI18n))
	if err := row.Scan(&p.UpdatedAt); err != nil {
		return fmt.Errorf("upsert mobile app policy: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanPolicy(row scanner) (*domain.MobileAppPolicy, error) {
	var (
		p          domain.MobileAppPolicy
		platform   string
		recTitle   []byte
		recMessage []byte
		reqTitle   []byte
		reqMessage []byte
	)
	if err := row.Scan(&platform, &p.MinSupportedVersion, &p.MinRecommendedVersion, &p.StoreURL,
		&p.RecommendedTitle, &recTitle, &p.RecommendedMessage, &recMessage,
		&p.RequiredTitle, &reqTitle, &p.RequiredMessage, &reqMessage, &p.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan mobile app policy: %w", err)
	}
	p.Platform = domain.DevicePlatform(platform)
	p.RecommendedTitleI18n = i18nFromDB(recTitle)
	p.RecommendedMessageI18n = i18nFromDB(recMessage)
	p.RequiredTitleI18n = i18nFromDB(reqTitle)
	p.RequiredMessageI18n = i18nFromDB(reqMessage)
	return &p, nil
}

func i18nToDB(m domain.I18n) any {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}

func i18nFromDB(b []byte) domain.I18n {
	if len(b) == 0 {
		return nil
	}
	var m domain.I18n
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
