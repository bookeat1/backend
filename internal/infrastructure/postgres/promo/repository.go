// Package promo is the Postgres implementation of domain.PromoRepository.
package promo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const foreignKeyViolation = "23503"

// Repository implements domain.PromoRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the promo repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.PromoRepository = (*Repository)(nil)

const selectCols = `id, restaurant_id, title, title_i18n, description, description_i18n,
	starts_at, ends_at, terms, terms_i18n, cover_image_url, discount_percent, status, created_at, updated_at, city`

// Create inserts a new promo. An unknown restaurant_id (FK violation) maps to
// ErrNotFound.
func (r *Repository) Create(ctx context.Context, p *domain.Promo) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO promos (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, terms, terms_i18n, cover_image_url, discount_percent, status, city)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING created_at, updated_at, city`,
		p.ID, p.RestaurantID, p.Title, i18nToDB(p.TitleI18n), p.Description, i18nToDB(p.DescriptionI18n),
		p.StartsAt, p.EndsAt, p.Terms, i18nToDB(p.TermsI18n), p.CoverImageURL, p.DiscountPercent, p.Status, p.City).
		Scan(&p.CreatedAt, &p.UpdatedAt, &p.City)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("create promo: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("create promo: %w", err)
	}
	return nil
}

// GetByID returns a promo by its id regardless of status.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Promo, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+selectCols+` FROM promos WHERE id = $1`, id)
	return scanPromo(row, "get promo")
}

// Update overwrites the mutable fields of an existing promo. A zero-rows UPDATE
// means the id is absent.
func (r *Repository) Update(ctx context.Context, p *domain.Promo) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE promos SET title = $2, title_i18n = $3, description = $4, description_i18n = $5,
			starts_at = $6, ends_at = $7, terms = $8, terms_i18n = $9, cover_image_url = $10,
			discount_percent = $11, status = $12, city = $13, updated_at = now()
		 WHERE id = $1`,
		p.ID, p.Title, i18nToDB(p.TitleI18n), p.Description, i18nToDB(p.DescriptionI18n),
		p.StartsAt, p.EndsAt, p.Terms, i18nToDB(p.TermsI18n), p.CoverImageURL, p.DiscountPercent,
		p.Status, p.City)
	if err != nil {
		return fmt.Errorf("update promo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update promo: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a promo. A zero-rows DELETE means the id is absent.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, `DELETE FROM promos WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete promo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete promo: %w", domain.ErrNotFound)
	}
	return nil
}

// ListByRestaurant returns a restaurant's promos for the admin cabinet,
// optionally status-filtered, newest start first with id as a stable tie-breaker.
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)
	statusArg := statusStrings(statuses)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM promos
		 WHERE restaurant_id = $1
		   AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))`,
		restaurantID, statusArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count promos: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM promos
		 WHERE restaurant_id = $1
		   AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))
		 ORDER BY starts_at DESC, id DESC
		 LIMIT $3 OFFSET $4`,
		restaurantID, statusArg, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list promos: %w", err)
	}
	return collect(rows, total)
}

// ListActive returns a restaurant's published promos whose validity window
// contains now (starts_at <= now AND ends_at > now), soonest-to-expire first
// with id as a stable tie-breaker. Matches idx_promos_active.
func (r *Repository) ListActive(ctx context.Context, restaurantID uuid.UUID, now time.Time, page, perPage int) ([]domain.Promo, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM promos
		 WHERE restaurant_id = $1 AND status = 'published' AND starts_at <= $2 AND ends_at > $2`,
		restaurantID, now).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count active promos: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM promos
		 WHERE restaurant_id = $1 AND status = 'published' AND starts_at <= $2 AND ends_at > $2
		 ORDER BY ends_at ASC, id ASC
		 LIMIT $3 OFFSET $4`,
		restaurantID, now, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list active promos: %w", err)
	}
	return collect(rows, total)
}

// ListPlatform returns the PLATFORM's own promos — the ones with no venue
// (restaurant_id IS NULL). Twin of event.Repository.ListPlatform.
func (r *Repository) ListPlatform(ctx context.Context, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)
	statusArg := statusStrings(statuses)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM promos
		 WHERE restaurant_id IS NULL
		   AND (cardinality($1::text[]) = 0 OR status = ANY($1::text[]))`,
		statusArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count platform promos: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM promos
		 WHERE restaurant_id IS NULL
		   AND (cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
		 ORDER BY starts_at DESC, id DESC
		 LIMIT $2 OFFSET $3`,
		statusArg, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list platform promos: %w", err)
	}
	return collect(rows, total)
}

