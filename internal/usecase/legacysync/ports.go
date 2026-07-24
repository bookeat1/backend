// Package legacysync is the one-way data sync that pulls the OLD BookEat system
// (the still-live Supabase Postgres that guests keep booking on during the
// "Вариант Б" transition) INTO this backend's own Postgres, so restaurants see
// real data — especially guest bookings — in the new admin panel.
//
// The old system is the source of truth during the transition. The sync is
// strictly ONE-WAY, old -> new: this package never writes to the old database.
// It reads changed rows since a per-entity cursor and UPSERTs them by id into
// the new tables, so a re-run never duplicates or corrupts. Ids are UUIDs and
// are reused verbatim, which is what makes the upsert idempotent.
//
// Layering: this package holds the orchestration and the old->new mapping
// rules. The read side (Source) and the write side (Sink) are ports,
// implemented by infrastructure adapters — this package imports neither pgx nor
// internal/infrastructure/*, exactly like the other usecase packages.
package legacysync

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Entity names are the keys used in the legacy_sync_cursor table. They are a
// closed set: the value reaches SQL as a primary key, so it must never be
// caller-shaped data.
const (
	EntityRestaurants    = "restaurants"
	EntityTables         = "restaurant_tables"
	EntityMenuCategories = "menu_categories"
	EntityMenuItems      = "menu_items"
	EntityBookings       = "bookings"
	EntityBookingTables  = "booking_tables"
)

// Cursor is a per-entity high-water mark: the (updated_at, id) of the last row
// that was durably written. Rows are fetched and ordered by this pair so that
// two source rows sharing the same timestamp are still walked deterministically
// and exactly once. The zero Cursor (epoch, uuid.Nil) matches every source row.
type Cursor struct {
	UpdatedAt time.Time
	ID        uuid.UUID
}

// Outcome is what one row upsert did.
type Outcome int

const (
	// Written: the row was inserted or updated in the new DB.
	Written Outcome = iota
	// Parked: a parent the row references is not in the new DB yet (a foreign
	// key would be violated). The row is left untouched and retried on a later
	// tick, once the parent syncs. The cursor does NOT advance past a parked
	// row, so it is never lost.
	Parked
	// Skipped: the row can never land no matter how many times it is retried —
	// e.g. it would violate an exclusion or check constraint (two overlapping
	// active holds on one table, a non-positive guest count). It is logged and
	// stepped over; the cursor DOES advance past it so it never blocks the
	// entity behind it forever.
	Skipped
)

// Source is the READ-ONLY view of the old system. Every method returns rows
// strictly after cur, ordered by (updated_at, id) ascending, capped at limit.
// Implementations must issue SELECT statements only — never a write or DDL
// against the old database.
type Source interface {
	Restaurants(ctx context.Context, cur Cursor, limit int) ([]Restaurant, error)
	Tables(ctx context.Context, cur Cursor, limit int) ([]Table, error)
	MenuCategories(ctx context.Context, cur Cursor, limit int) ([]MenuCategory, error)
	MenuItems(ctx context.Context, cur Cursor, limit int) ([]MenuItem, error)
	Bookings(ctx context.Context, cur Cursor, limit int) ([]LegacyBooking, error)
	BookingTables(ctx context.Context, cur Cursor, limit int) ([]LegacyBookingTable, error)
}

// Sink is the write side, over the NEW database. Every Upsert is an
// ON CONFLICT (id) DO UPDATE and returns an Outcome (see above). Get/SetCursor
// persist the per-entity high-water mark in legacy_sync_cursor.
type Sink interface {
	GetCursor(ctx context.Context, entity string) (Cursor, error)
	SetCursor(ctx context.Context, entity string, c Cursor) error

	// RestaurantDurations returns per-restaurant booking_duration_minutes for the
	// restaurants that have a set, positive override. Used to resolve a booking's
	// ends_at / slot end from the venue's own duration (restaurants sync before
	// bookings, so the value is already present), falling back to the env default
	// for any restaurant not in the map.
	RestaurantDurations(ctx context.Context) (map[uuid.UUID]int, error)

	UpsertRestaurant(ctx context.Context, r Restaurant) (Outcome, error)
	UpsertTable(ctx context.Context, t Table) (Outcome, error)
	UpsertMenuCategory(ctx context.Context, c MenuCategory) (Outcome, error)
	UpsertMenuItem(ctx context.Context, m MenuItem) (Outcome, error)
	UpsertBooking(ctx context.Context, b Booking) (Outcome, error)
	UpsertBookingTable(ctx context.Context, bt BookingTable) (Outcome, error)
}

// The trivial entities (restaurants, tables, menu) are near 1:1 old->new, so
// the Source fills the new-shaped struct directly. Bookings and booking_tables
// need real mapping (status enum, phone normalization, a derived end time, a
// derived slot), so the Source returns their raw Legacy* shape and the worker
// maps them (see mapping.go).

