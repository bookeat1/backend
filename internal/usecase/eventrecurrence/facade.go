// Package eventrecurrence is the application logic for recurring events: the
// admin CRUD over the RULES ("every Wednesday at 19:00") and the background
// generator that materialises those rules into real rows of the `events` table.
//
// Authorization is deliberately identical to usecase/events — every mutation
// requires PermRestaurantManage at the rule's OWN restaurant, or a superadmin.
// A rule is an event factory, so anyone who may create the events must be
// exactly the set of people who may create the rule, no more and no less.
package eventrecurrence

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. A global superadmin (domain.RoleAdmin)
// bypasses restaurant scoping; anyone else is authorized by
// PermRestaurantManage against the rule's own restaurant.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant",
// per the domain RBAC matrix. Bound to restaurants.ManagerUseCase in bootstrap.
// It is unaware of the global superadmin — this package checks RoleAdmin FIRST,
// the same contract every other HasPermission call site keeps.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// Facade exposes the admin CRUD over recurrence rules.
type Facade interface {
	Create(ctx context.Context, actor Actor, in Input) (*domain.EventRecurrence, error)
	Update(ctx context.Context, actor Actor, id uuid.UUID, in Input) (*domain.EventRecurrence, error)
	Get(ctx context.Context, actor Actor, id uuid.UUID) (*domain.EventRecurrence, error)
	List(ctx context.Context, actor Actor, restaurantID uuid.UUID, page, perPage int) ([]domain.EventRecurrence, int, error)
	// SetActive stops (or resumes) FUTURE generation. It never deletes an
	// occurrence that already exists — including the ones still ahead in the
	// window: those were published to guests and may already have tickets sold
	// against them, so withdrawing them is a per-event decision the venue makes
	// in the event editor, not a side effect of switching a rule off.
	SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) error
}

// Input carries a rule's fields. It is a full replace on update, like
// usecase/events.UpdateInput.
type Input struct {
	// RestaurantID is read on CREATE only; on update the rule keeps its venue.
	RestaurantID uuid.UUID

	Title            string
	TitleI18n        domain.I18n
	Description      string
	DescriptionI18n  domain.I18n
	Venue            string
	CoverImageURL    *string
	Tags             []string
	OccurrenceStatus domain.EventStatus
	Ticketed         bool
	TicketPriceMinor *int64
	Capacity         *int
	// RefundPolicy is the venue's own ticket-refund rules, copied onto every
	// generated occurrence. The zero value is the conservative platform default
	// (not refundable), same as usecase/events.CreateInput.
	RefundPolicy domain.TicketRefundPolicy

	Frequency       domain.RecurrenceFrequency
	Weekdays        []domain.ISOWeekday
	MonthDay        *int
	StartMinutes    int
	DurationMinutes int
	Timezone        string
	StartsOn        domain.CalendarDate
	UntilDate       *domain.CalendarDate
	IsActive        bool
}

type facade struct {
	repo  domain.EventRecurrenceRepository
	perms permissionChecker
}

// NewFacade constructs the recurrence Facade.
func NewFacade(repo domain.EventRecurrenceRepository, perms permissionChecker) Facade {
	return &facade{repo: repo, perms: perms}
}

func (f *facade) Create(ctx context.Context, actor Actor, in Input) (*domain.EventRecurrence, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	rec := &domain.EventRecurrence{RestaurantID: in.RestaurantID}
	apply(rec, in)
	if err := validate(rec); err != nil {
		return nil, err
	}
	if err := f.repo.Create(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Update replaces the rule's template and schedule. What it deliberately does
// NOT do is rewrite the occurrences already generated from the old version:
// they are live events a guest may have bookmarked or bought a ticket for, and
// silently retitling or moving them from a rule edit would be an invisible
// change to a published promise. The new template applies to occurrences
// generated from now on; the cabinet fixes an individual date in the event
// editor, as it always did.
func (f *facade) Update(ctx context.Context, actor Actor, id uuid.UUID, in Input) (*domain.EventRecurrence, error) {
	rec, err := f.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, rec.RestaurantID); err != nil {
		return nil, err
	}
	apply(rec, in)
	if err := validate(rec); err != nil {
		return nil, err
	}
	if err := f.repo.Update(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (f *facade) Get(ctx context.Context, actor Actor, id uuid.UUID) (*domain.EventRecurrence, error) {
	rec, err := f.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, rec.RestaurantID); err != nil {
		return nil, err
	}
	return rec, nil
}

func (f *facade) List(ctx context.Context, actor Actor, restaurantID uuid.UUID, page, perPage int) ([]domain.EventRecurrence, int, error) {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return nil, 0, err
	}
	return f.repo.ListByRestaurant(ctx, restaurantID, page, perPage)
}

func (f *facade) SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) error {
	rec, err := f.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := f.authorize(ctx, actor, rec.RestaurantID); err != nil {
		return err
	}
	return f.repo.SetActive(ctx, id, active)
}

// authorize enforces PermRestaurantManage at restaurantID; a superadmin
// bypasses the check entirely (same contract as usecase/events).
func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's recurring events", domain.ErrForbidden)
	}
	return nil
}

