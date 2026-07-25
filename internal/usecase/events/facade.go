// Package events is the application logic for restaurant events (Ф2). It owns
// the admin CRUD authorization (every mutation requires PermRestaurantManage at
// the event's OWN restaurant, or a superadmin) and the public read rules (only
// published, not-yet-ended events are listed). It reuses the shared domain RBAC
// matrix — it invents no permission — resolved per (actor, restaurant) exactly
// like usecase/admin and usecase/reviews do, so an owner of venue A can never
// touch venue B.
package events

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for the staff-side (admin CRUD) actions. A
// global superadmin (Role == domain.RoleAdmin) bypasses restaurant scoping;
// anyone else is authorized by PermRestaurantManage against the event's own
// restaurant.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant",
// per the domain RBAC matrix. Bound to restaurants.ManagerUseCase in bootstrap.
// It is unaware of the global superadmin — this package checks RoleAdmin FIRST
// and bypasses it, the same contract every other HasPermission call site keeps.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// feedModerator pulls an item off the main-screen feed when its content
// changes. Minimal local port (bound to the feed repository in bootstrap): the
// events usecase must not know the whole FeedRepository, only this one effect.
type feedModerator interface {
	DemoteAfterContentEdit(ctx context.Context, kind domain.FeedItemKind, itemID uuid.UUID) error
}

// Facade exposes admin CRUD and public read operations for events.
type Facade interface {
	Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Event, error)
	Update(ctx context.Context, actor Actor, eventID uuid.UUID, in UpdateInput) (*domain.Event, error)
	Delete(ctx context.Context, actor Actor, eventID uuid.UUID) error
	// GetAdmin returns any of a restaurant's events (any status) for the
	// cabinet. Requires PermRestaurantManage at the event's own restaurant.
	GetAdmin(ctx context.Context, actor Actor, eventID uuid.UUID) (*domain.Event, error)
	// ListAdmin returns a restaurant's events (optionally status-filtered) for
	// the cabinet, paginated. Requires PermRestaurantManage at restaurantID.
	ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error)
	// SetRefundPolicy sets the venue's OWN ticket-refund rules for one event
	// without going through the full-replace Update. Requires
	// PermRestaurantManage at the event's own restaurant. A change here applies
	// only to tickets sold AFTER it — every existing ticket keeps the snapshot
	// it was bought under (domain.EventTicket.RefundPolicy).
	SetRefundPolicy(ctx context.Context, actor Actor, eventID uuid.UUID, policy domain.TicketRefundPolicy) (*domain.Event, error)

	// ListPublic returns a restaurant's published, not-yet-ended events,
	// paginated. No authorization.
	ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Event, int, error)
	// GetPublic returns one published event that belongs to restaurantID.
	// A draft/hidden event, or one of another restaurant, is ErrNotFound.
	GetPublic(ctx context.Context, restaurantID, eventID uuid.UUID) (*domain.Event, error)
	// ListPublicUpcoming is the cross-venue guest listing (Explore screen):
	// published, not-yet-ended events at active venues, soonest first,
	// narrowed by f and paginated. No authorization. An inverted date range is
	// ErrValidation, never a silently empty page.
	ListPublicUpcoming(ctx context.Context, f domain.PublicEventFilter) ([]domain.EventListItem, int, error)
}

// CreateInput carries a new event's fields. Status defaults to draft when empty.
type CreateInput struct {
	RestaurantID     uuid.UUID
	Title            string
	TitleI18n        domain.I18n
	Description      string
	DescriptionI18n  domain.I18n
	StartsAt         time.Time
	EndsAt           time.Time
	Venue            string
	CoverImageURL    *string
	Status           domain.EventStatus
	Ticketed         bool
	TicketPriceMinor *int64
	Capacity         *int
	// RefundPolicy is the venue's own ticket-refund rules for this event. The
	// zero value is the conservative platform default (not refundable) — same
	// as the migration 0047 backfill, so an old client that does not send the
	// fields never accidentally opens refunds.
	RefundPolicy domain.TicketRefundPolicy
}

// UpdateInput carries an event's mutable fields (full replace). Status must be
// a valid EventStatus.
type UpdateInput struct {
	Title            string
	TitleI18n        domain.I18n
	Description      string
	DescriptionI18n  domain.I18n
	StartsAt         time.Time
	EndsAt           time.Time
	Venue            string
	CoverImageURL    *string
	Status           domain.EventStatus
	Ticketed         bool
	TicketPriceMinor *int64
	Capacity         *int
	// RefundPolicy replaces the event's refund rules. Unlike every other field
	// here, it is OPTIONAL: nil means "leave the rules as they are". Update is a
	// full replace, and a cabinet build that predates this feature sends the
	// whole event without these fields — treating that as "set false/0" would
	// silently switch refunds off for future buyers every time someone edited a
	// title. Money settings do not get turned off as a side effect of an
	// unrelated edit.
	RefundPolicy *domain.TicketRefundPolicy
}

