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
	// Search runs the full-text + fuzzy catalog search (a distinct endpoint
	// from List, which keeps its existing response shape untouched).
	Search(ctx context.Context, f domain.RestaurantSearchFilter) ([]domain.RestaurantListItem, int, error)
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

// SaveInput carries mutable restaurant fields plus inline collections for
// create/update. Every scalar field and collection is a pointer so the facade
// can distinguish "absent from the request" (nil → preserve on Update) from
// "explicitly provided". On Update the facade loads the existing row and
// overlays only the provided fields (read-modify-write), so a PATCH that omits
// a field never wipes it — including server-managed columns this input can't
// even address (kwaaka_restaurant_id, hidden_from_home), which are always
// carried over from the stored row.
type SaveInput struct {
	CategoryID    *uuid.UUID
	Name          *string
	NameI18n      domain.I18n
	Description   *string
	CuisineType   *string
	Address       *string
	OpeningHours  *string
	City          *string
	PriceCategory *string
	Email         *string
	Phone         *string
	Latitude      *float64
	Longitude     *float64
	IsActive      *bool // nil = leave is_active unchanged (Update) / default true (Create)
	IsNew         *bool
	IsPopular     *bool
	IsPremium     *bool
	DisplayOrder  *int

	Images      *[]domain.Image // nil = collection not provided (preserve on Update)
	Features    *[]domain.Feature
	Tags        *[]domain.Tag
	SocialLinks *[]domain.SocialLink
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

func (f *facade) Search(ctx context.Context, flt domain.RestaurantSearchFilter) ([]domain.RestaurantListItem, int, error) {
	return f.repo.Search(ctx, flt)
}

func (f *facade) Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	return f.repo.GetByID(ctx, id)
}

func (f *facade) Categories(ctx context.Context) ([]domain.RestaurantCategory, error) {
	return f.categories.List(ctx)
}

func (f *facade) Create(ctx context.Context, in SaveInput) (*domain.RestaurantAggregate, error) {
	if err := validateProvided(in); err != nil {
		return nil, err
	}
	rest := domain.Restaurant{ID: uuid.New(), IsActive: true}
	applyRestaurant(&rest, in)
	if err := validateRestaurant(rest); err != nil {
		return nil, err
	}
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.repo.Create(ctx, &rest); err != nil {
			return err
		}
		return f.saveAllCollections(ctx, in, rest.ID)
	})
	if err != nil {
		return nil, err
	}
	return f.repo.GetByID(ctx, rest.ID)
}

func (f *facade) Update(ctx context.Context, id uuid.UUID, in SaveInput) (*domain.RestaurantAggregate, error) {
	if err := validateProvided(in); err != nil {
		return nil, err
	}
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		existing, err := f.repo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		// Read-modify-write: start from the stored row and overlay only the
		// fields the request actually provided. Untouched columns (including
		// is_active, kwaaka_restaurant_id and hidden_from_home) keep their
		// existing values instead of being reset to the zero value.
		rest := existing.Restaurant
		applyRestaurant(&rest, in)
		rest.ID = id
		if err := f.repo.Update(ctx, &rest); err != nil {
			return err
		}
		return f.saveProvidedCollections(ctx, in, id)
	})
	if err != nil {
		return nil, err
	}
	return f.repo.GetByID(ctx, id)
}

// saveAllCollections replaces all four inline collections, treating a nil
// pointer as an explicitly empty collection. Used by Create, where a
// brand-new restaurant has no prior rows to preserve.
func (f *facade) saveAllCollections(ctx context.Context, in SaveInput, rid uuid.UUID) error {
	if err := f.related.ReplaceImages(ctx, rid, deref(in.Images)); err != nil {
		return err
	}
	if err := f.related.ReplaceFeatures(ctx, rid, deref(in.Features)); err != nil {
		return err
	}
	if err := f.related.ReplaceTags(ctx, rid, deref(in.Tags)); err != nil {
		return err
	}
	return f.related.ReplaceSocialLinks(ctx, rid, deref(in.SocialLinks))
}

// saveProvidedCollections replaces only the collections explicitly present in
// in (non-nil pointer). Used by Update so that omitting a collection in a
// PATCH preserves its existing rows instead of wiping them.
func (f *facade) saveProvidedCollections(ctx context.Context, in SaveInput, rid uuid.UUID) error {
	if in.Images != nil {
		if err := f.related.ReplaceImages(ctx, rid, *in.Images); err != nil {
			return err
		}
	}
	if in.Features != nil {
		if err := f.related.ReplaceFeatures(ctx, rid, *in.Features); err != nil {
			return err
		}
	}
	if in.Tags != nil {
		if err := f.related.ReplaceTags(ctx, rid, *in.Tags); err != nil {
			return err
		}
	}
	if in.SocialLinks != nil {
		if err := f.related.ReplaceSocialLinks(ctx, rid, *in.SocialLinks); err != nil {
			return err
		}
	}
	return nil
}

// deref returns the empty/nil-slice value of *p, or nil if p is nil.
func deref[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
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

// applyRestaurant overlays the fields present in in (non-nil) onto m, leaving
// everything else — including columns in isn't able to address — untouched.
func applyRestaurant(m *domain.Restaurant, in SaveInput) {
	if in.CategoryID != nil {
		m.CategoryID = in.CategoryID
	}
	if in.Name != nil {
		m.Name = *in.Name
	}
	if in.NameI18n != nil {
		m.NameI18n = in.NameI18n
	}
	if in.Description != nil {
		m.Description = *in.Description
	}
	if in.CuisineType != nil {
		m.CuisineType = *in.CuisineType
	}
	if in.Address != nil {
		m.Address = *in.Address
	}
	if in.OpeningHours != nil {
		m.OpeningHours = *in.OpeningHours
	}
	if in.City != nil {
		m.City = domain.City(*in.City)
	}
	if in.PriceCategory != nil {
		m.PriceCategory = domain.PriceCategory(*in.PriceCategory)
	}
	if in.Email != nil {
		m.Email = *in.Email
	}
	if in.Phone != nil {
		m.Phone = *in.Phone
	}
	if in.Latitude != nil {
		m.Latitude = in.Latitude
	}
	if in.Longitude != nil {
		m.Longitude = in.Longitude
	}
	if in.IsActive != nil {
		m.IsActive = *in.IsActive
	}
	if in.IsNew != nil {
		m.IsNew = in.IsNew
	}
	if in.IsPopular != nil {
		m.IsPopular = in.IsPopular
	}
	if in.IsPremium != nil {
		m.IsPremium = in.IsPremium
	}
	if in.DisplayOrder != nil {
		m.DisplayOrder = in.DisplayOrder
	}
}

// validateProvided rejects invalid values for the enumerated/required fields
// that are actually present in in. It runs before both Create and Update so a
// bad value fails fast (422) without a DB round-trip; on Update the fields the
// request omits keep the stored row's already-valid values.
func validateProvided(in SaveInput) error {
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return domain.ErrValidation
	}
	if in.City != nil && !domain.City(*in.City).Valid() {
		return domain.ErrValidation
	}
	if in.PriceCategory != nil && !domain.PriceCategory(*in.PriceCategory).Valid() {
		return domain.ErrValidation
	}
	return nil
}

// validateRestaurant enforces that a fully-built restaurant has the required
// enumerated fields set. Used by Create (where name/city/price must be present).
func validateRestaurant(r domain.Restaurant) error {
	if strings.TrimSpace(r.Name) == "" || !r.City.Valid() || !r.PriceCategory.Valid() {
		return domain.ErrValidation
	}
	return nil
}