// publicActiveFrom is the FROM + always-on visibility of the cross-venue promo
// listing, shared by the count and the page so the two can never disagree:
// published, inside its window, and — when it has a venue — at an active one.
// `hidden_from_home` is deliberately not applied, exactly as in the events
// listing: it hides a venue from the MAIN SCREEN, and this is a catalog read.
const publicActiveFrom = ` FROM promos p LEFT JOIN restaurants r ON r.id = p.restaurant_id
	 WHERE p.status = 'published'
	   AND p.starts_at <= $1 AND p.ends_at > $1
	   AND COALESCE(r.is_active, true) = true`

// ListPublicActive is the cross-venue public listing — the promo twin of
// event.Repository.ListPublicUpcoming, down to the city predicate. There is no
// recurrence collapse here because promos have no recurrence rules.
func (r *Repository) ListPublicActive(ctx context.Context, f domain.PublicPromoFilter, now time.Time) ([]domain.PromoListItem, int, error) {
	where := []string{}
	args := []any{now}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.City != nil {
		// The EFFECTIVE city: the promo's own override when set, otherwise its
		// venue's. A row with no effective city at all — a platform promo with
		// no override — is shown for EVERY city rather than for none, the same
		// choice events and the gastroguide collections make.
		add("(COALESCE(p.city, r.city) IS NULL OR COALESCE(p.city, r.city) = $%d)", string(*f.City))
	}
	if f.RestaurantID != nil {
		add("p.restaurant_id = $%d", *f.RestaurantID)
	}
	from := publicActiveFrom
	if len(where) > 0 {
		from += " AND " + strings.Join(where, " AND ")
	}

	q := sqltx.From(ctx, r.pool)
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count public promos: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	page, perPage := normalizePage(f.Page, f.PerPage)
	args = append(args, perPage, (page-1)*perPage)
	rows, err := q.Query(ctx, `SELECT `+ListColumns+from+`
		 ORDER BY p.ends_at ASC, p.id ASC
		 LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list public promos: %w", err)
	}
	defer rows.Close()

	var items []domain.PromoListItem
	for rows.Next() {
		it, err := ScanListItem(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan public promo: %w", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate public promos: %w", err)
	}
	return items, total, nil
}

// GetPublic reads ONE promo for the guest-facing detail page under exactly the
// listing's visibility rule — repeated in SQL rather than filtered in Go so the
// list and the detail can never disagree about the same promo.
func (r *Repository) GetPublic(ctx context.Context, id uuid.UUID, now time.Time) (*domain.PromoListItem, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+ListColumns+publicActiveFrom+` AND p.id = $2`, now, id)
	it, err := ScanListItem(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get public promo: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get public promo: %w", err)
	}
	return it, nil
}

func collect(rows pgx.Rows, total int) ([]domain.Promo, int, error) {
	defer rows.Close()
	var items []domain.Promo
	for rows.Next() {
		p, err := scanPromoRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan promo: %w", err)
		}
		items = append(items, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate promos: %w", err)
	}
	return items, total, nil
}

func scanPromo(row pgx.Row, op string) (*domain.Promo, error) {
	p, err := scanPromoRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return p, nil
}

func scanPromoRow(row pgx.Row) (*domain.Promo, error) {
	var p domain.Promo
	var titleI18n, descI18n, termsI18n []byte
	if err := row.Scan(&p.ID, &p.RestaurantID, &p.Title, &titleI18n, &p.Description, &descI18n,
		&p.StartsAt, &p.EndsAt, &p.Terms, &termsI18n, &p.CoverImageURL, &p.DiscountPercent, &p.Status,
		&p.CreatedAt, &p.UpdatedAt, &p.City); err != nil {
		return nil, err
	}
	p.TitleI18n = i18nFromDB(titleI18n)
	p.DescriptionI18n = i18nFromDB(descI18n)
	p.TermsI18n = i18nFromDB(termsI18n)
	return &p, nil
}

func statusStrings(statuses []domain.PromoStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, string(s))
	}
	return out
}

func normalizePage(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	return page, perPage
}

func i18nToDB(m domain.I18n) any {
	if m == nil {
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

// ReplaceImages rewrites a promo's gallery — see the event repository's twin
// for why this is a delete-then-insert and not a row-by-row diff.
func (r *Repository) ReplaceImages(ctx context.Context, promoID uuid.UUID, urls []string) error {
	q := sqltx.From(ctx, r.pool)
	if _, err := q.Exec(ctx, `DELETE FROM promo_images WHERE promo_id=$1`, promoID); err != nil {
		return fmt.Errorf("clear promo images: %w", err)
	}
	for i, url := range urls {
		if _, err := q.Exec(ctx,
			`INSERT INTO promo_images (id, promo_id, image_url, position) VALUES ($1,$2,$3,$4)`,
			uuid.New(), promoID, url, i); err != nil {
			return fmt.Errorf("insert promo image: %w", err)
		}
	}
	return nil
}

// ImagesByPromo loads the galleries of several promos in one query.
func (r *Repository) ImagesByPromo(ctx context.Context, promoIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(promoIDs))
	if len(promoIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT promo_id, image_url FROM promo_images
		 WHERE promo_id = ANY($1)
		 ORDER BY promo_id, position, created_at`, promoIDs)
	if err != nil {
		return nil, fmt.Errorf("list promo images: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var url string
		if err := rows.Scan(&id, &url); err != nil {
			return nil, fmt.Errorf("scan promo image: %w", err)
		}
		out[id] = append(out[id], url)
	}
	return out, rows.Err()
}

// ListColumns is the `p.`-qualified promo column list followed by the host
// venue's identity (`r.name, r.name_i18n, r.city`) — exactly what ScanListItem
// expects, in that order. Exported together with ScanListItem so a sibling
// package that joins through `promos p JOIN restaurants r` (the favorites read)
// selects the same shape rather than duplicating it. Mirrors event.ListColumns.
const ListColumns = `p.id, p.restaurant_id, p.title, p.title_i18n, p.description, p.description_i18n,
	p.starts_at, p.ends_at, p.terms, p.terms_i18n, p.cover_image_url, p.discount_percent, p.status,
	p.created_at, p.updated_at, p.city AS promo_city,
	r.name, r.name_i18n, r.city`

// ScanListItem scans one row shaped like ListColumns into a PromoListItem.
// Every venue column is a POINTER for the same reason as in the event package:
// the venue is LEFT-joined since migration 0085, and a platform promo brings
// back three NULLs that must become "no venue", not a failed scan.
func ScanListItem(row pgx.Row) (*domain.PromoListItem, error) {
	var it domain.PromoListItem
	p := &it.Promo
	var titleI18n, descI18n, termsI18n, venueNameI18n []byte
	var venueName *string
	var venueCity *domain.City
	if err := row.Scan(&p.ID, &p.RestaurantID, &p.Title, &titleI18n, &p.Description, &descI18n,
		&p.StartsAt, &p.EndsAt, &p.Terms, &termsI18n, &p.CoverImageURL, &p.DiscountPercent, &p.Status,
		&p.CreatedAt, &p.UpdatedAt, &p.City,
		&venueName, &venueNameI18n, &venueCity); err != nil {
		return nil, err
	}
	p.TitleI18n = i18nFromDB(titleI18n)
	p.DescriptionI18n = i18nFromDB(descI18n)
	p.TermsI18n = i18nFromDB(termsI18n)
	it.Restaurant = venueFromDB(p.RestaurantID, venueName, venueNameI18n, venueCity)
	return &it, nil
}

// venueFromDB builds the card's venue block, or nil when the promo has no
// venue. Twin of the event package's function of the same name.
func venueFromDB(restaurantID *uuid.UUID, name *string, nameI18n []byte, city *domain.City) *domain.EventRestaurant {
	if restaurantID == nil {
		return nil
	}
	v := domain.EventRestaurant{ID: *restaurantID, NameI18n: i18nFromDB(nameI18n)}
	if name != nil {
		v.Name = *name
	}
	if city != nil {
		v.City = *city
	}
	return &v
}