type facade struct {
	repo  domain.EventRepository
	perms permissionChecker
	feed  feedModerator
	clock func() time.Time
}

// NewFacade constructs the events Facade.
func NewFacade(repo domain.EventRepository, perms permissionChecker, feed feedModerator) Facade {
	return &facade{repo: repo, perms: perms, feed: feed, clock: time.Now}
}

func (f *facade) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Event, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = domain.EventDraft
	}
	e := &domain.Event{
		RestaurantID:     in.RestaurantID,
		Title:            strings.TrimSpace(in.Title),
		TitleI18n:        in.TitleI18n,
		Description:      in.Description,
		DescriptionI18n:  in.DescriptionI18n,
		StartsAt:         in.StartsAt,
		EndsAt:           in.EndsAt,
		Venue:            in.Venue,
		CoverImageURL:    in.CoverImageURL,
		Status:           status,
		Ticketed:         in.Ticketed,
		TicketPriceMinor: in.TicketPriceMinor,
		Capacity:         in.Capacity,

		TicketsRefundable:         in.RefundPolicy.Refundable,
		TicketRefundCutoffMinutes: in.RefundPolicy.CutoffMinutes,
	}
	if err := validateEvent(e); err != nil {
		return nil, err
	}
	if err := f.repo.Create(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (f *facade) Update(ctx context.Context, actor Actor, eventID uuid.UUID, in UpdateInput) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return nil, err
	}
	// Decided before anything is overwritten: only a change to what a moderator
	// actually read counts as an edit. Publishing or hiding an event travels
	// through this same method, and re-queueing a venue for that would punish
	// them for using their own visibility switch. Same rule as usecase/promos.
	contentChanged := eventContentChanged(*e, in)

	e.Title = strings.TrimSpace(in.Title)
	e.TitleI18n = in.TitleI18n
	e.Description = in.Description
	e.DescriptionI18n = in.DescriptionI18n
	e.StartsAt = in.StartsAt
	e.EndsAt = in.EndsAt
	e.Venue = in.Venue
	e.CoverImageURL = in.CoverImageURL
	e.Status = in.Status
	e.Ticketed = in.Ticketed
	e.TicketPriceMinor = in.TicketPriceMinor
	e.Capacity = in.Capacity
	if in.RefundPolicy != nil {
		e.TicketsRefundable = in.RefundPolicy.Refundable
		e.TicketRefundCutoffMinutes = in.RefundPolicy.CutoffMinutes
	}
	if err := validateEvent(e); err != nil {
		return nil, err
	}
	// Demote BEFORE writing the new content: the platform approved specific
	// words and dates, so changing them invalidates the decision. This ordering
	// makes both failure modes safe — a failed edit after a successful demotion
	// only costs the venue a re-review, while the reverse order could leave
	// unreviewed text live on the main screen. See usecase/promos.Update.
	if contentChanged {
		if err := f.feed.DemoteAfterContentEdit(ctx, domain.FeedItemEvent, eventID); err != nil {
			return nil, err
		}
	}
	if err := f.repo.Update(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// SetRefundPolicy is the narrow "just the refund rules" mutation the cabinet
// uses, so a venue does not have to re-send the whole event (and risk clobbering
// a field it did not intend to touch) to change one setting. Authorization is
// the same resolve-the-event-then-check-its-restaurant gate every other event
// mutation uses.
func (f *facade) SetRefundPolicy(ctx context.Context, actor Actor, eventID uuid.UUID, policy domain.TicketRefundPolicy) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return nil, err
	}
	e.TicketsRefundable = policy.Refundable
	e.TicketRefundCutoffMinutes = policy.CutoffMinutes
	if err := validateRefundPolicy(e.TicketRefundPolicy()); err != nil {
		return nil, err
	}
	if err := f.repo.Update(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (f *facade) Delete(ctx context.Context, actor Actor, eventID uuid.UUID) error {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return err
	}
	return f.repo.Delete(ctx, eventID)
}

func (f *facade) GetAdmin(ctx context.Context, actor Actor, eventID uuid.UUID) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return nil, err
	}
	return e, nil
}

func (f *facade) ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error) {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return nil, 0, err
	}
	return f.repo.ListByRestaurant(ctx, restaurantID, statuses, page, perPage)
}

func (f *facade) ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Event, int, error) {
	return f.repo.ListPublishedUpcoming(ctx, restaurantID, f.clock(), page, perPage)
}

