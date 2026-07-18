# Restaurants Domain (Wave 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-implement the BookEat restaurant catalog (restaurants + categories, media, working hours, time slots, tables, managers, floor plans, partnership requests) as a clean Go domain in `backend-core`, migrating the data from a Supabase dump with UUIDs preserved.

**Architecture:** Clean/Hexagonal per `backend-core/CLAUDE.md`. `transport(gin) → usecase → domain ← infrastructure(postgres)`. Public read endpoints are unauthenticated; mutations are gated by a new `middleware.RequireRole`. Data is copied by an idempotent `cmd/etl` subcommand reading from the `raw_supabase` staging schema.

**Tech Stack:** Go, gin, pgx v5 (native, via `sqltx.Querier`), goose migrations, `github.com/google/uuid`, hand-written test fakes, integration tests behind `TEST_DATABASE_URL`.

**Spec:** `docs/superpowers/specs/2026-07-13-restaurants-domain-design.md`
**Roadmap:** `docs/superpowers/specs/2026-07-13-backend-migration-roadmap.md`

## Global Constraints

- **No private deps.** Public modules + stdlib only.
- **No DB enums.** Enumerated fields are `VARCHAR`, validated in Go. `city ∈ {"Астана","Алматы"}`, `price_category ∈ {"₸","₸₸","₸₸₸"}` (values stored verbatim as they exist in Supabase).
- **Preserve UUIDs.** All `id` primary keys are copied verbatim from Supabase.
- **i18n fields are `jsonb`** of shape `{"ru":...,"kk":...,"en":...}`, modeled in Go as `map[string]string` (nil when absent).
- **Errors:** return `domain` sentinels; wrap with `fmt.Errorf("...: %w", err)`. Map SQLSTATE `23505` → `domain.ErrAlreadyExists`.
- **Repositories** take `sqltx.Querier` and read the active querier via `sqltx.From(ctx, r.pool)`.
- **Responses** go through `response.Envelope` / `response.OK` / `response.Created` / `response.HandleError`; always `return` immediately after writing an error.
- **API prefix is `/api/v1`** (see `bootstrap/app.go`).
- **Formatting:** `gofmt -w .` and `go vet ./...` must be clean before each commit.
- **Tests:** unit tests pass under `go test -short ./...`; integration tests skip without `TEST_DATABASE_URL`.

---

## File Structure

```
migrations/0002_restaurants.sql                         (Task 1)
internal/domain/restaurant.go                           (Task 2)
internal/domain/restaurant_related.go                   (Task 2)
internal/transport/rest/middleware/role.go              (Task 3)
internal/transport/rest/middleware/role_test.go         (Task 3)
internal/infrastructure/postgres/restaurant/repository.go       (Task 4)
internal/infrastructure/postgres/restaurant/repository_test.go  (Task 4)
internal/infrastructure/postgres/restaurant/related.go          (Task 5)
internal/infrastructure/postgres/restaurant/related_test.go     (Task 5)
internal/usecase/restaurants/facade.go                  (Task 6)
internal/usecase/restaurants/managers.go                (Task 6)
internal/usecase/restaurants/ports.go                   (Task 6)
internal/usecase/restaurants/facade_test.go             (Task 6)
internal/usecase/restaurants/fakes_test.go              (Task 6)
internal/transport/rest/restaurants/handler.go          (Task 7)
internal/transport/rest/restaurants/request.go          (Task 7)
internal/transport/rest/restaurants/response.go         (Task 7)
internal/bootstrap/deps.go        (modify — Task 7)
internal/bootstrap/app.go         (modify — Task 7)
cmd/etl/main.go                   (modify — Task 8)
cmd/etl/restaurants.go            (Task 8)
```

**Milestones:** Tasks 1–2, 4–8 deliver the public catalog and its data. Task 3 + the manager/admin routes in Tasks 6–7 deliver back-office management. The plan is ordered so `go build ./...` and `go test -short ./...` stay green after every task.

---

### Task 1: Schema migration

**Files:**
- Create: `migrations/0002_restaurants.sql`

**Interfaces:**
- Produces: tables `restaurant_categories`, `restaurants`, `restaurant_features`, `restaurant_images`, `restaurant_tags`, `restaurant_social_links`, `restaurant_working_hours`, `restaurant_time_slots`, `restaurant_tables`, `restaurant_floor_plans`, `restaurant_managers`, `restaurant_partnership_requests`.

- [ ] **Step 1: Write the migration**

Create `migrations/0002_restaurants.sql`:

```sql
-- +goose Up
CREATE TABLE restaurant_categories
(
    id               uuid PRIMARY KEY,
    name             varchar     NOT NULL,
    name_i18n        jsonb,
    description      varchar,
    description_i18n jsonb,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE restaurants
(
    id                   uuid PRIMARY KEY,
    category_id          uuid REFERENCES restaurant_categories (id) ON DELETE SET NULL,
    name                 varchar          NOT NULL,
    name_i18n            jsonb,
    description          varchar          NOT NULL DEFAULT '',
    description_i18n     jsonb,
    cuisine_type         varchar          NOT NULL DEFAULT '',
    cuisine_type_i18n    jsonb,
    address              varchar          NOT NULL DEFAULT '',
    address_i18n         jsonb,
    opening_hours        varchar          NOT NULL DEFAULT '',
    opening_hours_i18n   jsonb,
    city                 varchar          NOT NULL,
    price_category       varchar          NOT NULL,
    email                varchar          NOT NULL DEFAULT '',
    phone                varchar          NOT NULL DEFAULT '',
    latitude             double precision,
    longitude            double precision,
    kwaaka_restaurant_id varchar,
    is_active            boolean          NOT NULL DEFAULT true,
    is_new               boolean,
    is_popular           boolean,
    is_premium           boolean,
    hidden_from_home     boolean          NOT NULL DEFAULT false,
    display_order        integer,
    created_at           timestamptz      NOT NULL DEFAULT now(),
    updated_at           timestamptz      NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurants_listing ON restaurants (is_active, display_order, name);
CREATE INDEX idx_restaurants_category ON restaurants (category_id);

CREATE TABLE restaurant_features
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name          varchar     NOT NULL,
    name_i18n     jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_features_rid ON restaurant_features (restaurant_id);

CREATE TABLE restaurant_images
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    image_url     varchar     NOT NULL,
    is_primary    boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_images_rid_primary ON restaurant_images (restaurant_id, is_primary);

CREATE TABLE restaurant_tags
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    tag_name      varchar     NOT NULL,
    tag_name_i18n jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_tags_rid ON restaurant_tags (restaurant_id);

CREATE TABLE restaurant_social_links
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    type          varchar     NOT NULL,
    url           varchar     NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_social_links_rid ON restaurant_social_links (restaurant_id);

CREATE TABLE restaurant_working_hours
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    day_of_week   integer     NOT NULL,
    open_time     varchar,
    close_time    varchar,
    is_open       boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_working_hours_rid ON restaurant_working_hours (restaurant_id);

CREATE TABLE restaurant_time_slots
(
    id                   uuid PRIMARY KEY,
    restaurant_id        uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    day_of_week          integer     NOT NULL,
    start_time           varchar     NOT NULL,
    end_time             varchar     NOT NULL,
    is_manually_disabled boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_time_slots_rid ON restaurant_time_slots (restaurant_id);

CREATE TABLE restaurant_tables
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name          varchar     NOT NULL,
    capacity      integer     NOT NULL DEFAULT 0,
    description   varchar,
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_tables_rid ON restaurant_tables (restaurant_id);

CREATE TABLE restaurant_floor_plans
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    layout_data   jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_floor_plans_rid ON restaurant_floor_plans (restaurant_id);

CREATE TABLE restaurant_managers
(
    id             uuid PRIMARY KEY,
    restaurant_id  uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_by     uuid,
    whatsapp_opt_in boolean    NOT NULL DEFAULT false,
    whatsapp_phone varchar,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (restaurant_id, user_id)
);
CREATE INDEX idx_restaurant_managers_user ON restaurant_managers (user_id);

CREATE TABLE restaurant_partnership_requests
(
    id              uuid PRIMARY KEY,
    restaurant_name varchar     NOT NULL,
    contact_name    varchar     NOT NULL,
    email           varchar     NOT NULL,
    phone           varchar     NOT NULL,
    address         varchar     NOT NULL DEFAULT '',
    cuisine_type    varchar,
    description     varchar,
    additional_info varchar,
    status          varchar     NOT NULL DEFAULT 'pending',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE restaurant_partnership_requests;
DROP TABLE restaurant_managers;
DROP TABLE restaurant_floor_plans;
DROP TABLE restaurant_tables;
DROP TABLE restaurant_time_slots;
DROP TABLE restaurant_working_hours;
DROP TABLE restaurant_social_links;
DROP TABLE restaurant_tags;
DROP TABLE restaurant_images;
DROP TABLE restaurant_features;
DROP TABLE restaurants;
DROP TABLE restaurant_categories;
```