// Restaurant is a new-shaped restaurants row. category_id is intentionally
// absent: the old category_id points at old restaurant_categories, which is not
// synced in increment 1, so it is written NULL by the Sink.
type Restaurant struct {
	ID               uuid.UUID
	Name             string
	NameI18n         []byte // jsonb, nil when absent
	Description      string
	DescriptionI18n  []byte
	CuisineType      string
	CuisineTypeI18n  []byte
	Address          string
	AddressI18n      []byte
	OpeningHours     string
	OpeningHoursI18n []byte
	City             string
	PriceCategory    string
	Email            string
	Phone            string
	Latitude         *float64
	Longitude        *float64
	KwaakaID         *string
	IsActive         bool
	IsNew            *bool
	IsPopular        *bool
	IsPremium        *bool
	HiddenFromHome   bool
	DisplayOrder     *int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (r Restaurant) Cursor() Cursor { return Cursor{UpdatedAt: r.UpdatedAt, ID: r.ID} }

// Table is a new-shaped restaurant_tables row.
type Table struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Name         string
	Capacity     int
	Description  *string
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (t Table) Cursor() Cursor { return Cursor{UpdatedAt: t.UpdatedAt, ID: t.ID} }

// MenuCategory is a new-shaped menu_categories row. The old table has no
// updated_at, so the Source reports created_at as UpdatedAt for cursoring.
type MenuCategory struct {
	ID           uuid.UUID
	Name         string
	NameI18n     []byte
	ParentID     *uuid.UUID
	DisplayOrder int
	CreatedAt    time.Time
	UpdatedAt    time.Time // = created_at (old table has no updated_at)
}

func (c MenuCategory) Cursor() Cursor { return Cursor{UpdatedAt: c.UpdatedAt, ID: c.ID} }

// MenuItem is a new-shaped menu_items row. The old price is an integer number of
// tenge; the new column is numeric(12,2) tenge — the Sink casts, the value is
// unchanged.
type MenuItem struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	Name            string
	NameI18n        []byte
	Description     string
	DescriptionI18n []byte
	Price           int64
	ImageURL        *string
	IsAvailable     bool
	Category        *string
	CategoryI18n    []byte
	Subcategory     *string
	SubcategoryI18n []byte
	PortionSize     *string
	PortionSizeI18n []byte
	Language        *string
	DisplayOrder    *int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (m MenuItem) Cursor() Cursor { return Cursor{UpdatedAt: m.UpdatedAt, ID: m.ID} }

// LegacyBooking is the RAW old bookings row, before mapping. See mapping.go for
// how it becomes a Booking (status enum, phone normalization, derived ends_at,
// derived source, guarded user_id).
type LegacyBooking struct {
	ID                     uuid.UUID
	UserID                 *uuid.UUID
	RestaurantID           uuid.UUID
	Name                   string
	Phone                  string
	Email                  string
	Guests                 int
	BookingDate            time.Time // -> starts_at
	Status                 string    // old booking_status enum label
	Notes                  *string
	OriginalBookingTime    *string
	CreatedByAdmin         bool
	LateNotificationSent   bool
	UserNotifiedLateAt     *time.Time
	UserLateMessage        *string
	PromotionID            *uuid.UUID
	EventID                *uuid.UUID
	Reminder60SentAt       *time.Time
	Reminder30SentAt       *time.Time
	CancellationReasonCode *string
	CancellationReason     *string
	CancelledAt            *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (b LegacyBooking) Cursor() Cursor { return Cursor{UpdatedAt: b.UpdatedAt, ID: b.ID} }

// Booking is a new-shaped bookings row, after mapping.
type Booking struct {
	ID                     uuid.UUID
	RestaurantID           uuid.UUID
	UserID                 *uuid.UUID // written only if it exists in new users (FK-guarded)
	Name                   string
	Phone                  string
	Email                  string
	PhoneNormalized        string
	Guests                 int
	StartsAt               time.Time
	EndsAt                 time.Time
	Status                 string
	Source                 string
	Notes                  *string
	PromotionID            *uuid.UUID
	EventID                *uuid.UUID
	CreatedByAdmin         bool
	CancelledAt            *time.Time
	CancellationReasonCode *string
	CancellationReason     *string
	LateNotificationSent   bool
	UserNotifiedLateAt     *time.Time
	UserLateMessage        *string
	Reminder60SentAt       *time.Time
	Reminder30SentAt       *time.Time
	OriginalBookingTime    *string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// LegacyBookingTable is the RAW table-assignment row, before mapping. It unions
// two old sources: the (barely used) booking_tables join table and the
// per-booking bookings.table_id single-table pointer. For a synthesized row
// (from bookings.table_id) ID is uuid.Nil and the worker derives a deterministic
// id from (booking_id, table_id).
//
// SortID is the keyset pagination key: bt.id for a join row, the booking id for
// a synthesized row. It is unique per row across BOTH union arms and — unlike the
// synthesized new-DB id — is available in SQL before mapping, so the Source can
// paginate on (updated_at, sort_id) with a real, gap-free tie-break. Without it
// two rows sharing an updated_at could straddle a batch boundary and be lost.
type LegacyBookingTable struct {
	ID           uuid.UUID // uuid.Nil => synthesized from bookings.table_id
	SortID       uuid.UUID // keyset tie-break (bt.id or booking id); never Nil
	BookingID    uuid.UUID
	TableID      uuid.UUID
	RestaurantID uuid.UUID // booking's restaurant, for duration resolution
	BookingDate  time.Time // -> slot start
	Status       string    // old booking status, decides `active`
	CreatedAt    time.Time
	UpdatedAt    time.Time // = booking.updated_at
}

// Cursor keys booking_tables on (updated_at, sort_id) — the same pair the Source
// paginates on — so the worker advances exactly over what the next fetch returns.
func (bt LegacyBookingTable) Cursor() Cursor {
	return Cursor{UpdatedAt: bt.UpdatedAt, ID: bt.SortID}
}

// BookingTable is a new-shaped booking_tables row, after mapping.
type BookingTable struct {
	ID        uuid.UUID
	BookingID uuid.UUID
	TableID   uuid.UUID
	SlotStart time.Time
	SlotEnd   time.Time
	Active    bool
	CreatedAt time.Time
}
