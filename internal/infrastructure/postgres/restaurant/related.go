package restaurant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/schedule"
	"backend-core/internal/infrastructure/sqltx"
)

// Related implements domain.RestaurantRelatedRepository.
type Related struct{ pool sqltx.Querier }

func NewRelated(pool sqltx.Querier) *Related { return &Related{pool: pool} }

var _ domain.RestaurantRelatedRepository = (*Related)(nil)

func (r *Related) ListImages(ctx context.Context, rid uuid.UUID) ([]domain.Image, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, image_url, is_primary, created_at
		 FROM restaurant_images WHERE restaurant_id=$1 ORDER BY is_primary DESC, created_at`, rid)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	defer rows.Close()
	var out []domain.Image
	for rows.Next() {
		var i domain.Image
		if err := rows.Scan(&i.ID, &i.RestaurantID, &i.ImageURL, &i.IsPrimary, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (r *Related) ListTags(ctx context.Context, rid uuid.UUID) ([]domain.Tag, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, tag_name, tag_name_i18n, created_at
		 FROM restaurant_tags WHERE restaurant_id=$1 ORDER BY created_at`, rid)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var out []domain.Tag
	for rows.Next() {
		var tg domain.Tag
		var i18n []byte
		if err := rows.Scan(&tg.ID, &tg.RestaurantID, &tg.TagName, &i18n, &tg.CreatedAt); err != nil {
			return nil, err
		}
		tg.TagNameI18n = i18nFromDB(i18n)
		out = append(out, tg)
	}
	return out, rows.Err()
}