- [ ] **Step 2: Apply and roll back to verify**

Run: `make migrate-up && go run ./cmd/migrate/migrate.go status && make migrate-down && make migrate-up`
Expected: `0002_restaurants` applied, then rolled back cleanly, then re-applied — no SQL errors.

- [ ] **Step 3: Commit**

```bash
git add migrations/0002_restaurants.sql
git commit -m "feat(db): add restaurants domain schema (wave 1)"
```

---

### Task 2: Domain entities

**Files:**
- Create: `internal/domain/restaurant.go`
- Create: `internal/domain/restaurant_related.go`

**Interfaces:**
- Produces: `domain.Restaurant`, `domain.RestaurantAggregate`, `domain.City`, `domain.PriceCategory`, `domain.RestaurantFilter`, `domain.RestaurantRepository`; related structs `Feature`, `Image`, `Tag`, `SocialLink`, `WorkingHours`, `TimeSlot`, `RestaurantTable`, `FloorPlan`, `RestaurantManager`, `RestaurantCategory`, `PartnershipRequest` and their repository interfaces `RestaurantRelatedRepository`, `RestaurantCategoryRepository`, `RestaurantManagerRepository`, `PartnershipRequestRepository`.

- [ ] **Step 1: Write the validation test**

Create `internal/domain/restaurant_test.go`:

```go
package domain

import "testing"

func TestCityValid(t *testing.T) {
	cases := map[City]bool{"Астана": true, "Алматы": true, "almaty": false, "": false}
	for c, want := range cases {
		if got := c.Valid(); got != want {
			t.Errorf("City(%q).Valid() = %v, want %v", c, got, want)
		}
	}
}

func TestPriceCategoryValid(t *testing.T) {
	cases := map[PriceCategory]bool{"₸": true, "₸₸": true, "₸₸₸": true, "$": false, "": false}
	for p, want := range cases {
		if got := p.Valid(); got != want {
			t.Errorf("PriceCategory(%q).Valid() = %v, want %v", p, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/ -run 'TestCityValid|TestPriceCategoryValid' -v`
Expected: FAIL — `City`/`PriceCategory` undefined.

- [ ] **Step 3: Write `restaurant.go`**

```go
package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// City is a restaurant's city, stored as VARCHAR. Values are the raw Supabase
// enum labels (Cyrillic).
type City string

const (
	CityAstana City = "Астана"
	CityAlmaty City = "Алматы"
)

// Valid reports whether c is a known city.
func (c City) Valid() bool { return c == CityAstana || c == CityAlmaty }

// PriceCategory is a restaurant's price tier, stored as VARCHAR.
type PriceCategory string

const (
	PriceLow    PriceCategory = "₸"
	PriceMid    PriceCategory = "₸₸"
	PriceHigh   PriceCategory = "₸₸₸"
)

// Valid reports whether p is a known price category.
func (p PriceCategory) Valid() bool {
	return p == PriceLow || p == PriceMid || p == PriceHigh
}

// I18n is a localized field of shape {"ru":...,"kk":...,"en":...}. Nil when the
// column is NULL.
type I18n map[string]string

// Restaurant is a venue in the catalog. ID equals the original Supabase id.
type Restaurant struct {
	ID                 uuid.UUID
	CategoryID         *uuid.UUID
	Name               string
	NameI18n           I18n
	Description        string
	DescriptionI18n    I18n
	CuisineType        string
	CuisineTypeI18n    I18n
	Address            string
	AddressI18n        I18n
	OpeningHours       string
	OpeningHoursI18n   I18n
	City               City
	PriceCategory      PriceCategory
	Email              string
	Phone              string
	Latitude           *float64
	Longitude          *float64
	KwaakaRestaurantID *string
	IsActive           bool
	IsNew              *bool
	IsPopular          *bool
	IsPremium          *bool
	HiddenFromHome     bool
	DisplayOrder       *int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// RestaurantAggregate is a restaurant with its inline collections, matching the
// nested read the app performs on the detail screen.
type RestaurantAggregate struct {
	Restaurant
	Images      []Image
	Features    []Feature
	Tags        []Tag
	SocialLinks []SocialLink
}

// RestaurantFilter narrows a listing query. Zero-value fields are ignored.
type RestaurantFilter struct {
	City      *City
	Category  *uuid.UUID
	IsPopular *bool
	IsNew     *bool
	Search    string // case-insensitive substring match on name
	Page      int    // 1-based; <=0 means 1
	PerPage   int    // <=0 means default (20), capped at 100
}

// RestaurantRepository persists restaurants. Get* return ErrNotFound when absent.
type RestaurantRepository interface {
	Create(ctx context.Context, r *Restaurant) error
	Update(ctx context.Context, r *Restaurant) error
	GetByID(ctx context.Context, id uuid.UUID) (*RestaurantAggregate, error)
	// ListActive returns active restaurants matching f plus the total count.
	// Ordering: display_order (NULLs last), then name. PrimaryImage is populated.
	ListActive(ctx context.Context, f RestaurantFilter) ([]RestaurantListItem, int, error)
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
}

// RestaurantListItem is a lightweight row for the catalog listing.
type RestaurantListItem struct {
	Restaurant
	PrimaryImage *string
}
```

- [ ] **Step 4: Write `restaurant_related.go`**