// ListPublicUpcoming reads the cross-venue listing. The "what a guest may see"
// rule (published, not yet ended, active venue) lives in the repository query
// so it cannot be paginated or filtered away here; this method only validates
// the caller's filters and supplies the clock, exactly like ListPublic.
func (f *facade) ListPublicUpcoming(ctx context.Context, flt domain.PublicEventFilter) ([]domain.EventListItem, int, error) {
	// An inverted range can only ever return nothing, so it is a mistake worth
	// naming rather than an empty page the client has to explain to itself.
	if flt.From != nil && flt.To != nil && flt.To.Before(*flt.From) {
		return nil, 0, fmt.Errorf("%w: to must not be before from", domain.ErrValidation)
	}
	return f.repo.ListPublicUpcoming(ctx, flt, f.clock())
}

func (f *facade) GetPublic(ctx context.Context, restaurantID, eventID uuid.UUID) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// Never leak a draft/hidden event or one belonging to another restaurant
	// through the public endpoint: it simply does not exist to a guest.
	if e.RestaurantID != restaurantID || e.Status != domain.EventPublished {
		return nil, fmt.Errorf("get public event: %w", domain.ErrNotFound)
	}
	return e, nil
}

// authorize enforces PermRestaurantManage at restaurantID; a superadmin
// bypasses the check entirely (same contract as usecase/admin).
func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's events", domain.ErrForbidden)
	}
	return nil
}

// validateEvent enforces the invariants the DB CHECKs also guard, but with a
// domain.ErrValidation (422) instead of a 500 from a constraint violation.
func validateEvent(e *domain.Event) error {
	if e.Title == "" {
		return fmt.Errorf("%w: title is required", domain.ErrValidation)
	}
	if !e.Status.Valid() {
		return fmt.Errorf("%w: unknown event status %q", domain.ErrValidation, e.Status)
	}
	if e.StartsAt.IsZero() || e.EndsAt.IsZero() {
		return fmt.Errorf("%w: starts_at and ends_at are required", domain.ErrValidation)
	}
	if !e.EndsAt.After(e.StartsAt) {
		return fmt.Errorf("%w: ends_at must be after starts_at", domain.ErrValidation)
	}
	if e.TicketPriceMinor != nil && *e.TicketPriceMinor < 0 {
		return fmt.Errorf("%w: ticket_price_minor must be >= 0", domain.ErrValidation)
	}
	if e.Capacity != nil && *e.Capacity < 0 {
		return fmt.Errorf("%w: capacity must be >= 0", domain.ErrValidation)
	}
	return validateRefundPolicy(e.TicketRefundPolicy())
}

// Bounds for the refund cutoff, in the same spirit as usecase/admin's
// free-cancellation window: a non-negative number of minutes, capped at 30 days
// so a typo (minutes entered as seconds, say) cannot silently make every ticket
// unrefundable in practice.
const (
	minRefundCutoffMinutes = 0
	maxRefundCutoffMinutes = 30 * 24 * 60
)

// validateRefundPolicy mirrors the DB CHECK from migration 0048 but answers
// with domain.ErrValidation (422) instead of a 500 from a constraint violation.
func validateRefundPolicy(p domain.TicketRefundPolicy) error {
	if p.CutoffMinutes < minRefundCutoffMinutes || p.CutoffMinutes > maxRefundCutoffMinutes {
		return fmt.Errorf("%w: ticket refund cutoff must be between %d and %d minutes",
			domain.ErrValidation, minRefundCutoffMinutes, maxRefundCutoffMinutes)
	}
	return nil
}

// eventContentChanged reports whether this update touches anything shown on the
// feed card or reviewed by a moderator: the words, the dates, the venue line,
// the cover, or the ticketing terms a guest sees before paying. Status,
// deliberately, is not one of them.
func eventContentChanged(cur domain.Event, in UpdateInput) bool {
	switch {
	case strings.TrimSpace(in.Title) != cur.Title,
		in.Description != cur.Description,
		in.Venue != cur.Venue,
		!in.StartsAt.Equal(cur.StartsAt),
		!in.EndsAt.Equal(cur.EndsAt),
		in.Ticketed != cur.Ticketed,
		!strPtrEqual(in.CoverImageURL, cur.CoverImageURL),
		!int64PtrEqual(in.TicketPriceMinor, cur.TicketPriceMinor),
		!intPtrEqual(in.Capacity, cur.Capacity),
		!i18nEqual(in.TitleI18n, cur.TitleI18n),
		!i18nEqual(in.DescriptionI18n, cur.DescriptionI18n):
		return true
	}
	return false
}

// i18nEqual compares localized maps by content: nil and empty read the same to a
// guest, so they must not count as an edit.
func i18nEqual(a, b domain.I18n) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
