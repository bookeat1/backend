package promocodes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// AdminPromoCode is a code as the cabinet sees it: the row plus the two things
// that live outside it — how many guests have actually joined (counted from
// the bookings, ADR-047: there is no counter column) and the REAL state of the
// campaign behind it.
//
// The promo's own status is carried deliberately: a code can be active while
// its campaign is hidden or already over, in which case guests get
// promo_code_inactive and nobody in the cabinet can see why. The listing shows
// both so the mismatch is visible instead of being a support ticket.
type AdminPromoCode struct {
	Code domain.PromoCode
	// Activations is how many DIFFERENT guests hold a live booking with this
	// code — the same number MaxUsesTotal is compared against.
	Activations int
	PromoTitle  string
	PromoStatus domain.PromoStatus
	PromoEndsAt time.Time
	// PromoMissing is true when the campaign row is gone. The database forbids
	// deleting it (ON DELETE RESTRICT), so this should be impossible; it is
	// surfaced rather than turned into an error so one broken row cannot make
	// the whole cabinet screen 500.
	PromoMissing bool
}

// CreatePromoCodeInput is what the cabinet form sends. The code string is
// normalized here, not by the client.
type CreatePromoCodeInput struct {
	Code           string
	PromotionID    uuid.UUID
	StartsAt       time.Time
	ExpiresAt      time.Time
	MaxUsesTotal   *int
	MaxUsesPerUser int
	Status         domain.PromoCodeStatus
}

// PatchPromoCodeInput is a PARTIAL update: nil means "leave it alone". It is a
// PATCH and not a PUT on purpose — a full replace of this shape is exactly how
// the cabinet has already lost fields on promos and events
// (bugs/bookeat-admin-full-replace-wipes-fields.md), and pause/resume is that
// same "same payload, one field different" call.
type PatchPromoCodeInput struct {
	StartsAt       *time.Time
	ExpiresAt      *time.Time
	MaxUsesTotal   **int
	MaxUsesPerUser *int
	Status         *domain.PromoCodeStatus
	// Code is accepted only so an unchanged value can be echoed back by a
	// client that sends the whole form; a DIFFERENT value is refused once the
	// code has been redeemed, because past bookings carry the old string.
	Code *string
}

// Editor is the cabinet's promo-code CRUD. It is a separate interface from
// Facade because the two have different callers and different authorization:
// Facade is reached by guests, Editor only from the superadmin-only route
// group (RequireRole(RoleAdmin) in bootstrap.NewApp).
type Editor interface {
	// List returns codes matching filter, each with its activation count and
	// the state of the campaign behind it.
	List(ctx context.Context, filter domain.PromoCodeFilter) ([]AdminPromoCode, error)
	// Get returns one code in the same shape as List.
	Get(ctx context.Context, id uuid.UUID) (*AdminPromoCode, error)
	// Create normalizes and validates the code, then inserts it. A duplicate
	// answers ErrAlreadyExists, an unknown promo ErrNotFound.
	Create(ctx context.Context, in CreatePromoCodeInput, actorID uuid.UUID) (*AdminPromoCode, error)
	// Patch applies the non-nil fields. Status moves are checked against
	// PromoCodeStatus.CanTransitionTo.
	Patch(ctx context.Context, id uuid.UUID, in PatchPromoCodeInput) (*AdminPromoCode, error)
	// Delete removes a code that NOBODY has redeemed. A code with activations
	// is refused with CodePromoCodeActivated — archive it instead.
	Delete(ctx context.Context, id uuid.UUID) error
}

// promoAdminReader reads a promo regardless of its visibility — the cabinet
// needs the real status, including the draft/hidden/ended ones that the
// guest-facing promoReader deliberately hides. Bound to the promo repository
// in bootstrap/deps.go.
type promoAdminReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Promo, error)
}

type editor struct {
	codes    domain.PromoCodeRepository
	promos   promoAdminReader
	bookings usageCounter
	now      func() time.Time
}

// NewEditor builds the cabinet-side promo-code usecase.
func NewEditor(codes domain.PromoCodeRepository, promos promoAdminReader, bookings usageCounter) Editor {
	return &editor{codes: codes, promos: promos, bookings: bookings, now: time.Now}
}

