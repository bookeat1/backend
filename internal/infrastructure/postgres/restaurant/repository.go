// Package restaurant is the Postgres implementation of the restaurant
// repositories.
package restaurant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const uniqueViolation = "23505"

// Repository implements domain.RestaurantRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the restaurant repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.RestaurantRepository = (*Repository)(nil)

const cols = `id, category_id, name, name_i18n, description, description_i18n,
	cuisine_type, cuisine_type_i18n, address, address_i18n, opening_hours,
	opening_hours_i18n, city, price_category, email, phone, latitude, longitude,
	kwaaka_restaurant_id, is_active, is_new, is_popular, is_premium,
	hidden_from_home, display_order, created_at, updated_at`

// policyCols are the venue's booking-policy overrides (all NULLABLE — NULL
// means "use the global default"). They are read only by GetByID: the policy is
// resolved per booking, and the catalog listing has no use for them. They are
// deliberately absent from cols so the Create/Update placeholder numbering
// stays untouched.
const policyCols = `timezone, booking_duration_minutes, booking_buffer_minutes,
	booking_lead_minutes, booking_horizon_days, cancel_deadline_minutes,
	confirm_sla_minutes, max_guests_per_booking, auto_confirm`

func (r *Repository) Create(ctx context.Context, m *domain.Restaurant) error {
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	q := `INSERT INTO restaurants (` + cols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`
	_, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(m)...)
	if err != nil {
		return mapWrite(err, "create restaurant")
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, m *domain.Restaurant) error {
	m.UpdatedAt = time.Now()
	q := `UPDATE restaurants SET category_id=$2, name=$3, name_i18n=$4, description=$5,
		description_i18n=$6, cuisine_type=$7, cuisine_type_i18n=$8, address=$9,
		address_i18n=$10, opening_hours=$11, opening_hours_i18n=$12, city=$13,
		price_category=$14, email=$15, phone=$16, latitude=$17, longitude=$18,
		kwaaka_restaurant_id=$19, is_active=$20, is_new=$21, is_popular=$22,
		is_premium=$23, hidden_from_home=$24, display_order=$25, updated_at=$26
		WHERE id=$1`
	// Built explicitly (not sliced out of r.args) so adding an INSERT column
	// can't silently shift the UPDATE placeholders out of alignment. Update
	// intentionally omits created_at.
	args := []any{
		m.ID, m.CategoryID, m.Name, i18nToDB(m.NameI18n), m.Description,
		i18nToDB(m.DescriptionI18n), m.CuisineType, i18nToDB(m.CuisineTypeI18n),
		m.Address, i18nToDB(m.AddressI18n), m.OpeningHours, i18nToDB(m.OpeningHoursI18n),
		string(m.City), string(m.PriceCategory), m.Email, m.Phone, m.Latitude, m.Longitude,
		m.KwaakaRestaurantID, m.IsActive, m.IsNew, m.IsPopular, m.IsPremium,
		m.HiddenFromHome, m.DisplayOrder, m.UpdatedAt,
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return mapWrite(err, "update restaurant")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) SetActive(ctx context.Context, id uuid.UUID, active bool) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurants SET is_active=$2, updated_at=now() WHERE id=$1`, id, active)
	if err != nil {
		return fmt.Errorf("set active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+`, `+policyCols+` FROM restaurants WHERE id=$1`, id)
	base, err := scanRestaurantWithPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get restaurant: %w", err)
	}
	agg := &domain.RestaurantAggregate{Restaurant: *base}
	rel := &Related{pool: r.pool}
	if agg.Images, err = rel.ListImages(ctx, id); err != nil {
		return nil, err
	}
	if agg.Features, err = rel.ListFeatures(ctx, id); err != nil {
		return nil, err
	}
	if agg.Tags, err = rel.ListTags(ctx, id); err != nil {
		return nil, err
	}
	if agg.SocialLinks, err = rel.ListSocialLinks(ctx, id); err != nil {
		return nil, err
	}
	return agg, nil
}

