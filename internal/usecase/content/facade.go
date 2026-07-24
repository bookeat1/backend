// Package content is the application logic for the content-draft review queue
// (Ф2) — the source-agnostic seam a future AI content parser (and a
// hand-entering staff member) feeds candidate events/promos into. It is the
// human-in-the-loop gate: a draft NEVER auto-publishes; a staff member with
// PermRestaurantManage at the draft's own restaurant explicitly approves it
// (which creates the real PUBLISHED Event/Promo, atomically) or rejects it.
//
// There is deliberately no external HTTP submission endpoint in this increment.
// Submit exists as a repository-backed method for a future INTERNAL caller (the
// parser); wiring it to any transport must gate it behind auth (noted in the PR).
package content

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated staff caller for the review actions.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant".
// Bound to restaurants.ManagerUseCase in bootstrap.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// Facade exposes the staff review-queue operations plus the internal Submit
// seam for a future parser.
type Facade interface {
	// Submit inserts a new candidate draft in pending_review. Intended for a
	// future INTERNAL caller (the parser) — not wired to any external endpoint
	// here. Returns the stored draft.
	Submit(ctx context.Context, in SubmitInput) (*domain.ContentDraft, error)

	// ListPending returns a restaurant's pending drafts (the review queue),
	// paginated. Requires PermRestaurantManage at restaurantID.
	ListPending(ctx context.Context, actor Actor, restaurantID uuid.UUID, page, perPage int) ([]domain.ContentDraft, int, error)

	// Approve turns a pending draft into a real PUBLISHED Event/Promo and marks
	// the draft approved, atomically. Requires PermRestaurantManage at the
	// draft's own restaurant. ErrInvalidStatus if the draft is not pending.
	// Returns the created event id XOR promo id.
	Approve(ctx context.Context, actor Actor, draftID uuid.UUID) (*ApproveResult, error)

	// Reject marks a pending draft rejected; nothing is created. Same
	// authorization and CAS semantics as Approve.
	Reject(ctx context.Context, actor Actor, draftID uuid.UUID) (*domain.ContentDraft, error)
}

// SubmitInput carries a candidate draft. RawPayload may be nil (stored as {}).
type SubmitInput struct {
	RestaurantID             uuid.UUID
	Kind                     domain.DraftKind
	Source                   domain.ContentSource
	SourceRef                *string
	SourceURL                *string
	RawPayload               json.RawMessage
	SuggestedTitle           string
	SuggestedTitleI18n       domain.I18n
	SuggestedDescription     string
	SuggestedDescriptionI18n domain.I18n
	SuggestedStartsAt        *time.Time
	SuggestedEndsAt          *time.Time
	SuggestedVenue           *string
	SuggestedTerms           *string
}

// ApproveResult reports which entity an approval created (exactly one is set).
type ApproveResult struct {
	Draft   *domain.ContentDraft
	EventID *uuid.UUID
	PromoID *uuid.UUID
}

type facade struct {
	drafts domain.ContentDraftRepository
	events domain.EventRepository
	promos domain.PromoRepository
	perms  permissionChecker
	tx     domain.TxManager
	clock  func() time.Time
}

// NewFacade constructs the content-draft Facade.
func NewFacade(
	drafts domain.ContentDraftRepository,
	events domain.EventRepository,
	promos domain.PromoRepository,
	perms permissionChecker,
	tx domain.TxManager,
) Facade {
	return &facade{drafts: drafts, events: events, promos: promos, perms: perms, tx: tx, clock: time.Now}
}

func (f *facade) Submit(ctx context.Context, in SubmitInput) (*domain.ContentDraft, error) {
	if !in.Kind.Valid() {
		return nil, fmt.Errorf("%w: unknown draft kind %q", domain.ErrValidation, in.Kind)
	}
	if !in.Source.Valid() {
		return nil, fmt.Errorf("%w: unknown content source %q", domain.ErrValidation, in.Source)
	}
	payload := in.RawPayload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	d := &domain.ContentDraft{
		RestaurantID:             in.RestaurantID,
		Kind:                     in.Kind,
		Source:                   in.Source,
		SourceRef:                in.SourceRef,
		SourceURL:                in.SourceURL,
		RawPayload:               payload,
		SuggestedTitle:           strings.TrimSpace(in.SuggestedTitle),
		SuggestedTitleI18n:       in.SuggestedTitleI18n,
		SuggestedDescription:     in.SuggestedDescription,
		SuggestedDescriptionI18n: in.SuggestedDescriptionI18n,
		SuggestedStartsAt:        in.SuggestedStartsAt,
		SuggestedEndsAt:          in.SuggestedEndsAt,
		SuggestedVenue:           in.SuggestedVenue,
		SuggestedTerms:           in.SuggestedTerms,
		Status:                   domain.DraftPendingReview,
	}
	if err := f.drafts.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (f *facade) ListPending(ctx context.Context, actor Actor, restaurantID uuid.UUID, page, perPage int) ([]domain.ContentDraft, int, error) {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return nil, 0, err
	}
	return f.drafts.ListPendingByRestaurant(ctx, restaurantID, page, perPage)
}