```go
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

type RestaurantManager struct {
	ID             uuid.UUID
	RestaurantID   uuid.UUID
	UserID         uuid.UUID
	CreatedBy      *uuid.UUID
	WhatsappOptIn  bool
	WhatsappPhone  *string
	CreatedAt      time.Time
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
	Create(ctx context.Context, m *RestaurantManager) error
	Delete(ctx context.Context, id uuid.UUID) error
}

type PartnershipRequestRepository interface {
	Create(ctx context.Context, p *PartnershipRequest) error
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/domain/ -v && go build ./...`
Expected: PASS; build succeeds.

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go vet ./...
git add internal/domain/restaurant.go internal/domain/restaurant_related.go internal/domain/restaurant_test.go
git commit -m "feat(domain): add restaurant entities and repository ports"
```

---

### Task 3: `middleware.RequireRole`

`bootstrap/app.go` references role gating but no `RequireRole` exists yet. Build it now so the manager/admin routes in Task 7 can use it.

**Files:**
- Create: `internal/transport/rest/middleware/role.go`
- Create: `internal/transport/rest/middleware/role_test.go`

**Interfaces:**
- Consumes: `middleware.GetAuthUser(ctx) (AuthUser, bool)`, `AuthUser{ID uuid.UUID, Role string}` (existing, `auth.go`).
- Produces: `func RequireRole(roles ...domain.Role) gin.HandlerFunc`.

- [ ] **Step 1: Write the failing test**

Create `internal/transport/rest/middleware/role_test.go`:

```go
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestRequireRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		set        bool
		role       domain.Role
		allow      []domain.Role
		wantStatus int
	}{
		{"allowed", true, domain.RoleAdmin, []domain.Role{domain.RoleAdmin}, http.StatusOK},
		{"forbidden", true, domain.RoleUser, []domain.Role{domain.RoleAdmin}, http.StatusForbidden},
		{"one-of", true, domain.RoleRestaurant, []domain.Role{domain.RoleAdmin, domain.RoleRestaurant}, http.StatusOK},
		{"no-auth-user", false, "", []domain.Role{domain.RoleAdmin}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.set {
				ctx := context.WithValue(req.Context(), authUserKey{}, AuthUser{ID: uuid.New(), Role: string(tc.role)})
				req = req.WithContext(ctx)
			}
			c.Request = req
			handler := RequireRole(tc.allow...)
			handler(c)
			if !c.IsAborted() {
				c.Status(http.StatusOK)
			}
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transport/rest/middleware/ -run TestRequireRole -v`
Expected: FAIL — `RequireRole` undefined.

- [ ] **Step 3: Write `role.go`**

```go
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
)

// RequireRole aborts the request unless the authenticated user's role is one of
// roles. Must run after Auth, which stores the AuthUser on the context.
func RequireRole(roles ...domain.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		au, ok := GetAuthUser(c.Request.Context())
		if !ok {
			response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
			c.Abort()
			return
		}
		for _, r := range roles {
			if au.Role == string(r) {
				c.Next()
				return
			}
		}
		response.Error(c.Writer, http.StatusForbidden, "forbidden")
		c.Abort()
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transport/rest/middleware/ -run TestRequireRole -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add internal/transport/rest/middleware/role.go internal/transport/rest/middleware/role_test.go
git commit -m "feat(middleware): add RequireRole role gating"
```

---

### Task 4: Restaurant Postgres repository (aggregate + listing + CRUD)

**Files:**
- Create: `internal/infrastructure/postgres/restaurant/repository.go`
- Create: `internal/infrastructure/postgres/restaurant/repository_test.go`

**Interfaces:**
- Consumes: `sqltx.Querier`, `sqltx.From`, `domain.Restaurant`, `domain.RestaurantAggregate`, `domain.RestaurantFilter`, `domain.RestaurantListItem`.
- Produces: `restaurant.New(pool sqltx.Querier) *restaurant.Repository` implementing `domain.RestaurantRepository`; helpers `i18nToDB`, `i18nFromDB` reused by Task 5.

- [ ] **Step 1: Write `repository.go`**

```go
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
		is_premium=$23, hidden_from_home=$24, display_order=$25, updated_at=$27
		WHERE id=$1`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(m)...)
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
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+` FROM restaurants WHERE id=$1`, id)
	base, err := scanRestaurant(row)
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
		add("r.name ILIKE '%%' || $%d || '%%'", s)
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

func mapWrite(err error, ctx string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: restaurant", domain.ErrAlreadyExists)
	}
	return fmt.Errorf("%s: %w", ctx, err)
}
```

> Note: `Related` (with its `List*` methods and the shared `pool` field) is defined in Task 5's `related.go`, same package. `GetByID` above uses it; the package compiles only once Task 5 lands, so implement Task 5 before running Task 4's tests. Alternatively land Tasks 4 and 5 together — they share the package.

- [ ] **Step 2: Write the integration test**

Create `internal/infrastructure/postgres/restaurant/repository_test.go`:

```go
package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestRestaurantCRUDAndList(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	order := 1
	popular := true
	m := &domain.Restaurant{
		ID: uuid.New(), Name: "Test Bistro", NameI18n: domain.I18n{"ru": "Бистро"},
		City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
		IsActive: true, IsPopular: &popular, DisplayOrder: &order,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Test Bistro" || got.NameI18n["ru"] != "Бистро" || got.City != domain.CityAlmaty {
		t.Errorf("roundtrip mismatch: %+v", got.Restaurant)
	}

	items, total, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), IsPopular: &popular})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != m.ID {
		t.Errorf("list = %d items (total %d), want 1", len(items), total)
	}

	if err := repo.SetActive(ctx, m.ID, false); err != nil {
		t.Fatalf("set active: %v", err)
	}
	_, total, _ = repo.ListActive(ctx, domain.RestaurantFilter{})
	if total != 0 {
		t.Errorf("after deactivate total = %d, want 0", total)
	}

	if _, err := repo.GetByID(ctx, uuid.New()); err != domain.ErrNotFound {
		t.Errorf("missing get err = %v, want ErrNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }
```

- [ ] **Step 3: Run tests**

Run: `go build ./... && TEST_DATABASE_URL=postgres://... go test ./internal/infrastructure/postgres/restaurant/ -run TestRestaurantCRUDAndList -v`
Expected: build succeeds (after Task 5 lands); integration test PASS. Under `go test -short ./...` it SKIPS.

- [ ] **Step 4: Commit**

```bash
gofmt -w . && go vet ./...
git add internal/infrastructure/postgres/restaurant/repository.go internal/infrastructure/postgres/restaurant/repository_test.go
git commit -m "feat(postgres): add restaurant repository (aggregate, listing, crud)"
```

---

### Task 5: Related-collections repository

Implements `RestaurantRelatedRepository`, `RestaurantCategoryRepository`, `RestaurantManagerRepository`, `PartnershipRequestRepository` in the same `restaurant` package (they share the i18n helpers).

**Files:**
- Create: `internal/infrastructure/postgres/restaurant/related.go`
- Create: `internal/infrastructure/postgres/restaurant/related_test.go`

**Interfaces:**
- Consumes: helpers `i18nToDB`, `i18nFromDB` (Task 4), `sqltx.Querier`, `sqltx.From`, domain related structs (Task 2).
- Produces: `restaurant.NewRelated(pool)`, `restaurant.NewCategories(pool)`, `restaurant.NewManagers(pool)`, `restaurant.NewPartnership(pool)`; type `Related` used by Task 4's `GetByID`.

- [ ] **Step 1: Write `related.go`**

```go
package restaurant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
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

func (r *Related) ListFeatures(ctx context.Context, rid uuid.UUID) ([]domain.Feature, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, name, name_i18n, created_at
		 FROM restaurant_features WHERE restaurant_id=$1 ORDER BY created_at`, rid)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	defer rows.Close()
	var out []domain.Feature
	for rows.Next() {
		var f domain.Feature
		var i18n []byte
		if err := rows.Scan(&f.ID, &f.RestaurantID, &f.Name, &i18n, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.NameI18n = i18nFromDB(i18n)
		out = append(out, f)
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
	for _, i := range items {
		if i.ID == uuid.Nil {
			i.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_images (id, restaurant_id, image_url, is_primary, created_at)
			 VALUES ($1,$2,$3,$4,now())`, i.ID, rid, i.ImageURL, i.IsPrimary); err != nil {
			return fmt.Errorf("replace images: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceFeatures(ctx context.Context, rid uuid.UUID, items []domain.Feature) error {
	if err := r.del(ctx, "restaurant_features", rid); err != nil {
		return fmt.Errorf("replace features: %w", err)
	}
	for _, f := range items {
		if f.ID == uuid.Nil {
			f.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_features (id, restaurant_id, name, name_i18n, created_at)
			 VALUES ($1,$2,$3,$4,now())`, f.ID, rid, f.Name, i18nToDB(f.NameI18n)); err != nil {
			return fmt.Errorf("replace features: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceTags(ctx context.Context, rid uuid.UUID, items []domain.Tag) error {
	if err := r.del(ctx, "restaurant_tags", rid); err != nil {
		return fmt.Errorf("replace tags: %w", err)
	}
	for _, tg := range items {
		if tg.ID == uuid.Nil {
			tg.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_tags (id, restaurant_id, tag_name, tag_name_i18n, created_at)
			 VALUES ($1,$2,$3,$4,now())`, tg.ID, rid, tg.TagName, i18nToDB(tg.TagNameI18n)); err != nil {
			return fmt.Errorf("replace tags: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceSocialLinks(ctx context.Context, rid uuid.UUID, items []domain.SocialLink) error {
	if err := r.del(ctx, "restaurant_social_links", rid); err != nil {
		return fmt.Errorf("replace social links: %w", err)
	}
	for _, s := range items {
		if s.ID == uuid.Nil {
			s.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_social_links (id, restaurant_id, type, url, created_at)
			 VALUES ($1,$2,$3,$4,now())`, s.ID, rid, s.Type, s.URL); err != nil {
			return fmt.Errorf("replace social links: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceWorkingHours(ctx context.Context, rid uuid.UUID, items []domain.WorkingHours) error {
	if err := r.del(ctx, "restaurant_working_hours", rid); err != nil {
		return fmt.Errorf("replace working hours: %w", err)
	}
	for _, w := range items {
		if w.ID == uuid.Nil {
			w.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, w.ID, rid, w.DayOfWeek, w.OpenTime, w.CloseTime, w.IsOpen); err != nil {
			return fmt.Errorf("replace working hours: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceTimeSlots(ctx context.Context, rid uuid.UUID, items []domain.TimeSlot) error {
	if err := r.del(ctx, "restaurant_time_slots", rid); err != nil {
		return fmt.Errorf("replace time slots: %w", err)
	}
	for _, s := range items {
		if s.ID == uuid.Nil {
			s.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_time_slots (id, restaurant_id, day_of_week, start_time, end_time, is_manually_disabled, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, s.ID, rid, s.DayOfWeek, s.StartTime, s.EndTime, s.IsManuallyDisabled); err != nil {
			return fmt.Errorf("replace time slots: %w", err)
		}
	}
	return nil
}

func (r *Related) ReplaceTables(ctx context.Context, rid uuid.UUID, items []domain.RestaurantTable) error {
	if err := r.del(ctx, "restaurant_tables", rid); err != nil {
		return fmt.Errorf("replace tables: %w", err)
	}
	for _, tb := range items {
		if tb.ID == uuid.Nil {
			tb.ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, description, is_active, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, tb.ID, rid, tb.Name, tb.Capacity, tb.Description, tb.IsActive); err != nil {
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
		return fmt.Errorf("create category: %w", err)
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
		if err := rows.Scan(&mn.ID, &mn.RestaurantID, &mn.UserID, &mn.CreatedBy, &mn.WhatsappOptIn, &mn.WhatsappPhone, &mn.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, mn)
	}
	return out, rows.Err()
}

const mgrCols = `id, restaurant_id, user_id, created_by, whatsapp_opt_in, whatsapp_phone, created_at`

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

func (m *Managers) Create(ctx context.Context, mn *domain.RestaurantManager) error {
	if mn.ID == uuid.Nil {
		mn.ID = uuid.New()
	}
	mn.CreatedAt = time.Now()
	_, err := sqltx.From(ctx, m.pool).Exec(ctx,
		`INSERT INTO restaurant_managers (`+mgrCols+`) VALUES ($1,$2,$3,$4,$5,$6,now())`,
		mn.ID, mn.RestaurantID, mn.UserID, mn.CreatedBy, mn.WhatsappOptIn, mn.WhatsappPhone)
	if err != nil {
		return mapWrite(err, "create manager")
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
		return fmt.Errorf("create partnership request: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Write the integration test**

Create `internal/infrastructure/postgres/restaurant/related_test.go`:

```go
package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func TestRelatedReplaceAndRead(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rel := NewRelated(pool)
	txm := sqltx.NewManager(pool)

	rid := uuid.New()
	if err := repo.Create(ctx, &domain.Restaurant{
		ID: rid, Name: "X", City: domain.CityAstana, PriceCategory: domain.PriceLow, IsActive: true,
	}); err != nil {
		t.Fatalf("create restaurant: %v", err)
	}

	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := rel.ReplaceImages(ctx, rid, []domain.Image{{ImageURL: "a.png", IsPrimary: true}}); err != nil {
			return err
		}
		return rel.ReplaceFeatures(ctx, rid, []domain.Feature{{Name: "wifi", NameI18n: domain.I18n{"ru": "вайфай"}}})
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	agg, err := repo.GetByID(ctx, rid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(agg.Images) != 1 || agg.Images[0].ImageURL != "a.png" {
		t.Errorf("images = %+v", agg.Images)
	}
	if len(agg.Features) != 1 || agg.Features[0].NameI18n["ru"] != "вайфай" {
		t.Errorf("features = %+v", agg.Features)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go build ./... && TEST_DATABASE_URL=postgres://... go test ./internal/infrastructure/postgres/restaurant/ -v`
Expected: build succeeds; both integration tests PASS.

- [ ] **Step 4: Commit**

```bash
gofmt -w . && go vet ./...
git add internal/infrastructure/postgres/restaurant/related.go internal/infrastructure/postgres/restaurant/related_test.go
git commit -m "feat(postgres): add restaurant related-collections, categories, managers, partnership repos"
```

---

### Task 6: Usecase (facade, managers, ports)

**Files:**
- Create: `internal/usecase/restaurants/ports.go`
- Create: `internal/usecase/restaurants/facade.go`
- Create: `internal/usecase/restaurants/managers.go`
- Create: `internal/usecase/restaurants/facade_test.go`
- Create: `internal/usecase/restaurants/fakes_test.go`

**Interfaces:**
- Consumes: `domain.RestaurantRepository`, `domain.RestaurantRelatedRepository`, `domain.RestaurantCategoryRepository`, `domain.RestaurantManagerRepository`, `domain.PartnershipRequestRepository`, `domain.TxManager`, `domain.UserRepository`.
- Produces: `restaurants.Facade` + `restaurants.NewFacade(...)`; `restaurants.ManagerUseCase` + `restaurants.NewManagerUseCase(...)`; input structs `SaveInput`, `PartnershipInput`, `AssignManagerInput`.

- [ ] **Step 1: Write `ports.go`**

```go
// Package restaurants is the application logic for the restaurant catalog.
package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// userReader is the minimal slice of the users repository this package needs
// (verifying a manager assignee exists). Bound to the concrete user repo in deps.
type userReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}
```

- [ ] **Step 2: Write `facade.go`**

```go
package restaurants

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes catalog reads and admin mutations.
type Facade interface {
	List(ctx context.Context, f domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error)
	Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error)
	Categories(ctx context.Context) ([]domain.RestaurantCategory, error)
	Create(ctx context.Context, in SaveInput) (*domain.RestaurantAggregate, error)
	Update(ctx context.Context, id uuid.UUID, in SaveInput) (*domain.RestaurantAggregate, error)
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
	SubmitPartnership(ctx context.Context, in PartnershipInput) error
}

type facade struct {
	repo       domain.RestaurantRepository
	related    domain.RestaurantRelatedRepository
	categories domain.RestaurantCategoryRepository
	partners   domain.PartnershipRequestRepository
	tx         domain.TxManager
}

// NewFacade constructs the restaurants Facade.
func NewFacade(
	repo domain.RestaurantRepository,
	related domain.RestaurantRelatedRepository,
	categories domain.RestaurantCategoryRepository,
	partners domain.PartnershipRequestRepository,
	tx domain.TxManager,
) Facade {
	return &facade{repo: repo, related: related, categories: categories, partners: partners, tx: tx}
}

// SaveInput carries a restaurant plus its inline collections for create/update.
type SaveInput struct {
	Restaurant  domain.Restaurant
	Images      []domain.Image
	Features    []domain.Feature
	Tags        []domain.Tag
	SocialLinks []domain.SocialLink
}

// PartnershipInput is a public partnership lead submission.
type PartnershipInput struct {
	RestaurantName string
	ContactName    string
	Email          string
	Phone          string
	Address        string
	CuisineType    *string
	Description    *string
	AdditionalInfo *string
}

func (f *facade) List(ctx context.Context, flt domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	return f.repo.ListActive(ctx, flt)
}

func (f *facade) Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	return f.repo.GetByID(ctx, id)
}

func (f *facade) Categories(ctx context.Context) ([]domain.RestaurantCategory, error) {
	return f.categories.List(ctx)
}

func (f *facade) Create(ctx context.Context, in SaveInput) (*domain.RestaurantAggregate, error) {
	if err := validateRestaurant(in.Restaurant); err != nil {
		return nil, err
	}
	if in.Restaurant.ID == uuid.Nil {
		in.Restaurant.ID = uuid.New()
	}
	var out *domain.RestaurantAggregate
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.repo.Create(ctx, &in.Restaurant); err != nil {
			return err
		}
		return f.saveCollections(ctx, in)
	})
	if err != nil {
		return nil, err
	}
	out, err = f.repo.GetByID(ctx, in.Restaurant.ID)
	return out, err
}

func (f *facade) Update(ctx context.Context, id uuid.UUID, in SaveInput) (*domain.RestaurantAggregate, error) {
	if err := validateRestaurant(in.Restaurant); err != nil {
		return nil, err
	}
	in.Restaurant.ID = id
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.repo.Update(ctx, &in.Restaurant); err != nil {
			return err
		}
		return f.saveCollections(ctx, in)
	})
	if err != nil {
		return nil, err
	}
	return f.repo.GetByID(ctx, id)
}

func (f *facade) saveCollections(ctx context.Context, in SaveInput) error {
	rid := in.Restaurant.ID
	if err := f.related.ReplaceImages(ctx, rid, in.Images); err != nil {
		return err
	}
	if err := f.related.ReplaceFeatures(ctx, rid, in.Features); err != nil {
		return err
	}
	if err := f.related.ReplaceTags(ctx, rid, in.Tags); err != nil {
		return err
	}
	return f.related.ReplaceSocialLinks(ctx, rid, in.SocialLinks)
}

func (f *facade) SetActive(ctx context.Context, id uuid.UUID, active bool) error {
	return f.repo.SetActive(ctx, id, active)
}

func (f *facade) SubmitPartnership(ctx context.Context, in PartnershipInput) error {
	if strings.TrimSpace(in.RestaurantName) == "" || strings.TrimSpace(in.Email) == "" ||
		strings.TrimSpace(in.Phone) == "" || strings.TrimSpace(in.ContactName) == "" {
		return domain.ErrValidation
	}
	return f.partners.Create(ctx, &domain.PartnershipRequest{
		RestaurantName: in.RestaurantName, ContactName: in.ContactName, Email: in.Email,
		Phone: in.Phone, Address: in.Address, CuisineType: in.CuisineType,
		Description: in.Description, AdditionalInfo: in.AdditionalInfo, Status: "pending",
	})
}

// validateRestaurant enforces the enumerated-field constraints in app code.
func validateRestaurant(r domain.Restaurant) error {
	if strings.TrimSpace(r.Name) == "" || !r.City.Valid() || !r.PriceCategory.Valid() {
		return domain.ErrValidation
	}
	return nil
}
```

- [ ] **Step 3: Write `managers.go`**

```go
package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ManagerUseCase manages restaurant↔user manager assignments and answers the
// "does this user manage this restaurant?" question used to gate the back office.
type ManagerUseCase interface {
	List(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantManager, error)
	Assign(ctx context.Context, in AssignManagerInput) (*domain.RestaurantManager, error)
	Remove(ctx context.Context, id uuid.UUID) error
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
}

type managerUseCase struct {
	managers domain.RestaurantManagerRepository
	users    userReader
}

// NewManagerUseCase constructs the ManagerUseCase.
func NewManagerUseCase(managers domain.RestaurantManagerRepository, users userReader) ManagerUseCase {
	return &managerUseCase{managers: managers, users: users}
}

// AssignManagerInput assigns a user as a manager of a restaurant.
type AssignManagerInput struct {
	RestaurantID  uuid.UUID
	UserID        uuid.UUID
	CreatedBy     *uuid.UUID
	WhatsappOptIn bool
	WhatsappPhone *string
}

func (u *managerUseCase) List(ctx context.Context, rid uuid.UUID) ([]domain.RestaurantManager, error) {
	return u.managers.ListByRestaurant(ctx, rid)
}

func (u *managerUseCase) Assign(ctx context.Context, in AssignManagerInput) (*domain.RestaurantManager, error) {
	if _, err := u.users.GetByID(ctx, in.UserID); err != nil {
		return nil, err // ErrNotFound when the assignee doesn't exist
	}
	m := &domain.RestaurantManager{
		RestaurantID: in.RestaurantID, UserID: in.UserID, CreatedBy: in.CreatedBy,
		WhatsappOptIn: in.WhatsappOptIn, WhatsappPhone: in.WhatsappPhone,
	}
	if err := u.managers.Create(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (u *managerUseCase) Remove(ctx context.Context, id uuid.UUID) error {
	return u.managers.Delete(ctx, id)
}

func (u *managerUseCase) Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	ms, err := u.managers.ListByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.RestaurantID == restaurantID {
			return true, nil
		}
	}
	return false, nil
}
```

- [ ] **Step 4: Write `fakes_test.go`**

```go
package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeRestaurantRepo struct {
	created  *domain.Restaurant
	updated  *domain.Restaurant
	getErr   error
	agg      *domain.RestaurantAggregate
	list     []domain.RestaurantListItem
	total    int
	activeID uuid.UUID
	active   bool
}

func (f *fakeRestaurantRepo) Create(_ context.Context, r *domain.Restaurant) error { f.created = r; return nil }
func (f *fakeRestaurantRepo) Update(_ context.Context, r *domain.Restaurant) error { f.updated = r; return nil }
func (f *fakeRestaurantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.agg, nil
}
func (f *fakeRestaurantRepo) ListActive(_ context.Context, _ domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	return f.list, f.total, nil
}
func (f *fakeRestaurantRepo) SetActive(_ context.Context, id uuid.UUID, a bool) error {
	f.activeID, f.active = id, a
	return nil
}

type fakeRelated struct{ replaced int }

func (f *fakeRelated) ListImages(context.Context, uuid.UUID) ([]domain.Image, error)             { return nil, nil }
func (f *fakeRelated) ListFeatures(context.Context, uuid.UUID) ([]domain.Feature, error)         { return nil, nil }
func (f *fakeRelated) ListTags(context.Context, uuid.UUID) ([]domain.Tag, error)                 { return nil, nil }
func (f *fakeRelated) ListSocialLinks(context.Context, uuid.UUID) ([]domain.SocialLink, error)   { return nil, nil }
func (f *fakeRelated) ListWorkingHours(context.Context, uuid.UUID) ([]domain.WorkingHours, error) { return nil, nil }
func (f *fakeRelated) ListTimeSlots(context.Context, uuid.UUID) ([]domain.TimeSlot, error)       { return nil, nil }
func (f *fakeRelated) ListTables(context.Context, uuid.UUID) ([]domain.RestaurantTable, error)   { return nil, nil }
func (f *fakeRelated) GetFloorPlan(context.Context, uuid.UUID) (*domain.FloorPlan, error)        { return nil, domain.ErrNotFound }
func (f *fakeRelated) ReplaceImages(context.Context, uuid.UUID, []domain.Image) error            { f.replaced++; return nil }
func (f *fakeRelated) ReplaceFeatures(context.Context, uuid.UUID, []domain.Feature) error        { f.replaced++; return nil }
func (f *fakeRelated) ReplaceTags(context.Context, uuid.UUID, []domain.Tag) error                { f.replaced++; return nil }
func (f *fakeRelated) ReplaceSocialLinks(context.Context, uuid.UUID, []domain.SocialLink) error  { f.replaced++; return nil }
func (f *fakeRelated) ReplaceWorkingHours(context.Context, uuid.UUID, []domain.WorkingHours) error { return nil }
func (f *fakeRelated) ReplaceTimeSlots(context.Context, uuid.UUID, []domain.TimeSlot) error      { return nil }
func (f *fakeRelated) ReplaceTables(context.Context, uuid.UUID, []domain.RestaurantTable) error  { return nil }
func (f *fakeRelated) UpsertFloorPlan(context.Context, *domain.FloorPlan) error                  { return nil }

type fakeCategories struct{ items []domain.RestaurantCategory }

func (f *fakeCategories) List(context.Context) ([]domain.RestaurantCategory, error) { return f.items, nil }
func (f *fakeCategories) Create(context.Context, *domain.RestaurantCategory) error  { return nil }

type fakePartners struct{ created *domain.PartnershipRequest }

func (f *fakePartners) Create(_ context.Context, p *domain.PartnershipRequest) error { f.created = p; return nil }

type fakeManagers struct {
	byUser  []domain.RestaurantManager
	created *domain.RestaurantManager
	delErr  error
}

func (f *fakeManagers) ListByRestaurant(context.Context, uuid.UUID) ([]domain.RestaurantManager, error) { return nil, nil }
func (f *fakeManagers) ListByUser(context.Context, uuid.UUID) ([]domain.RestaurantManager, error) { return f.byUser, nil }
func (f *fakeManagers) Create(_ context.Context, m *domain.RestaurantManager) error { f.created = m; return nil }
func (f *fakeManagers) Delete(context.Context, uuid.UUID) error { return f.delErr }

type fakeUsers struct{ err error }

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.User{ID: id}, nil
}

// inlineTx runs fn directly (no real transaction) for unit tests.
type inlineTx struct{}

func (inlineTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
```

- [ ] **Step 5: Write `facade_test.go`**

```go
package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestCreateValidatesAndSavesCollections(t *testing.T) {
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{}}
	rel := &fakeRelated{}
	f := NewFacade(repo, rel, &fakeCategories{}, &fakePartners{}, inlineTx{})

	_, err := f.Create(context.Background(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Ok", City: domain.CityAlmaty, PriceCategory: domain.PriceLow},
		Images:     []domain.Image{{ImageURL: "a"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.created == nil || repo.created.ID == uuid.Nil {
		t.Error("expected restaurant created with generated ID")
	}
	if rel.replaced != 4 { // images, features, tags, social
		t.Errorf("replaced collections = %d, want 4", rel.replaced)
	}
}

func TestCreateRejectsInvalidCity(t *testing.T) {
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, inlineTx{})
	_, err := f.Create(context.Background(), SaveInput{
		Restaurant: domain.Restaurant{Name: "Bad", City: "Nowhere", PriceCategory: domain.PriceLow},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestSubmitPartnershipValidates(t *testing.T) {
	p := &fakePartners{}
	f := NewFacade(&fakeRestaurantRepo{}, &fakeRelated{}, &fakeCategories{}, p, inlineTx{})
	if err := f.SubmitPartnership(context.Background(), PartnershipInput{}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("empty submit err = %v, want ErrValidation", err)
	}
	if err := f.SubmitPartnership(context.Background(), PartnershipInput{
		RestaurantName: "R", ContactName: "C", Email: "e@x.io", Phone: "+7700",
	}); err != nil {
		t.Fatalf("valid submit: %v", err)
	}
	if p.created == nil || p.created.Status != "pending" {
		t.Error("expected partnership request created with pending status")
	}
}

func TestManagerAssignChecksUserExists(t *testing.T) {
	u := NewManagerUseCase(&fakeManagers{}, &fakeUsers{err: domain.ErrNotFound})
	if _, err := u.Assign(context.Background(), AssignManagerInput{UserID: uuid.New(), RestaurantID: uuid.New()}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("assign missing user err = %v, want ErrNotFound", err)
	}
}

func TestManagerManages(t *testing.T) {
	rid := uuid.New()
	fm := &fakeManagers{byUser: []domain.RestaurantManager{{RestaurantID: rid}}}
	u := NewManagerUseCase(fm, &fakeUsers{})
	ok, err := u.Manages(context.Background(), uuid.New(), rid)
	if err != nil || !ok {
		t.Errorf("Manages = %v, %v; want true, nil", ok, err)
	}
	ok, _ = u.Manages(context.Background(), uuid.New(), uuid.New())
	if ok {
		t.Error("Manages = true for unrelated restaurant, want false")
	}
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/usecase/restaurants/ -v`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
gofmt -w . && go vet ./...
git add internal/usecase/restaurants/
git commit -m "feat(usecase): add restaurants facade and manager usecase"
```

---

### Task 7: Transport layer + wiring

**Files:**
- Create: `internal/transport/rest/restaurants/response.go`
- Create: `internal/transport/rest/restaurants/request.go`
- Create: `internal/transport/rest/restaurants/handler.go`
- Modify: `internal/bootstrap/deps.go`
- Modify: `internal/bootstrap/app.go`

**Interfaces:**
- Consumes: `restaurants.Facade`, `restaurants.ManagerUseCase`, `middleware.RequireRole`, `middleware.GetAuthUser`, `response.*`.
- Produces: `restaurants.NewHandler(facade, managers).RegisterPublic(rg)` and `.RegisterAdmin(rg)`; `Deps.RestaurantsFacade`, `Deps.RestaurantManagers`.

- [ ] **Step 1: Write `response.go`**

```go
package restaurants

import (
	"time"

	"backend-core/internal/domain"
)

type restaurantResponse struct {
	ID            string            `json:"id"`
	CategoryID    *string           `json:"category_id"`
	Name          string            `json:"name"`
	NameI18n      map[string]string `json:"name_i18n,omitempty"`
	Description   string            `json:"description"`
	CuisineType   string            `json:"cuisine_type"`
	Address       string            `json:"address"`
	OpeningHours  string            `json:"opening_hours"`
	City          string            `json:"city"`
	PriceCategory string            `json:"price_category"`
	Email         string            `json:"email"`
	Phone         string            `json:"phone"`
	Latitude      *float64          `json:"latitude"`
	Longitude     *float64          `json:"longitude"`
	IsActive      bool              `json:"is_active"`
	IsNew         *bool             `json:"is_new"`
	IsPopular     *bool             `json:"is_popular"`
	IsPremium     *bool             `json:"is_premium"`
	DisplayOrder  *int              `json:"display_order"`
	PrimaryImage  *string           `json:"primary_image,omitempty"`
	Images        []imageResponse   `json:"images,omitempty"`
	Features      []featureResponse `json:"features,omitempty"`
	Tags          []tagResponse     `json:"tags,omitempty"`
	SocialLinks   []socialResponse  `json:"social_links,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

type imageResponse struct {
	ID        string `json:"id"`
	ImageURL  string `json:"image_url"`
	IsPrimary bool   `json:"is_primary"`
}
type featureResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
}
type tagResponse struct {
	ID          string            `json:"id"`
	TagName     string            `json:"tag_name"`
	TagNameI18n map[string]string `json:"tag_name_i18n,omitempty"`
}
type socialResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}
type categoryResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
}
type managerResponse struct {
	ID            string  `json:"id"`
	RestaurantID  string  `json:"restaurant_id"`
	UserID        string  `json:"user_id"`
	WhatsappOptIn bool    `json:"whatsapp_opt_in"`
	WhatsappPhone *string `json:"whatsapp_phone"`
}

func baseFromDomain(r domain.Restaurant) restaurantResponse {
	var cat *string
	if r.CategoryID != nil {
		s := r.CategoryID.String()
		cat = &s
	}
	return restaurantResponse{
		ID: r.ID.String(), CategoryID: cat, Name: r.Name, NameI18n: r.NameI18n,
		Description: r.Description, CuisineType: r.CuisineType, Address: r.Address,
		OpeningHours: r.OpeningHours, City: string(r.City), PriceCategory: string(r.PriceCategory),
		Email: r.Email, Phone: r.Phone, Latitude: r.Latitude, Longitude: r.Longitude,
		IsActive: r.IsActive, IsNew: r.IsNew, IsPopular: r.IsPopular, IsPremium: r.IsPremium,
		DisplayOrder: r.DisplayOrder, CreatedAt: r.CreatedAt,
	}
}

func listItemToResponse(it domain.RestaurantListItem) restaurantResponse {
	resp := baseFromDomain(it.Restaurant)
	resp.PrimaryImage = it.PrimaryImage
	return resp
}

func aggregateToResponse(a *domain.RestaurantAggregate) restaurantResponse {
	resp := baseFromDomain(a.Restaurant)
	for _, i := range a.Images {
		resp.Images = append(resp.Images, imageResponse{ID: i.ID.String(), ImageURL: i.ImageURL, IsPrimary: i.IsPrimary})
		if i.IsPrimary && resp.PrimaryImage == nil {
			u := i.ImageURL
			resp.PrimaryImage = &u
		}
	}
	for _, f := range a.Features {
		resp.Features = append(resp.Features, featureResponse{ID: f.ID.String(), Name: f.Name, NameI18n: f.NameI18n})
	}
	for _, t := range a.Tags {
		resp.Tags = append(resp.Tags, tagResponse{ID: t.ID.String(), TagName: t.TagName, TagNameI18n: t.TagNameI18n})
	}
	for _, s := range a.SocialLinks {
		resp.SocialLinks = append(resp.SocialLinks, socialResponse{ID: s.ID.String(), Type: s.Type, URL: s.URL})
	}
	return resp
}

func categoryToResponse(c domain.RestaurantCategory) categoryResponse {
	return categoryResponse{ID: c.ID.String(), Name: c.Name, NameI18n: c.NameI18n}
}

func managerToResponse(m domain.RestaurantManager) managerResponse {
	return managerResponse{
		ID: m.ID.String(), RestaurantID: m.RestaurantID.String(), UserID: m.UserID.String(),
		WhatsappOptIn: m.WhatsappOptIn, WhatsappPhone: m.WhatsappPhone,
	}
}
```

- [ ] **Step 2: Write `request.go`**

```go
package restaurants

import (
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/restaurants"
)

type saveRestaurantRequest struct {
	CategoryID    *string           `json:"category_id"`
	Name          string            `json:"name"`
	NameI18n      map[string]string `json:"name_i18n"`
	Description   string            `json:"description"`
	CuisineType   string            `json:"cuisine_type"`
	Address       string            `json:"address"`
	OpeningHours  string            `json:"opening_hours"`
	City          string            `json:"city"`
	PriceCategory string            `json:"price_category"`
	Email         string            `json:"email"`
	Phone         string            `json:"phone"`
	Latitude      *float64          `json:"latitude"`
	Longitude     *float64          `json:"longitude"`
	IsActive      *bool             `json:"is_active"`
	IsNew         *bool             `json:"is_new"`
	IsPopular     *bool             `json:"is_popular"`
	IsPremium     *bool             `json:"is_premium"`
	DisplayOrder  *int              `json:"display_order"`
	Images        []imageInput      `json:"images"`
	Features      []featureInput    `json:"features"`
	Tags          []tagInput        `json:"tags"`
	SocialLinks   []socialInput     `json:"social_links"`
}

type imageInput struct {
	ImageURL  string `json:"image_url"`
	IsPrimary bool   `json:"is_primary"`
}
type featureInput struct {
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n"`
}
type tagInput struct {
	TagName     string            `json:"tag_name"`
	TagNameI18n map[string]string `json:"tag_name_i18n"`
}
type socialInput struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func (r saveRestaurantRequest) toInput() uc.SaveInput {
	rest := domain.Restaurant{
		Name: r.Name, NameI18n: r.NameI18n, Description: r.Description,
		CuisineType: r.CuisineType, Address: r.Address, OpeningHours: r.OpeningHours,
		City: domain.City(r.City), PriceCategory: domain.PriceCategory(r.PriceCategory),
		Email: r.Email, Phone: r.Phone, Latitude: r.Latitude, Longitude: r.Longitude,
		IsNew: r.IsNew, IsPopular: r.IsPopular, IsPremium: r.IsPremium, DisplayOrder: r.DisplayOrder,
	}
	rest.IsActive = true
	if r.IsActive != nil {
		rest.IsActive = *r.IsActive
	}
	if r.CategoryID != nil {
		if id, err := uuid.Parse(*r.CategoryID); err == nil {
			rest.CategoryID = &id
		}
	}
	in := uc.SaveInput{Restaurant: rest}
	for _, i := range r.Images {
		in.Images = append(in.Images, domain.Image{ImageURL: i.ImageURL, IsPrimary: i.IsPrimary})
	}
	for _, f := range r.Features {
		in.Features = append(in.Features, domain.Feature{Name: f.Name, NameI18n: f.NameI18n})
	}
	for _, t := range r.Tags {
		in.Tags = append(in.Tags, domain.Tag{TagName: t.TagName, TagNameI18n: t.TagNameI18n})
	}
	for _, s := range r.SocialLinks {
		in.SocialLinks = append(in.SocialLinks, domain.SocialLink{Type: s.Type, URL: s.URL})
	}
	return in
}

type partnershipRequest struct {
	RestaurantName string  `json:"restaurant_name"`
	ContactName    string  `json:"contact_name"`
	Email          string  `json:"email"`
	Phone          string  `json:"phone"`
	Address        string  `json:"address"`
	CuisineType    *string `json:"cuisine_type"`
	Description    *string `json:"description"`
	AdditionalInfo *string `json:"additional_info"`
}

func (r partnershipRequest) toInput() uc.PartnershipInput {
	return uc.PartnershipInput{
		RestaurantName: r.RestaurantName, ContactName: r.ContactName, Email: r.Email,
		Phone: r.Phone, Address: r.Address, CuisineType: r.CuisineType,
		Description: r.Description, AdditionalInfo: r.AdditionalInfo,
	}
}

type assignManagerRequest struct {
	UserID        string  `json:"user_id"`
	WhatsappOptIn bool    `json:"whatsapp_opt_in"`
	WhatsappPhone *string `json:"whatsapp_phone"`
}
```

- [ ] **Step 3: Write `handler.go`**

```go
// Package restaurants exposes the restaurant catalog HTTP endpoints.
package restaurants

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/restaurants"
)

type Handler struct {
	facade   uc.Facade
	managers uc.ManagerUseCase
}

func NewHandler(f uc.Facade, m uc.ManagerUseCase) *Handler {
	return &Handler{facade: f, managers: m}
}

// RegisterPublic mounts the unauthenticated catalog routes.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants", h.list)
	rg.GET("/restaurants/:id", h.get)
	rg.GET("/restaurant-categories", h.categories)
	rg.POST("/partnership-requests", h.submitPartnership)
}

// RegisterAdmin mounts routes on a group already gated by RequireRole(admin[,restaurant]).
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.POST("/restaurants", h.create)
	rg.PATCH("/restaurants/:id", h.update)
	rg.DELETE("/restaurants/:id", h.deactivate)
	rg.GET("/restaurants/:id/managers", h.listManagers)
	rg.POST("/restaurants/:id/managers", h.assignManager)
	rg.DELETE("/restaurants/:id/managers/:managerID", h.removeManager)
}

func (h *Handler) list(c *gin.Context) {
	f := domain.RestaurantFilter{Search: c.Query("search")}
	if v := c.Query("city"); v != "" {
		city := domain.City(v)
		f.City = &city
	}
	if v := c.Query("category"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			f.Category = &id
		}
	}
	if v := c.Query("is_popular"); v != "" {
		b := v == "true"
		f.IsPopular = &b
	}
	if v := c.Query("is_new"); v != "" {
		b := v == "true"
		f.IsNew = &b
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))

	items, total, err := h.facade.List(c.Request.Context(), f)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]restaurantResponse, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it))
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	agg, err := h.facade.Get(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, aggregateToResponse(agg))
}

func (h *Handler) categories(c *gin.Context) {
	cats, err := h.facade.Categories(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]categoryResponse, 0, len(cats))
	for _, cat := range cats {
		out = append(out, categoryToResponse(cat))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) submitPartnership(c *gin.Context) {
	var req partnershipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.facade.SubmitPartnership(c.Request.Context(), req.toInput()); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, gin.H{"status": "received"})
}

func (h *Handler) create(c *gin.Context) {
	var req saveRestaurantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	agg, err := h.facade.Create(c.Request.Context(), req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, aggregateToResponse(agg))
}

func (h *Handler) update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req saveRestaurantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	agg, err := h.facade.Update(c.Request.Context(), id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, aggregateToResponse(agg))
}

func (h *Handler) deactivate(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	if err := h.facade.SetActive(c.Request.Context(), id, false); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deactivated"})
}

func (h *Handler) listManagers(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	ms, err := h.managers.List(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]managerResponse, 0, len(ms))
	for _, m := range ms {
		out = append(out, managerToResponse(m))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) assignManager(c *gin.Context) {
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req assignManagerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	uid, err := uuid.Parse(req.UserID)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid user_id")
		return
	}
	var createdBy *uuid.UUID
	if au, ok := middleware.GetAuthUser(c.Request.Context()); ok {
		createdBy = &au.ID
	}
	m, err := h.managers.Assign(c.Request.Context(), uc.AssignManagerInput{
		RestaurantID: rid, UserID: uid, CreatedBy: createdBy,
		WhatsappOptIn: req.WhatsappOptIn, WhatsappPhone: req.WhatsappPhone,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, managerToResponse(*m))
}

func (h *Handler) removeManager(c *gin.Context) {
	mid, err := uuid.Parse(c.Param("managerID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid manager id")
		return
	}
	if err := h.managers.Remove(c.Request.Context(), mid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "removed"})
}
```

> Note: `middleware.GetAuthUser` returns `AuthUser` whose `ID` is a `uuid.UUID` (see `middleware/auth.go`). `createdBy = &au.ID` relies on that; if `AuthUser.ID` is a different type in the current code, convert it here.

- [ ] **Step 4: Wire into `deps.go`**

In `internal/bootstrap/deps.go`, add imports and fields, and construct the usecases. Add to the import block:

```go
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/usecase/restaurants"
```

Add fields to `Deps`:

```go
	RestaurantsFacade  restaurants.Facade
	RestaurantManagers restaurants.ManagerUseCase
```

In `NewDeps`, after the existing repo construction, add:

```go
	restRepo := restrepo.New(db)
	restRelated := restrepo.NewRelated(db)
	restCategories := restrepo.NewCategories(db)
	restManagers := restrepo.NewManagers(db)
	restPartners := restrepo.NewPartnership(db)
```

And set the returned struct fields:

```go
		RestaurantsFacade:  restaurants.NewFacade(restRepo, restRelated, restCategories, restPartners, txm),
		RestaurantManagers: restaurants.NewManagerUseCase(restManagers, usersRepo),
```

- [ ] **Step 5: Wire routes into `app.go`**

In `internal/bootstrap/app.go`, add the import:

```go
	restrest "backend-core/internal/transport/rest/restaurants"
```

In `NewApp`, register public routes on the unauthenticated `api` group and admin routes on a role-gated subgroup of `authed`:

```go
	restHandler := restrest.NewHandler(deps.RestaurantsFacade, deps.RestaurantManagers)
	restHandler.RegisterPublic(api)

	adminRest := authed.Group("")
	adminRest.Use(middleware.RequireRole(domain.RoleAdmin, domain.RoleRestaurant))
	restHandler.RegisterAdmin(adminRest)
```

Add `"backend-core/internal/domain"` to `app.go` imports if not present.

- [ ] **Step 6: Build, vet, run smoke check**

Run: `go build ./... && go vet ./... && go test -short ./...`
Expected: build + vet clean; all unit tests PASS (integration skipped).

- [ ] **Step 7: Commit**

```bash
gofmt -w .
git add internal/transport/rest/restaurants/ internal/bootstrap/deps.go internal/bootstrap/app.go
git commit -m "feat(rest): expose restaurant catalog endpoints and wire deps"
```

---

### Task 8: ETL — restaurants subcommand

**Files:**
- Modify: `cmd/etl/main.go`
- Create: `cmd/etl/restaurants.go`

**Interfaces:**
- Consumes: `raw_supabase.*` staging tables, `bootstrap.NewSQLDB`.
- Produces: `runRestaurants(ctx, db, log) error`; `cmd/etl` dispatches on the first CLI arg (`users` default, or `restaurants`).

- [ ] **Step 1: Make `main.go` dispatch on a subcommand**

In `cmd/etl/main.go`, replace the `run(...)` call in `main` with a dispatch. Change:

```go
	if err := run(context.Background(), db, log); err != nil {
```

to:

```go
	target := "users"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	var runErr error
	switch target {
	case "users":
		runErr = run(context.Background(), db, log)
	case "restaurants":
		runErr = runRestaurants(context.Background(), db, log)
	default:
		log.Error("unknown etl target", slog.String("target", target))
		os.Exit(1)
	}
	if runErr != nil {
```

and update the closing of that block so the existing `log.Error("etl failed", ...)` uses `runErr`:

```go
		log.Error("etl failed", slog.String("error", runErr.Error()))
		os.Exit(1)
	}
```

- [ ] **Step 2: Write `restaurants.go`**

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

// runRestaurants upserts the restaurant catalog from raw_supabase into the clean
// schema. Idempotent (upsert by id); FK order: categories → restaurants → children.
func runRestaurants(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	steps := []struct {
		name string
		sql  string
	}{
		{"categories", `
			INSERT INTO restaurant_categories (id, name, name_i18n, description, description_i18n, created_at)
			SELECT id, name, name_i18n, description, description_i18n, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_categories
			ON CONFLICT (id) DO UPDATE SET
			  name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n,
			  description=EXCLUDED.description, description_i18n=EXCLUDED.description_i18n`},
		{"restaurants", `
			INSERT INTO restaurants (id, category_id, name, name_i18n, description, description_i18n,
			  cuisine_type, cuisine_type_i18n, address, address_i18n, opening_hours, opening_hours_i18n,
			  city, price_category, email, phone, latitude, longitude, kwaaka_restaurant_id,
			  is_active, is_new, is_popular, is_premium, hidden_from_home, display_order, created_at, updated_at)
			SELECT id, category_id, name, name_i18n, COALESCE(description,''), description_i18n,
			  COALESCE(cuisine_type,''), cuisine_type_i18n, COALESCE(address,''), address_i18n,
			  COALESCE(opening_hours,''), opening_hours_i18n, city::text, price_category::text,
			  COALESCE(email,''), COALESCE(phone,''), latitude, longitude, kwaaka_restaurant_id,
			  COALESCE(is_active,true), is_new, is_popular, is_premium, COALESCE(hidden_from_home,false),
			  display_order, COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurants
			ON CONFLICT (id) DO UPDATE SET
			  category_id=EXCLUDED.category_id, name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n,
			  description=EXCLUDED.description, description_i18n=EXCLUDED.description_i18n,
			  cuisine_type=EXCLUDED.cuisine_type, cuisine_type_i18n=EXCLUDED.cuisine_type_i18n,
			  address=EXCLUDED.address, address_i18n=EXCLUDED.address_i18n,
			  opening_hours=EXCLUDED.opening_hours, opening_hours_i18n=EXCLUDED.opening_hours_i18n,
			  city=EXCLUDED.city, price_category=EXCLUDED.price_category, email=EXCLUDED.email,
			  phone=EXCLUDED.phone, latitude=EXCLUDED.latitude, longitude=EXCLUDED.longitude,
			  kwaaka_restaurant_id=EXCLUDED.kwaaka_restaurant_id, is_active=EXCLUDED.is_active,
			  is_new=EXCLUDED.is_new, is_popular=EXCLUDED.is_popular, is_premium=EXCLUDED.is_premium,
			  hidden_from_home=EXCLUDED.hidden_from_home, display_order=EXCLUDED.display_order,
			  updated_at=EXCLUDED.updated_at`},
		{"features", `
			INSERT INTO restaurant_features (id, restaurant_id, name, name_i18n, created_at)
			SELECT id, restaurant_id, name, name_i18n, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_features
			ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n`},
		{"images", `
			INSERT INTO restaurant_images (id, restaurant_id, image_url, is_primary, created_at)
			SELECT id, restaurant_id, image_url, COALESCE(is_primary,false), COALESCE(created_at, now())
			FROM raw_supabase.restaurant_images
			ON CONFLICT (id) DO UPDATE SET image_url=EXCLUDED.image_url, is_primary=EXCLUDED.is_primary`},
		{"tags", `
			INSERT INTO restaurant_tags (id, restaurant_id, tag_name, tag_name_i18n, created_at)
			SELECT id, restaurant_id, tag_name, tag_name_i18n, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_tags
			ON CONFLICT (id) DO UPDATE SET tag_name=EXCLUDED.tag_name, tag_name_i18n=EXCLUDED.tag_name_i18n`},
		{"social_links", `
			INSERT INTO restaurant_social_links (id, restaurant_id, type, url, created_at)
			SELECT id, restaurant_id, type, url, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_social_links
			ON CONFLICT (id) DO UPDATE SET type=EXCLUDED.type, url=EXCLUDED.url`},
		{"working_hours", `
			INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open, created_at, updated_at)
			SELECT id, restaurant_id, day_of_week, open_time::text, close_time::text, COALESCE(is_open,true),
			  COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_working_hours
			ON CONFLICT (id) DO UPDATE SET day_of_week=EXCLUDED.day_of_week, open_time=EXCLUDED.open_time,
			  close_time=EXCLUDED.close_time, is_open=EXCLUDED.is_open, updated_at=EXCLUDED.updated_at`},
		{"time_slots", `
			INSERT INTO restaurant_time_slots (id, restaurant_id, day_of_week, start_time, end_time, is_manually_disabled, created_at, updated_at)
			SELECT id, restaurant_id, day_of_week, start_time::text, end_time::text, COALESCE(is_manually_disabled,false),
			  COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_time_slots
			ON CONFLICT (id) DO UPDATE SET day_of_week=EXCLUDED.day_of_week, start_time=EXCLUDED.start_time,
			  end_time=EXCLUDED.end_time, is_manually_disabled=EXCLUDED.is_manually_disabled, updated_at=EXCLUDED.updated_at`},
		{"tables", `
			INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, description, is_active, created_at, updated_at)
			SELECT id, restaurant_id, name, COALESCE(capacity,0), description, COALESCE(is_active,true),
			  COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_tables
			ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, capacity=EXCLUDED.capacity,
			  description=EXCLUDED.description, is_active=EXCLUDED.is_active, updated_at=EXCLUDED.updated_at`},
		{"floor_plans", `
			INSERT INTO restaurant_floor_plans (id, restaurant_id, layout_data, created_at, updated_at)
			SELECT id, restaurant_id, layout_data, COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_floor_plans
			ON CONFLICT (id) DO UPDATE SET layout_data=EXCLUDED.layout_data, updated_at=EXCLUDED.updated_at`},
		{"managers", `
			INSERT INTO restaurant_managers (id, restaurant_id, user_id, created_by, whatsapp_opt_in, whatsapp_phone, created_at)
			SELECT m.id, m.restaurant_id, m.user_id, m.created_by, COALESCE(m.whatsapp_opt_in,false), m.whatsapp_phone, COALESCE(m.created_at, now())
			FROM raw_supabase.restaurant_managers m
			JOIN users u ON u.id = m.user_id
			ON CONFLICT (id) DO UPDATE SET whatsapp_opt_in=EXCLUDED.whatsapp_opt_in, whatsapp_phone=EXCLUDED.whatsapp_phone`},
		{"partnership_requests", `
			INSERT INTO restaurant_partnership_requests (id, restaurant_name, contact_name, email, phone, address, cuisine_type, description, additional_info, status, created_at, updated_at)
			SELECT id, restaurant_name, contact_name, email, phone, COALESCE(address,''), cuisine_type, description, additional_info,
			  COALESCE(status,'pending'), COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_partnership_requests
			ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, updated_at=EXCLUDED.updated_at`},
	}

	var totalRestaurants int
	for _, s := range steps {
		res, err := db.ExecContext(ctx, s.sql)
		if err != nil {
			return errors.New("etl step " + s.name + ": " + err.Error())
		}
		n, _ := res.RowsAffected()
		if s.name == "restaurants" {
			totalRestaurants = int(n)
		}
		log.Info("etl step done", slog.String("step", s.name), slog.Int64("rows", n))
	}
	if totalRestaurants == 0 {
		return errors.New("no restaurants found in raw_supabase — is the dump loaded?")
	}
	log.Info("restaurants etl complete", slog.Int("restaurants", totalRestaurants))
	return nil
}
```

> Note: the `managers` step JOINs `users` so manager rows referencing users that
> failed to migrate are silently skipped (satisfies the spec's "orphans logged
> and skipped, don't fail the run"). If you want an explicit skip count, add a
> `LEFT JOIN ... WHERE u.id IS NULL` diagnostic query and log its count.

- [ ] **Step 3: Build and verify dispatch**

Run: `go build ./cmd/etl/ && go vet ./cmd/etl/`
Expected: build + vet clean. (Full run requires a loaded `raw_supabase` dump; verify manually at cutover per the spec.)

- [ ] **Step 4: Commit**

```bash
gofmt -w .
git add cmd/etl/main.go cmd/etl/restaurants.go
git commit -m "feat(etl): add idempotent restaurants migration subcommand"
```

---

## Final verification

- [ ] **Run the full unit suite:** `go test -short ./...` → all PASS.
- [ ] **Run integration suite** (against a migrated DB): `TEST_DATABASE_URL=postgres://... go test ./...` → all PASS.
- [ ] **Vet + format:** `go vet ./... && gofmt -l .` → no output.
- [ ] **Manual smoke:** `make run`, then `curl localhost:<port>/api/v1/restaurants` returns an empty page envelope; after loading data, returns restaurants ordered by display_order then name.

## Self-review notes (coverage vs. spec)

- Spec §1 entities → Tasks 2, 4, 5 (all tables incl. partnership; surveys correctly excluded).
- Spec §2 schema (VARCHAR enums, jsonb i18n, UUID PKs, indexes) → Task 1.
- Spec §3 domain (one file per entity, aggregate read shape) → Task 2, 4 (`GetByID` aggregate).
- Spec §4 usecase (Facade + ManagerUseCase + ports; TxManager for atomic collection writes) → Task 6. Availability *read* of slots/hours/tables is provided by `RestaurantRelatedRepository.ListWorkingHours/ListTimeSlots/ListTables` (surfaced via detail endpoints in later waves); no availability *computation* here, per spec.
- Spec §5 transport (public reads, category list, partnership POST, admin/manager-gated mutations, Envelope + HandleError, `/api/v1` prefix) → Tasks 3, 7.
- Spec §6 ETL (idempotent, FK order, UUID/i18n verbatim, orphan managers skipped) → Task 8.
- Spec §7 DoD → Final verification section.