func (r *Repository) ListActive(ctx context.Context, f domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	where := []string{"r.is_active = true"}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.City != nil {
		add("r.city = $%d", string(*f.City))
	}
	if f.Category != nil {
		add("r.category_id = $%d", *f.Category)
	}
	if f.IsPopular != nil {
		add("r.is_popular = $%d", *f.IsPopular)
	}
	if f.IsNew != nil {
		add("r.is_new = $%d", *f.IsNew)
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		// Escape LIKE wildcards so a term containing % or _ matches literally
		// instead of turning "%" into a match-everything filter.
		add(`r.name ILIKE '%%' || $%d || '%%' ESCAPE '\'`, escapeLike(s))
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM restaurants r WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count restaurants: %w", err)
	}

	page, perPage := f.Page, f.PerPage
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}
	args = append(args, perPage, (page-1)*perPage)
	q := `SELECT ` + prefixed(cols, "r") + `,
		(SELECT image_url FROM restaurant_images i WHERE i.restaurant_id = r.id
		 ORDER BY i.is_primary DESC, i.created_at ASC LIMIT 1) AS primary_image
		FROM restaurants r WHERE ` + whereSQL + `
		ORDER BY r.display_order ASC NULLS LAST, r.name ASC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list restaurants: %w", err)
	}
	defer rows.Close()

	var items []domain.RestaurantListItem
	for rows.Next() {
		base, primary, err := scanListItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, domain.RestaurantListItem{Restaurant: *base, PrimaryImage: primary})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list restaurants: %w", err)
	}
	return items, total, nil
}

func (r *Repository) args(m *domain.Restaurant) []any {
	return []any{
		m.ID, m.CategoryID, m.Name, i18nToDB(m.NameI18n), m.Description,
		i18nToDB(m.DescriptionI18n), m.CuisineType, i18nToDB(m.CuisineTypeI18n),
		m.Address, i18nToDB(m.AddressI18n), m.OpeningHours, i18nToDB(m.OpeningHoursI18n),
		string(m.City), string(m.PriceCategory), m.Email, m.Phone, m.Latitude, m.Longitude,
		m.KwaakaRestaurantID, m.IsActive, m.IsNew, m.IsPopular, m.IsPremium,
		m.HiddenFromHome, m.DisplayOrder, m.CreatedAt, m.UpdatedAt,
	}
}

type scanner interface{ Scan(dest ...any) error }

func scanRestaurant(row scanner) (*domain.Restaurant, error) {
	var m domain.Restaurant
	var city, price string
	var name, desc, cuisine, addr, opening []byte
	if err := row.Scan(
		&m.ID, &m.CategoryID, &m.Name, &name, &m.Description, &desc,
		&m.CuisineType, &cuisine, &m.Address, &addr, &m.OpeningHours, &opening,
		&city, &price, &m.Email, &m.Phone, &m.Latitude, &m.Longitude,
		&m.KwaakaRestaurantID, &m.IsActive, &m.IsNew, &m.IsPopular, &m.IsPremium,
		&m.HiddenFromHome, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.City = domain.City(city)
	m.PriceCategory = domain.PriceCategory(price)
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CuisineTypeI18n = i18nFromDB(cuisine)
	m.AddressI18n = i18nFromDB(addr)
	m.OpeningHoursI18n = i18nFromDB(opening)
	return &m, nil
}

// scanRestaurantWithPolicy scans the base columns plus the booking-policy
// overrides. Without it every venue would silently fall back to the env
// defaults and restaurant-level auto_confirm / SLA settings would be ignored.
func scanRestaurantWithPolicy(row scanner) (*domain.Restaurant, error) {
	var m domain.Restaurant
	var city, price string
	var name, desc, cuisine, addr, opening []byte
	p := &m.BookingPolicy
	if err := row.Scan(
		&m.ID, &m.CategoryID, &m.Name, &name, &m.Description, &desc,
		&m.CuisineType, &cuisine, &m.Address, &addr, &m.OpeningHours, &opening,
		&city, &price, &m.Email, &m.Phone, &m.Latitude, &m.Longitude,
		&m.KwaakaRestaurantID, &m.IsActive, &m.IsNew, &m.IsPopular, &m.IsPremium,
		&m.HiddenFromHome, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt,
		&p.Timezone, &p.BookingDurationMinutes, &p.BookingBufferMinutes,
		&p.BookingLeadMinutes, &p.BookingHorizonDays, &p.CancelDeadlineMinutes,
		&p.ConfirmSLAMinutes, &p.MaxGuestsPerBooking, &p.AutoConfirm,
	); err != nil {
		return nil, err
	}
	m.City = domain.City(city)
	m.PriceCategory = domain.PriceCategory(price)
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CuisineTypeI18n = i18nFromDB(cuisine)
	m.AddressI18n = i18nFromDB(addr)
	m.OpeningHoursI18n = i18nFromDB(opening)
	return &m, nil
}

func scanListItem(row scanner) (*domain.Restaurant, *string, error) {
	var m domain.Restaurant
	var city, price string
	var name, desc, cuisine, addr, opening []byte
	var primary *string
	if err := row.Scan(
		&m.ID, &m.CategoryID, &m.Name, &name, &m.Description, &desc,
		&m.CuisineType, &cuisine, &m.Address, &addr, &m.OpeningHours, &opening,
		&city, &price, &m.Email, &m.Phone, &m.Latitude, &m.Longitude,
		&m.KwaakaRestaurantID, &m.IsActive, &m.IsNew, &m.IsPopular, &m.IsPremium,
		&m.HiddenFromHome, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt, &primary,
	); err != nil {
		return nil, nil, err
	}
	m.City = domain.City(city)
	m.PriceCategory = domain.PriceCategory(price)
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CuisineTypeI18n = i18nFromDB(cuisine)
	m.AddressI18n = i18nFromDB(addr)
	m.OpeningHoursI18n = i18nFromDB(opening)
	return &m, primary, nil
}

// escapeLike escapes the LIKE/ILIKE metacharacters (backslash first) so a
// user-supplied term is matched literally under `ESCAPE '\'`.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// prefixed rewrites a bare column list into a table-qualified one.
func prefixed(colList, alias string) string {
	parts := strings.Split(colList, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
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

// mapWrite maps a unique_violation to domain.ErrAlreadyExists, otherwise wraps
// err with resource for context. resource should name the entity/operation
// being written (e.g. "create restaurant", "create manager").
func mapWrite(err error, resource string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, resource)
	}
	return fmt.Errorf("%s: %w", resource, err)
}