func (r *Related) ListSocialLinks(ctx context.Context, rid uuid.UUID) ([]domain.SocialLink, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, type, url, created_at
		 FROM restaurant_social_links WHERE restaurant_id=$1 ORDER BY created_at`, rid)
	if err != nil {
		return nil, fmt.Errorf("list social links: %w", err)
	}
	defer rows.Close()
	var out []domain.SocialLink
	for rows.Next() {
		var s domain.SocialLink
		if err := rows.Scan(&s.ID, &s.RestaurantID, &s.Type, &s.URL, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Related) ListWorkingHours(ctx context.Context, rid uuid.UUID) ([]domain.WorkingHours, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, day_of_week, open_time, close_time, is_open, created_at, updated_at
		 FROM restaurant_working_hours WHERE restaurant_id=$1 ORDER BY day_of_week`, rid)
	if err != nil {
		return nil, fmt.Errorf("list working hours: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkingHours
	for rows.Next() {
		var w domain.WorkingHours
		if err := rows.Scan(&w.ID, &w.RestaurantID, &w.DayOfWeek, &w.OpenTime, &w.CloseTime, &w.IsOpen, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *Related) ListTimeSlots(ctx context.Context, rid uuid.UUID) ([]domain.TimeSlot, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, day_of_week, start_time, end_time, is_manually_disabled, created_at, updated_at
		 FROM restaurant_time_slots WHERE restaurant_id=$1 ORDER BY day_of_week, start_time`, rid)
	if err != nil {
		return nil, fmt.Errorf("list time slots: %w", err)
	}
	defer rows.Close()
	var out []domain.TimeSlot
	for rows.Next() {
		var s domain.TimeSlot
		if err := rows.Scan(&s.ID, &s.RestaurantID, &s.DayOfWeek, &s.StartTime, &s.EndTime, &s.IsManuallyDisabled, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Related) ListTables(ctx context.Context, rid uuid.UUID) ([]domain.RestaurantTable, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, name, capacity, description, is_active, created_at, updated_at
		 FROM restaurant_tables WHERE restaurant_id=$1 ORDER BY name`, rid)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []domain.RestaurantTable
	for rows.Next() {
		var tb domain.RestaurantTable
		if err := rows.Scan(&tb.ID, &tb.RestaurantID, &tb.Name, &tb.Capacity, &tb.Description, &tb.IsActive, &tb.CreatedAt, &tb.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, tb)
	}
	return out, rows.Err()
}

func (r *Related) GetFloorPlan(ctx context.Context, rid uuid.UUID) (*domain.FloorPlan, error) {
	var fp domain.FloorPlan
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT id, restaurant_id, layout_data, created_at, updated_at
		 FROM restaurant_floor_plans WHERE restaurant_id=$1`, rid).
		Scan(&fp.ID, &fp.RestaurantID, &fp.LayoutData, &fp.CreatedAt, &fp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get floor plan: %w", err)
	}
	return &fp, nil
}

// --- Replace* : delete-all-then-insert within the caller's transaction ---

func (r *Related) del(ctx context.Context, table string, rid uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx, `DELETE FROM `+table+` WHERE restaurant_id=$1`, rid)
	return err
}

func (r *Related) ReplaceImages(ctx context.Context, rid uuid.UUID, items []domain.Image) error {
	if err := r.del(ctx, "restaurant_images", rid); err != nil {
		return fmt.Errorf("replace images: %w", err)
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_images (id, restaurant_id, image_url, is_primary, created_at)
			 VALUES ($1,$2,$3,$4,now())`, items[i].ID, rid, items[i].ImageURL, items[i].IsPrimary); err != nil {
			return fmt.Errorf("replace images: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceTags(ctx context.Context, rid uuid.UUID, items []domain.Tag) error {
	if err := r.del(ctx, "restaurant_tags", rid); err != nil {
		return fmt.Errorf("replace tags: %w", err)
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_tags (id, restaurant_id, tag_name, tag_name_i18n, created_at)
			 VALUES ($1,$2,$3,$4,now())`, items[i].ID, rid, items[i].TagName, i18nToDB(items[i].TagNameI18n)); err != nil {
			return fmt.Errorf("replace tags: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceSocialLinks(ctx context.Context, rid uuid.UUID, items []domain.SocialLink) error {
	if err := r.del(ctx, "restaurant_social_links", rid); err != nil {
		return fmt.Errorf("replace social links: %w", err)
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_social_links (id, restaurant_id, type, url, created_at)
			 VALUES ($1,$2,$3,$4,now())`, items[i].ID, rid, items[i].Type, items[i].URL); err != nil {
			return fmt.Errorf("replace social links: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceWorkingHours(ctx context.Context, rid uuid.UUID, items []domain.WorkingHours) error {
	// The same per-venue advisory lock the legacy-hours import takes (see
	// legacysync.Sink.LoadWorkingHours). Only one of the two writers may hold a
	// venue's hours at a time, otherwise the import can delete a venue's edit
	// microseconds after it landed. Outside a transaction this lock is a no-op
	// beyond its own statement, which is exactly right: without a surrounding
	// tx there is no window to protect.
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, rid.String()); err != nil {
		return fmt.Errorf("replace working hours: lock venue %s: %w", rid, err)
	}
	if err := r.del(ctx, "restaurant_working_hours", rid); err != nil {
		return fmt.Errorf("replace working hours: %w", err)
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, items[i].ID, rid, items[i].DayOfWeek, items[i].OpenTime, items[i].CloseTime, items[i].IsOpen); err != nil {
			return fmt.Errorf("replace working hours: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceTimeSlots(ctx context.Context, rid uuid.UUID, items []domain.TimeSlot) error {
	if err := r.del(ctx, "restaurant_time_slots", rid); err != nil {
		return fmt.Errorf("replace time slots: %w", err)
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_time_slots (id, restaurant_id, day_of_week, start_time, end_time, is_manually_disabled, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, items[i].ID, rid, items[i].DayOfWeek, items[i].StartTime, items[i].EndTime, items[i].IsManuallyDisabled); err != nil {
			return fmt.Errorf("replace time slots: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceTables(ctx context.Context, rid uuid.UUID, items []domain.RestaurantTable) error {
	if err := r.del(ctx, "restaurant_tables", rid); err != nil {
		return fmt.Errorf("replace tables: %w", err)
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, description, is_active, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, items[i].ID, rid, items[i].Name, items[i].Capacity, items[i].Description, items[i].IsActive); err != nil {
			return fmt.Errorf("replace tables: %w", err)
		}
	}
	return nil
}

func (r *Related) UpsertFloorPlan(ctx context.Context, fp *domain.FloorPlan) error {
	if fp.ID == uuid.Nil {
		fp.ID = uuid.New()
	}
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_floor_plans (id, restaurant_id, layout_data, created_at, updated_at)
		 VALUES ($1,$2,$3,now(),now())
		 ON CONFLICT (restaurant_id) DO UPDATE SET layout_data=EXCLUDED.layout_data, updated_at=now()`,
		fp.ID, fp.RestaurantID, []byte(fp.LayoutData))
	if err != nil {
		return fmt.Errorf("upsert floor plan: %w", err)
	}
	return nil
}

// Categories implements domain.RestaurantCategoryRepository.
type Categories struct{ pool sqltx.Querier }

func NewCategories(pool sqltx.Querier) *Categories { return &Categories{pool: pool} }

var _ domain.RestaurantCategoryRepository = (*Categories)(nil)

func (c *Categories) List(ctx context.Context) ([]domain.RestaurantCategory, error) {
	rows, err := sqltx.From(ctx, c.pool).Query(ctx,
		`SELECT id, name, name_i18n, description, description_i18n, created_at
		 FROM restaurant_categories ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	defer rows.Close()
	var out []domain.RestaurantCategory
	for rows.Next() {
		var cat domain.RestaurantCategory
		var nI18n, dI18n []byte
		if err := rows.Scan(&cat.ID, &cat.Name, &nI18n, &cat.Description, &dI18n, &cat.CreatedAt); err != nil {
			return nil, err
		}
		cat.NameI18n = i18nFromDB(nI18n)
		cat.DescriptionI18n = i18nFromDB(dI18n)
		out = append(out, cat)
	}
	return out, rows.Err()
}

func (c *Categories) Create(ctx context.Context, cat *domain.RestaurantCategory) error {
	if cat.ID == uuid.Nil {
		cat.ID = uuid.New()
	}
	cat.CreatedAt = time.Now()
	_, err := sqltx.From(ctx, c.pool).Exec(ctx,
		`INSERT INTO restaurant_categories (id, name, name_i18n, description, description_i18n, created_at)
		 VALUES ($1,$2,$3,$4,$5,now())`,
		cat.ID, cat.Name, i18nToDB(cat.NameI18n), cat.Description, i18nToDB(cat.DescriptionI18n))
	if err != nil {
		return mapWrite(err, "create category")
	}
	return nil
}

// Managers implements domain.RestaurantManagerRepository.
type Managers struct{ pool sqltx.Querier }

func NewManagers(pool sqltx.Querier) *Managers { return &Managers{pool: pool} }

var _ domain.RestaurantManagerRepository = (*Managers)(nil)

func (m *Managers) scanRows(rows pgx.Rows) ([]domain.RestaurantManager, error) {
	defer rows.Close()
	var out []domain.RestaurantManager
	for rows.Next() {
		var mn domain.RestaurantManager
		var role string
		if err := rows.Scan(&mn.ID, &mn.RestaurantID, &mn.UserID, &role, &mn.CreatedBy, &mn.WhatsappOptIn, &mn.WhatsappPhone, &mn.CreatedAt); err != nil {
			return nil, err
		}
		mn.Role = domain.StaffRole(role)
		out = append(out, mn)
	}
	return out, rows.Err()
}

const mgrCols = `id, restaurant_id, user_id, role, created_by, whatsapp_opt_in, whatsapp_phone, created_at`

func (m *Managers) ListByRestaurant(ctx context.Context, rid uuid.UUID) ([]domain.RestaurantManager, error) {
	rows, err := sqltx.From(ctx, m.pool).Query(ctx,
		`SELECT `+mgrCols+` FROM restaurant_managers WHERE restaurant_id=$1 ORDER BY created_at`, rid)
	if err != nil {
		return nil, fmt.Errorf("list managers: %w", err)
	}
	return m.scanRows(rows)
}

func (m *Managers) ListByUser(ctx context.Context, uid uuid.UUID) ([]domain.RestaurantManager, error) {
	rows, err := sqltx.From(ctx, m.pool).Query(ctx,
		`SELECT `+mgrCols+` FROM restaurant_managers WHERE user_id=$1 ORDER BY created_at`, uid)
	if err != nil {
		return nil, fmt.Errorf("list managers by user: %w", err)
	}
	return m.scanRows(rows)
}

// ListMembershipsByUser returns every restaurant uid is staff of, joined to the
// venue's display name, ordered by name. It is the read behind
// GET /admin/my-restaurants: one query scoped to the caller's own user id, so a
// caller can never see a restaurant they have no membership in. Returns an
// empty (non-nil-safe) slice, not an error, when the user manages nothing.
func (m *Managers) ListMembershipsByUser(ctx context.Context, uid uuid.UUID) ([]domain.StaffMembership, error) {
	rows, err := sqltx.From(ctx, m.pool).Query(ctx,
		`SELECT rm.restaurant_id, r.name, r.name_i18n, rm.role
		 FROM restaurant_managers rm
		 JOIN restaurants r ON r.id = rm.restaurant_id
		 WHERE rm.user_id=$1
		 ORDER BY r.name`, uid)
	if err != nil {
		return nil, fmt.Errorf("list memberships by user: %w", err)
	}
	defer rows.Close()
	var out []domain.StaffMembership
	for rows.Next() {
		var sm domain.StaffMembership
		var role string
		var nameI18n []byte
		if err := rows.Scan(&sm.RestaurantID, &sm.Name, &nameI18n, &role); err != nil {
			return nil, err
		}
		sm.NameI18n = i18nFromDB(nameI18n)
		sm.Role = domain.StaffRole(role)
		out = append(out, sm)
	}
	return out, rows.Err()
}

func (m *Managers) GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantManager, error) {
	row := sqltx.From(ctx, m.pool).QueryRow(ctx,
		`SELECT `+mgrCols+` FROM restaurant_managers WHERE id=$1`, id)
	var mn domain.RestaurantManager
	var role string
	err := row.Scan(&mn.ID, &mn.RestaurantID, &mn.UserID, &role, &mn.CreatedBy, &mn.WhatsappOptIn, &mn.WhatsappPhone, &mn.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get manager: %w", err)
	}
	mn.Role = domain.StaffRole(role)
	return &mn, nil
}

func (m *Managers) Create(ctx context.Context, mn *domain.RestaurantManager) error {
	if mn.ID == uuid.Nil {
		mn.ID = uuid.New()
	}
	mn.CreatedAt = time.Now()
	_, err := sqltx.From(ctx, m.pool).Exec(ctx,
		`INSERT INTO restaurant_managers (`+mgrCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,now())`,
		mn.ID, mn.RestaurantID, mn.UserID, string(mn.Role), mn.CreatedBy, mn.WhatsappOptIn, mn.WhatsappPhone)
	if err != nil {
		return mapWrite(err, "create manager")
	}
	return nil
}

func (m *Managers) UpdateRole(ctx context.Context, id uuid.UUID, role domain.StaffRole) error {
	tag, err := sqltx.From(ctx, m.pool).Exec(ctx,
		`UPDATE restaurant_managers SET role=$2 WHERE id=$1`, id, string(role))
	if err != nil {
		return mapWrite(err, "update manager role")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (m *Managers) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, m.pool).Exec(ctx, `DELETE FROM restaurant_managers WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete manager: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Partnership implements domain.PartnershipRequestRepository.
type Partnership struct{ pool sqltx.Querier }

func NewPartnership(pool sqltx.Querier) *Partnership { return &Partnership{pool: pool} }

var _ domain.PartnershipRequestRepository = (*Partnership)(nil)

func (p *Partnership) Create(ctx context.Context, req *domain.PartnershipRequest) error {
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	req.CreatedAt = time.Now()
	_, err := sqltx.From(ctx, p.pool).Exec(ctx,
		`INSERT INTO restaurant_partnership_requests
		 (id, restaurant_name, contact_name, email, phone, address, cuisine_type, description, additional_info, status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now(),now())`,
		req.ID, req.RestaurantName, req.ContactName, req.Email, req.Phone, req.Address,
		req.CuisineType, req.Description, req.AdditionalInfo, req.Status)
	if err != nil {
		return mapWrite(err, "create partnership request")
	}
	return nil
}

// WorkingHoursFor reads the weekly working hours of MANY venues in one query,
// so a catalog page costs one round-trip instead of one per restaurant. A venue
// with no rows is absent from the map — the caller must keep telling "hours
// unknown" apart from "closed" (see usecase/restaurants.buildVenueState).
func (r *Related) WorkingHoursFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.WorkingHours, error) {
	out := make(map[uuid.UUID][]domain.WorkingHours, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, day_of_week, open_time, close_time, is_open, created_at, updated_at
		 FROM restaurant_working_hours WHERE restaurant_id = ANY($1)
		 ORDER BY restaurant_id, day_of_week`, ids)
	if err != nil {
		return nil, fmt.Errorf("list working hours for venues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var w domain.WorkingHours
		if err := rows.Scan(&w.ID, &w.RestaurantID, &w.DayOfWeek, &w.OpenTime, &w.CloseTime, &w.IsOpen, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan working hours: %w", err)
		}
		out[w.RestaurantID] = append(out[w.RestaurantID], w)
	}
	return out, rows.Err()
}

// ListScheduleOverrides returns one venue's special-day exceptions for the
// calendar dates in [from, to], for the availability engine
// (usecase/bookings.loadSchedule) — the same rows the admin panel writes and the
// money path reads.
//
// The window is what keeps this read flat: it runs on every availability,
// create and update call, and the engine only ever resolves the dates it was
// asked about. The venue's full history stays available to the admin cabinet
// through the unbounded ListByRestaurant.
//
// It delegates to the schedule repository, which OWNS this table: the query,
// the column order and the scan live in exactly one place, so a column added
// there (as 0036 added the paid-day pair) cannot reach one reader and miss
// another.
func (r *Related) ListScheduleOverrides(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.ScheduleOverride, error) {
	return schedule.New(r.pool).ListByRestaurantBetween(ctx, restaurantID, from, to)
}

// ScheduleOverridesFor is the batch read behind the public catalog: the
// special days of MANY venues inside a date window, in one round-trip.
func (r *Related) ScheduleOverridesFor(ctx context.Context, ids []uuid.UUID, from, to time.Time) (map[uuid.UUID][]domain.ScheduleOverride, error) {
	return schedule.New(r.pool).ListForVenues(ctx, ids, from, to)
}

// BookableTableCountsFor counts, per venue, the tables that could actually seat
// a party: is_active AND capacity > 0 — exactly the filter the availability
// engine applies (usecase/bookings.loadSchedule). A venue with no such table is
// absent from the map (count 0), which is how "Adept" is recognised as unable
// to accept online bookings at all.
func (r *Related) BookableTableCountsFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	out := make(map[uuid.UUID]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT restaurant_id, count(*) FROM restaurant_tables
		 WHERE restaurant_id = ANY($1) AND is_active AND capacity > 0
		 GROUP BY restaurant_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("count bookable tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan bookable table count: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// TimeSlotsFor and TablesFor are the batch twins of ListTimeSlots/ListTables,
// for the catalog's availability filter (usecase/bookings.AvailabilitySearch).
// One query per collection whatever the number of venues: the filter runs over
// a whole catalog page, and per-venue reads there grow without bound.
//
// Unlike BookableTableCountsFor, TablesFor returns the tables THEMSELVES,
// inactive ones included — the availability engine applies its own is_active /
// capacity filter, and it must stay the one place that rule lives.
func (r *Related) TimeSlotsFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.TimeSlot, error) {
	out := make(map[uuid.UUID][]domain.TimeSlot, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, day_of_week, start_time, end_time, is_manually_disabled, created_at, updated_at
		 FROM restaurant_time_slots WHERE restaurant_id = ANY($1)
		 ORDER BY restaurant_id, day_of_week, start_time`, ids)
	if err != nil {
		return nil, fmt.Errorf("list time slots for venues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s domain.TimeSlot
		if err := rows.Scan(&s.ID, &s.RestaurantID, &s.DayOfWeek, &s.StartTime, &s.EndTime,
			&s.IsManuallyDisabled, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan time slot: %w", err)
		}
		out[s.RestaurantID] = append(out[s.RestaurantID], s)
	}
	return out, rows.Err()
}

func (r *Related) TablesFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.RestaurantTable, error) {
	out := make(map[uuid.UUID][]domain.RestaurantTable, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, name, capacity, description, is_active, created_at, updated_at
		 FROM restaurant_tables WHERE restaurant_id = ANY($1)
		 ORDER BY restaurant_id, name`, ids)
	if err != nil {
		return nil, fmt.Errorf("list tables for venues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tb domain.RestaurantTable
		if err := rows.Scan(&tb.ID, &tb.RestaurantID, &tb.Name, &tb.Capacity, &tb.Description,
			&tb.IsActive, &tb.CreatedAt, &tb.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan table: %w", err)
		}
		out[tb.RestaurantID] = append(out[tb.RestaurantID], tb)
	}
	return out, rows.Err()
}
