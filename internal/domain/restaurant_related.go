package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Feature struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Name         string
	NameI18n     I18n
	CreatedAt    time.Time
}

type Image struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	ImageURL     string
	IsPrimary    bool
	CreatedAt    time.Time
}

type Tag struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	TagName      string
	TagNameI18n  I18n
	CreatedAt    time.Time
}

type SocialLink struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Type         string
	URL          string
	CreatedAt    time.Time
}

type WorkingHours struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	DayOfWeek    int
	OpenTime     *string
	CloseTime    *string
	IsOpen       bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type TimeSlot struct {
	ID                 uuid.UUID
	RestaurantID       uuid.UUID
	DayOfWeek          int
	StartTime          string
	EndTime            string
	IsManuallyDisabled bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type RestaurantTable struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Name         string
	Capacity     int
	Description  *string
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// FloorPlan carries the opaque editor layout as raw JSON (never interpreted
// server-side in Wave 1).
type FloorPlan struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	LayoutData   json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RestaurantManager is one row of a restaurant's staff roster: a user with a
// StaffRole (owner/manager/hostess) at this specific restaurant (migration
// 0012 added Role; older rows were backfilled to StaffRoleManager, see the
// migration's comment for why that default is safe).
type RestaurantManager struct {
	ID            uuid.UUID
	RestaurantID  uuid.UUID
	UserID        uuid.UUID
	Role          StaffRole
	CreatedBy     *uuid.UUID
	WhatsappOptIn bool
	WhatsappPhone *string
	CreatedAt     time.Time
}

// StaffMembership is one restaurant the caller is a staff member of, carrying
// the venue's display name (localizable) and the caller's StaffRole there. It
// is the read model behind GET /admin/my-restaurants — the post-login "which
// restaurants do I manage" picker, so the admin panel never asks staff to type
// a restaurant UUID. It is produced by a single join of restaurant_managers to
// restaurants scoped to the caller's own user id; a caller only ever sees rows
// they have a membership in.
type StaffMembership struct {
	RestaurantID uuid.UUID
	Name         string
	NameI18n     I18n
	Role         StaffRole
}

type RestaurantCategory struct {
	ID              uuid.UUID
	Name            string
	NameI18n        I18n
	Description     *string
	DescriptionI18n I18n
	CreatedAt       time.Time
}

// PartnershipRequest is a public lead-form submission (no FK).
type PartnershipRequest struct {
	ID             uuid.UUID
	RestaurantName string
	ContactName    string
	Email          string
	Phone          string
	Address        string
	CuisineType    *string
	Description    *string
	AdditionalInfo *string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RestaurantRelatedRepository reads and replaces a restaurant's inline
// collections. Replace* delete existing rows for the restaurant and insert the
// given set (call inside a TxManager for the parent mutation).
type RestaurantRelatedRepository interface {
	ListImages(ctx context.Context, restaurantID uuid.UUID) ([]Image, error)
	ListFeatures(ctx context.Context, restaurantID uuid.UUID) ([]Feature, error)
	ListTags(ctx context.Context, restaurantID uuid.UUID) ([]Tag, error)
	ListSocialLinks(ctx context.Context, restaurantID uuid.UUID) ([]SocialLink, error)
	ListWorkingHours(ctx context.Context, restaurantID uuid.UUID) ([]WorkingHours, error)
	ListTimeSlots(ctx context.Context, restaurantID uuid.UUID) ([]TimeSlot, error)
	ListTables(ctx context.Context, restaurantID uuid.UUID) ([]RestaurantTable, error)
	GetFloorPlan(ctx context.Context, restaurantID uuid.UUID) (*FloorPlan, error)

	ReplaceImages(ctx context.Context, restaurantID uuid.UUID, items []Image) error
	ReplaceFeatures(ctx context.Context, restaurantID uuid.UUID, items []Feature) error
	ReplaceTags(ctx context.Context, restaurantID uuid.UUID, items []Tag) error
	ReplaceSocialLinks(ctx context.Context, restaurantID uuid.UUID, items []SocialLink) error
	ReplaceWorkingHours(ctx context.Context, restaurantID uuid.UUID, items []WorkingHours) error
	ReplaceTimeSlots(ctx context.Context, restaurantID uuid.UUID, items []TimeSlot) error
	ReplaceTables(ctx context.Context, restaurantID uuid.UUID, items []RestaurantTable) error
	UpsertFloorPlan(ctx context.Context, fp *FloorPlan) error
}

type RestaurantCategoryRepository interface {
	List(ctx context.Context) ([]RestaurantCategory, error)
	Create(ctx context.Context, c *RestaurantCategory) error
}

type RestaurantManagerRepository interface {
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]RestaurantManager, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]RestaurantManager, error)
	// GetByID returns ErrNotFound when absent. It exists so a caller acting
	// on a manager row by id alone (SetRole, Remove) can first resolve WHICH
	// restaurant that row belongs to and authorize against THAT restaurant —
	// never trust a path parameter for the restaurant id when the mutation
	// target is identified by its own id (avoids the cross-tenant IDOR the
	// old admin-only gate on these routes used to rely on entirely).
	GetByID(ctx context.Context, id uuid.UUID) (*RestaurantManager, error)
	Create(ctx context.Context, m *RestaurantManager) error
	// UpdateRole changes an existing staff member's role in place. Returns
	// ErrNotFound when id is absent.
	UpdateRole(ctx context.Context, id uuid.UUID, role StaffRole) error
	Delete(ctx context.Context, id uuid.UUID) error
}

type PartnershipRequestRepository interface {
	Create(ctx context.Context, p *PartnershipRequest) error
}