func (e *editor) List(ctx context.Context, filter domain.PromoCodeFilter) ([]AdminPromoCode, error) {
	codes, err := e.codes.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]AdminPromoCode, 0, len(codes))
	for _, c := range codes {
		item, err := e.decorate(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, nil
}

func (e *editor) Get(ctx context.Context, id uuid.UUID) (*AdminPromoCode, error) {
	c, err := e.codes.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return e.decorate(ctx, *c)
}

func (e *editor) Create(ctx context.Context, in CreatePromoCodeInput, actorID uuid.UUID) (*AdminPromoCode, error) {
	status := in.Status
	if status == "" {
		// A code nobody switched on yet. Defaulting to draft rather than
		// active means a half-filled form cannot start accepting guests.
		status = domain.PromoCodeDraft
	}
	perUser := in.MaxUsesPerUser
	if perUser == 0 {
		perUser = 1
	}
	c := domain.PromoCode{
		ID:             uuid.New(),
		Code:           domain.NormalizePromoCode(in.Code),
		PromotionID:    in.PromotionID,
		StartsAt:       in.StartsAt,
		ExpiresAt:      in.ExpiresAt,
		MaxUsesTotal:   in.MaxUsesTotal,
		MaxUsesPerUser: perUser,
		Status:         status,
	}
	if actorID != uuid.Nil {
		c.CreatedBy = &actorID
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := e.codes.Create(ctx, &c); err != nil {
		return nil, err
	}
	return e.decorate(ctx, c)
}

func (e *editor) Patch(ctx context.Context, id uuid.UUID, in PatchPromoCodeInput) (*AdminPromoCode, error) {
	c, err := e.codes.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	usage, err := e.bookings.CountPromoCodeUsage(ctx, id, uuid.Nil)
	if err != nil {
		return nil, err
	}
	if in.Code != nil {
		normalized := domain.NormalizePromoCode(*in.Code)
		if normalized != c.Code && usage.DistinctUsers > 0 {
			return nil, domain.WithCode(domain.CodePromoCodeActivated,
				fmt.Errorf("promo code %s has already been redeemed and cannot be renamed: %w",
					c.Code, domain.ErrValidation))
		}
		if normalized != c.Code {
			// The repository's Update deliberately does not touch the code
			// column, so even an unredeemed rename is refused rather than
			// silently ignored — a cabinet that thinks it renamed a code and
			// did not is worse than a visible 422.
			return nil, domain.WithCode(domain.CodePromoCodeActivated,
				fmt.Errorf("a promo code's string is immutable; create a new code: %w", domain.ErrValidation))
		}
	}
	if in.Status != nil {
		if !c.Status.CanTransitionTo(*in.Status) {
			return nil, domain.WithCode(domain.CodePromoCodeBadTransition,
				fmt.Errorf("promo code %s cannot go from %s to %s: %w",
					c.Code, c.Status, *in.Status, domain.ErrValidation))
		}
		c.Status = *in.Status
	}
	if in.StartsAt != nil {
		c.StartsAt = *in.StartsAt
	}
	if in.ExpiresAt != nil {
		c.ExpiresAt = *in.ExpiresAt
	}
	if in.MaxUsesTotal != nil {
		c.MaxUsesTotal = *in.MaxUsesTotal
	}
	if in.MaxUsesPerUser != nil {
		c.MaxUsesPerUser = *in.MaxUsesPerUser
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Lowering the overall limit below what is already spent is allowed and
	// deliberately not an error: it stops NEW guests without throwing out the
	// ones who already joined. checkLimits compares >=, so the code simply
	// stops accepting anybody new.
	if err := e.codes.Update(ctx, c); err != nil {
		return nil, err
	}
	return e.decorate(ctx, *c)
}

func (e *editor) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := e.codes.GetByID(ctx, id); err != nil {
		return err
	}
	usage, err := e.bookings.CountPromoCodeUsage(ctx, id, uuid.Nil)
	if err != nil {
		return err
	}
	if usage.DistinctUsers > 0 {
		return domain.WithCode(domain.CodePromoCodeActivated,
			fmt.Errorf("promo code has %d activation(s); archive it instead of deleting: %w",
				usage.DistinctUsers, domain.ErrValidation))
	}
	return e.codes.Delete(ctx, id)
}

// decorate attaches the activation count and the campaign's real state.
// CountPromoCodeUsage is called with uuid.Nil as the guest: the per-guest half
// of its answer is meaningless here and comes back as zero, while the distinct
// count — the one the cabinet shows — is exactly what a guest-side check sees.
func (e *editor) decorate(ctx context.Context, c domain.PromoCode) (*AdminPromoCode, error) {
	usage, err := e.bookings.CountPromoCodeUsage(ctx, c.ID, uuid.Nil)
	if err != nil {
		return nil, err
	}
	item := AdminPromoCode{Code: c, Activations: usage.DistinctUsers}
	promo, err := e.promos.GetByID(ctx, c.PromotionID)
	switch {
	case err == nil:
		item.PromoTitle, item.PromoStatus, item.PromoEndsAt = promo.Title, promo.Status, promo.EndsAt
	case errors.Is(err, domain.ErrNotFound):
		item.PromoMissing = true
	default:
		return nil, err
	}
	return &item, nil
}