func apply(rec *domain.EventRecurrence, in Input) {
	status := in.OccurrenceStatus
	if status == "" {
		status = domain.EventDraft
	}
	rec.Title = strings.TrimSpace(in.Title)
	rec.TitleI18n = in.TitleI18n
	rec.Description = in.Description
	rec.DescriptionI18n = in.DescriptionI18n
	rec.Venue = in.Venue
	rec.CoverImageURL = in.CoverImageURL
	rec.Tags = normalizeTags(in.Tags)
	rec.OccurrenceStatus = status
	rec.Ticketed = in.Ticketed
	rec.TicketPriceMinor = in.TicketPriceMinor
	rec.Capacity = in.Capacity
	rec.TicketsRefundable = in.RefundPolicy.Refundable
	rec.TicketRefundCutoffMinutes = in.RefundPolicy.CutoffMinutes
	rec.Frequency = in.Frequency
	rec.Weekdays = normalizeWeekdays(in.Frequency, in.Weekdays)
	rec.MonthDay = in.MonthDay
	rec.StartMinutes = in.StartMinutes
	rec.DurationMinutes = in.DurationMinutes
	rec.Timezone = strings.TrimSpace(in.Timezone)
	rec.StartsOn = in.StartsOn
	rec.UntilDate = in.UntilDate
	rec.IsActive = in.IsActive
}

// Bounds mirrored from the DB CHECKs of migration 0074, answered here with
// domain.ErrValidation (422) instead of a 500 from a constraint violation. The
// duration ceiling (a week) is the same defensive spirit as the refund cutoff
// cap: it catches "minutes entered as seconds" before it generates a series of
// month-long events.
const (
	maxDurationMinutes     = 7 * 24 * 60
	maxTags                = 10
	maxRefundCutoffMinutes = 30 * 24 * 60
)

func validate(rec *domain.EventRecurrence) error {
	if rec.Title == "" {
		return fmt.Errorf("%w: title is required", domain.ErrValidation)
	}
	if !rec.OccurrenceStatus.Valid() {
		return fmt.Errorf("%w: unknown event status %q", domain.ErrValidation, rec.OccurrenceStatus)
	}
	if !rec.Frequency.Valid() {
		return fmt.Errorf("%w: unknown frequency %q; use daily, weekly or monthly", domain.ErrValidation, rec.Frequency)
	}
	if rec.Frequency == domain.RecurrenceWeekly && len(rec.Weekdays) == 0 {
		return fmt.Errorf("%w: a weekly rule needs at least one weekday (1=Mon … 7=Sun)", domain.ErrValidation)
	}
	for _, w := range rec.Weekdays {
		if !w.Valid() {
			return fmt.Errorf("%w: weekday %d is out of range; use 1=Mon … 7=Sun", domain.ErrValidation, w)
		}
	}
	if rec.Frequency == domain.RecurrenceMonthly {
		if rec.MonthDay == nil || *rec.MonthDay < 1 || *rec.MonthDay > 31 {
			return fmt.Errorf("%w: a monthly rule needs month_day between 1 and 31", domain.ErrValidation)
		}
	}
	if rec.StartMinutes < 0 || rec.StartMinutes >= 24*60 {
		return fmt.Errorf("%w: start time must be within the day", domain.ErrValidation)
	}
	if rec.DurationMinutes <= 0 || rec.DurationMinutes > maxDurationMinutes {
		return fmt.Errorf("%w: duration must be between 1 and %d minutes", domain.ErrValidation, maxDurationMinutes)
	}
	// A zone is validated by the SAME rule the venue's own zone is written
	// under: no empty string (that is UTC to time.LoadLocation), no "Local",
	// no fixed-offset abbreviation. Empty here means "no override" and is fine.
	if rec.Timezone != "" {
		tz, err := domain.NormalizeVenueTimezone(rec.Timezone)
		if err != nil {
			return err
		}
		rec.Timezone = tz
	}
	if (rec.StartsOn == domain.CalendarDate{}) {
		return fmt.Errorf("%w: starts_on is required", domain.ErrValidation)
	}
	if rec.UntilDate != nil && dateBefore(*rec.UntilDate, rec.StartsOn) {
		return fmt.Errorf("%w: until_date must not be before starts_on", domain.ErrValidation)
	}
	if rec.TicketPriceMinor != nil && *rec.TicketPriceMinor < 0 {
		return fmt.Errorf("%w: ticket_price_minor must be >= 0", domain.ErrValidation)
	}
	if rec.Capacity != nil && *rec.Capacity < 0 {
		return fmt.Errorf("%w: capacity must be >= 0", domain.ErrValidation)
	}
	if rec.TicketRefundCutoffMinutes < 0 || rec.TicketRefundCutoffMinutes > maxRefundCutoffMinutes {
		return fmt.Errorf("%w: ticket refund cutoff must be between 0 and %d minutes",
			domain.ErrValidation, maxRefundCutoffMinutes)
	}
	return nil
}

func dateBefore(a, b domain.CalendarDate) bool {
	if a.Year != b.Year {
		return a.Year < b.Year
	}
	if a.Month != b.Month {
		return a.Month < b.Month
	}
	return a.Day < b.Day
}

// normalizeWeekdays drops duplicates and sorts, and returns an empty list for
// any frequency that has no weekdays: storing "Wednesdays" on a DAILY rule
// would be a lie the next reader has to guess about.
func normalizeWeekdays(freq domain.RecurrenceFrequency, ws []domain.ISOWeekday) []domain.ISOWeekday {
	if freq != domain.RecurrenceWeekly {
		return []domain.ISOWeekday{}
	}
	var seen [8]bool
	out := make([]domain.ISOWeekday, 0, len(ws))
	for d := domain.ISOWeekday(1); d <= 7; d++ {
		for _, w := range ws {
			if w == d && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	// Out-of-range values are kept so validate() can refuse them by name
	// instead of silently dropping a weekday the caller believes it set.
	for _, w := range ws {
		if !w.Valid() {
			out = append(out, w)
		}
	}
	return out
}

// normalizeTags trims, drops blanks and caps the list — same rule as
// usecase/events.normalizeTags, so a chip set defined on a rule and one typed
// on a single event behave identically.
func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	return out
}
