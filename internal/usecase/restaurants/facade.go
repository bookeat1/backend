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