func (f *facade) Approve(ctx context.Context, actor Actor, draftID uuid.UUID) (*ApproveResult, error) {
	d, err := f.drafts.GetByID(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, d.RestaurantID); err != nil {
		return nil, err
	}
	if d.Status != domain.DraftPendingReview {
		return nil, fmt.Errorf("%w: draft is not pending review (status=%s)", domain.ErrInvalidStatus, d.Status)
	}
	if err := validateForApproval(d); err != nil {
		return nil, err
	}

	now := f.clock()
	res := &ApproveResult{Draft: d}

	// Create the real entity and flip the draft in ONE transaction: if the
	// MarkApproved CAS loses to a concurrent approval, the whole tx rolls back
	// and no orphan Event/Promo is left behind. The draft's own
	// pending_review precondition is the single arbiter of "approved once".
	err = f.tx.WithinTx(ctx, func(ctx context.Context) error {
		switch d.Kind {
		case domain.DraftKindEvent:
			e := &domain.Event{
				RestaurantID:    d.RestaurantID,
				Title:           d.SuggestedTitle,
				TitleI18n:       d.SuggestedTitleI18n,
				Description:     d.SuggestedDescription,
				DescriptionI18n: d.SuggestedDescriptionI18n,
				StartsAt:        *d.SuggestedStartsAt,
				EndsAt:          *d.SuggestedEndsAt,
				Status:          domain.EventPublished,
			}
			if d.SuggestedVenue != nil {
				e.Venue = *d.SuggestedVenue
			}
			if err := f.events.Create(ctx, e); err != nil {
				return err
			}
			id := e.ID
			res.EventID = &id
			return f.drafts.MarkApproved(ctx, draftID, actor.UserID, now, &id, nil)
		case domain.DraftKindPromo:
			p := &domain.Promo{
				RestaurantID:    d.RestaurantID,
				Title:           d.SuggestedTitle,
				TitleI18n:       d.SuggestedTitleI18n,
				Description:     d.SuggestedDescription,
				DescriptionI18n: d.SuggestedDescriptionI18n,
				StartsAt:        *d.SuggestedStartsAt,
				EndsAt:          *d.SuggestedEndsAt,
				Status:          domain.PromoPublished,
			}
			if d.SuggestedTerms != nil {
				p.Terms = *d.SuggestedTerms
			}
			if err := f.promos.Create(ctx, p); err != nil {
				return err
			}
			id := p.ID
			res.PromoID = &id
			return f.drafts.MarkApproved(ctx, draftID, actor.UserID, now, nil, &id)
		default:
			return fmt.Errorf("%w: unknown draft kind %q", domain.ErrValidation, d.Kind)
		}
	})
	if err != nil {
		return nil, err
	}
	d.Status = domain.DraftApproved
	d.ReviewedBy = &actor.UserID
	d.ReviewedAt = &now
	d.CreatedEventID = res.EventID
	d.CreatedPromoID = res.PromoID
	return res, nil
}

func (f *facade) Reject(ctx context.Context, actor Actor, draftID uuid.UUID) (*domain.ContentDraft, error) {
	d, err := f.drafts.GetByID(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, d.RestaurantID); err != nil {
		return nil, err
	}
	if d.Status != domain.DraftPendingReview {
		return nil, fmt.Errorf("%w: draft is not pending review (status=%s)", domain.ErrInvalidStatus, d.Status)
	}
	now := f.clock()
	if err := f.drafts.MarkRejected(ctx, draftID, actor.UserID, now); err != nil {
		return nil, err
	}
	d.Status = domain.DraftRejected
	d.ReviewedBy = &actor.UserID
	d.ReviewedAt = &now
	return d, nil
}

func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to review this restaurant's content drafts", domain.ErrForbidden)
	}
	return nil
}

// validateForApproval checks a draft carries enough to become a real entity.
// Both an event and a promo need a title and a non-empty [starts, ends) window.
func validateForApproval(d *domain.ContentDraft) error {
	if strings.TrimSpace(d.SuggestedTitle) == "" {
		return fmt.Errorf("%w: draft has no suggested title to publish", domain.ErrValidation)
	}
	if d.SuggestedStartsAt == nil || d.SuggestedEndsAt == nil {
		return fmt.Errorf("%w: draft is missing a suggested start/end window", domain.ErrValidation)
	}
	if !d.SuggestedEndsAt.After(*d.SuggestedStartsAt) {
		return fmt.Errorf("%w: suggested ends_at must be after starts_at", domain.ErrValidation)
	}
	return nil
}
