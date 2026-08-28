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
	"errors"
	"fmt"
	"log/slog"
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

// feedModerator is the events usecase's minimal slice of the feed's moderation
// writes (bound to the feed repository in bootstrap): pull an item off the main
// screen when its content changes, and record the platform's approval of the
// platform's OWN item at creation. Deliberately identical to the port in
// usecase/promos — the two content types must not disagree about moderation.
type feedModerator interface {
	DemoteAfterContentEdit(ctx context.Context, kind domain.FeedItemKind, itemID uuid.UUID) error
	ApprovePlatformItem(ctx context.Context, kind domain.FeedItemKind, itemID, reviewerID uuid.UUID, at time.Time) error
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
	// ListPlatformAdmin returns the PLATFORM's own events (no host venue), any
	// status, for the platform cabinet. Authorized by
	// domain.CanManagePlatformContent, not by a per-restaurant permission —
	// there is no restaurant to check one at.
	ListPlatformAdmin(ctx context.Context, actor Actor, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error)
	// ResetSeriesContent hands the named content fields of one DATE back to
	// its series (migration 0097): the series values are copied onto the event
	// and the override markers for those fields are cleared, so the date
	// follows the series again. An empty field list resets every content
	// field. ErrValidation when the event belongs to no series. Requires
	// PermRestaurantManage at the event's own restaurant.
	ResetSeriesContent(ctx context.Context, actor Actor, eventID uuid.UUID, fields []domain.EventContentField) (*domain.Event, error)
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
	// GetPublicDetail returns ONE published, not-yet-ended event by its own id,
	// with its venue when it has one. This is the event's own page — the target
	// an action button points at when it has no external link — and the only
	// public read that can address a PLATFORM event at all, since the older
	// GetPublic is reached through a restaurant's path. No authorization.
	GetPublicDetail(ctx context.Context, eventID uuid.UUID) (*domain.EventListItem, error)
}

// CreateInput carries a new event's fields. Status defaults to draft when empty.
type CreateInput struct {
	// RestaurantID is the host venue. nil creates a PLATFORM event — one with
	// no venue at all — which only domain.CanManagePlatformContent roles may do.
	RestaurantID     *uuid.UUID
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
	// City overrides the city the event is shown in. nil (or blank) is the
	// normal case and means "wherever the host venue is" — resolved on every
	// read, so it follows the venue if the venue ever moves. A value is
	// resolved through the city dictionary and stored in the dictionary's own
	// spelling; an unknown or hidden city is ErrValidation, because this one is
	// typed by an editor, not imported from the old system.
	City *string
	// Tags are the «Афиша» chips ("Бранч", "Живая музыка", ...). Blank entries
	// are dropped and the list is capped (see normalizeTags); empty means the
	// card draws no chips.
	Tags []string
	// Images — галерея события в порядке редактора, без обложки. Пустой список
	// означает «галереи нет»; обложка задаётся отдельно (CoverImageURL).
	Images []string
	// RefundPolicy is the venue's own ticket-refund rules for this event. The
	// zero value is the conservative platform default (not refundable) — same
	// as the migration 0047 backfill, so an old client that does not send the
	// fields never accidentally opens refunds.
	RefundPolicy domain.TicketRefundPolicy
	// Action is the optional call-to-action button. nil = no button. A non-nil
	// Action with a nil URL is a button onto the event's OWN page; with a URL it
	// is an external link, validated before it is stored.
	Action *domain.EventAction
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
	// City replaces the event's city override, full replace like the rest of
	// this struct: nil or blank CLEARS it, and the event goes back to following
	// its venue. That is safe for an older cabinet build that does not send the
	// field — clearing the override restores the venue-derived city, which is
	// exactly what every event did before migration 0084. (Contrast
	// RefundPolicy below, which is optional precisely because its default is
	// NOT the previous behaviour.)
	City *string
	// Tags replaces the event's «Афиша» chips (full replace, like the rest of
	// this struct). Blank entries are dropped and the list is capped; an empty
	// or absent list clears the chips.
	Tags []string
	// Images заменяет галерею целиком, как и остальные поля этой структуры:
	// пустой или отсутствующий список её очищает.
	Images []string
	// RefundPolicy replaces the event's refund rules. Unlike every other field
	// here, it is OPTIONAL: nil means "leave the rules as they are". Update is a
	// full replace, and a cabinet build that predates this feature sends the
	// whole event without these fields — treating that as "set false/0" would
	// silently switch refunds off for future buyers every time someone edited a
	// title. Money settings do not get turned off as a side effect of an
	// unrelated edit.
	RefundPolicy *domain.TicketRefundPolicy
	// Action replaces the call-to-action button, full replace like everything
	// else here except RefundPolicy: absent (nil) REMOVES the button. That is
	// the safe default for an older cabinet build — it removes a button nobody
	// could have added from that build, rather than stranding one it cannot see.
	Action *domain.EventAction
}

// occurrenceSkipRecorder tombstones one slot of a recurrence rule so the
// generator never fills it again. Minimal local port (bound to the
// event-recurrence repository in bootstrap): this usecase must not know the
// whole EventRecurrenceRepository, only this one effect — same shape as
// feedModerator above.
type occurrenceSkipRecorder interface {
	RecordSkip(ctx context.Context, recurrenceID uuid.UUID, slot time.Time) error
}

// seriesContentReader is the minimal slice of the recurrence repository this
// package needs: "what does the SERIES this date belongs to say?". Declared
// here and bound in bootstrap (to the event-recurrence repository), so the
// events usecase never depends on usecase/eventrecurrence — the same one-effect
// port shape as feedModerator and occurrenceSkipRecorder above.
//
// It answers two questions and only those two: which fields of an edited date
// now differ from its series (and are therefore this date's own — see
// domain.EventContentDiff), and what content to put back when an override is
// reset.
type seriesContentReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.EventRecurrence, error)
}

// cityResolver is the minimal slice of usecase/cities this package needs:
// "which dictionary entry does this written spelling mean". Declared here and
// bound in bootstrap/deps.go, so the events usecase never depends on the
// dictionary package itself — the same seam usecase/restaurants uses.
//
// A nil *domain.CityEntry with a nil error means "no such city". On the READ
// path that is a normal answer for a filter value typed by a client; on the
// WRITE path it is a validation error.
type cityResolver interface {
	Resolve(ctx context.Context, raw string) (*domain.CityEntry, error)
}

type facade struct {
	repo  domain.EventRepository
	perms permissionChecker
	feed  feedModerator
	skips occurrenceSkipRecorder
	// cities is the optional city-dictionary resolver (see WithCityResolver).
	// Nil unless wired; ?city= then behaves exactly as it did before migration
	// 0084 — the raw string is compared to the stored spelling, and an event's
	// own city override is validated against the two legacy constants.
	cities cityResolver
	// series reads the rule a generated occurrence belongs to (see
	// WithSeriesContent). Nil unless wired.
	series seriesContentReader
	clock  func() time.Time
}

// Option tunes the facade. Variadic options keep every existing positional
// caller (and test) compiling unchanged — the same backward-compatible pattern
// the bookings usecase uses.
type Option func(*facade)

// WithOccurrenceSkips wires the tombstone writer that keeps a DELETED (or
// moved) recurring occurrence from being regenerated by the next worker pass.
// bootstrap always supplies it; without it a hard-deleted occurrence would
// simply reappear, so the only callers that may omit it are tests that never
// touch a generated event.
func WithOccurrenceSkips(r occurrenceSkipRecorder) Option {
	return func(f *facade) { f.skips = r }
}

// WithSeriesContent wires the reader of the recurrence rules, which is what
// lets a single DATE of a series be told apart from the series itself
// (migration 0097).
//
// Without it the facade still works exactly as it did before 0097, with one
// documented consequence: editing one date of a series no longer records WHICH
// fields that date now owns, so the next edit of the series would overwrite
// them. The existing override markers are never dropped in that case — a
// missing dependency must not silently un-own a venue's poster. bootstrap
// always supplies it; only tests that never touch a generated occurrence may
// omit it.
func WithSeriesContent(r seriesContentReader) Option {
	return func(f *facade) { f.series = r }
}

// WithCityResolver teaches the events usecase the city dictionary (migration
// 0081): ?city= starts accepting a city CODE (?city=almaty) and any registered
// spelling — a historical name, another case — next to the Russian name it has
// always accepted, and an event's own city override is validated against the
// dictionary instead of two constants compiled into the binary.
func WithCityResolver(r cityResolver) Option {
	return func(f *facade) { f.cities = r }
}

// NewFacade constructs the events Facade.
func NewFacade(repo domain.EventRepository, perms permissionChecker, feed feedModerator, opts ...Option) Facade {
	f := &facade{repo: repo, perms: perms, feed: feed, clock: time.Now}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// skipSlot tombstones a slot the generator must never refill. Called BEFORE the
// write that frees the slot (a delete, or a move to another time), and
// deliberately NOT inside a transaction with it: a tombstone written for a slot
// whose delete then failed is harmless — the event still occupies the slot, so
// nothing would have been generated there anyway — while the reverse order
// leaves a real window in which the generator recreates the event the venue
// just removed.
func (f *facade) skipSlot(ctx context.Context, e *domain.Event, slot time.Time) error {
	if e.RecurrenceID == nil || f.skips == nil {
		return nil
	}
	return f.skips.RecordSkip(ctx, *e.RecurrenceID, slot)
}

// maxGalleryImages — потолок на галерею одной карточки. Двадцать фотографий
// гость всё равно не пролистает, а сотня превратит запись в длинную транзакцию.
const maxGalleryImages = 20

func (f *facade) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Event, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = domain.EventDraft
	}
	city, err := f.cityOverride(ctx, in.City)
	if err != nil {
		return nil, err
	}
	action := in.Action
	if err := domain.ValidateEventAction(action); err != nil {
		return nil, err
	}
	e := &domain.Event{
		RestaurantID:     in.RestaurantID,
		Action:           action,
		Title:            strings.TrimSpace(in.Title),
		TitleI18n:        in.TitleI18n,
		Description:      in.Description,
		DescriptionI18n:  in.DescriptionI18n,
		StartsAt:         in.StartsAt,
		EndsAt:           in.EndsAt,
		Venue:            in.Venue,
		City:             city,
		CoverImageURL:    in.CoverImageURL,
		Status:           status,
		Ticketed:         in.Ticketed,
		TicketPriceMinor: in.TicketPriceMinor,
		Capacity:         in.Capacity,
		Tags:             normalizeTags(in.Tags),

		TicketsRefundable:         in.RefundPolicy.Refundable,
		TicketRefundCutoffMinutes: in.RefundPolicy.CutoffMinutes,
	}
	if err := validateEvent(e); err != nil {
		return nil, err
	}
	if err := f.repo.Create(ctx, e); err != nil {
		return nil, err
	}
	// Галерея пишется отдельной таблицей и ТОЛЬКО после того, как событие
	// получило id. Ошибка здесь не откатывает событие: оно уже существует и
	// показывается с обложкой, а фотографии редактор допишет повторным
	// сохранением — терять созданное событие из-за картинки хуже.
	if err := f.repo.ReplaceImages(ctx, e.ID, normalizeImages(in.Images)); err != nil {
		return nil, err
	}
	e.Images = normalizeImages(in.Images)
	// PLATFORM content (no venue) skips the moderation round trip — see the
	// long note in usecase/promos.Create; the rule is one rule and both content
	// types follow it. The approval is written down with its reviewer, and the
	// venue path is not touched.
	if e.RestaurantID == nil {
		if err := f.feed.ApprovePlatformItem(ctx, domain.FeedItemEvent, e.ID, actor.UserID, f.clock()); err != nil {
			return nil, err
		}
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
	city, err := f.cityOverride(ctx, in.City)
	if err != nil {
		return nil, err
	}
	// A city override is moderated content too: it decides WHICH city's main
	// screen the card can reach, so changing it hands the same approved words to
	// a different audience than the platform said yes to. Compared on the
	// RESOLVED value, not the raw input — otherwise re-saving «almaty» over a
	// stored «Алматы» would read as an edit and cost the venue a re-review.
	// The button is moderated content as much as the words are: it is what a
	// guest taps, and repointing it at another site after approval is exactly
	// the substitution moderation exists to catch.
	contentChanged := eventContentChanged(*e, in) || !cityPtrEqual(city, e.City) ||
		!actionEqual(in.Action, e.Action)
	// Moving a generated occurrence to another time frees its ORIGINAL slot, so
	// that slot needs the same tombstone a delete leaves — otherwise the next
	// pass fills the old date back in and the venue ends up with both.
	if !in.StartsAt.Equal(e.StartsAt) {
		if err := f.skipSlot(ctx, e, e.StartsAt); err != nil {
			return nil, err
		}
	}

	e.Title = strings.TrimSpace(in.Title)
	e.TitleI18n = in.TitleI18n
	e.Description = in.Description
	e.DescriptionI18n = in.DescriptionI18n
	e.StartsAt = in.StartsAt
	e.EndsAt = in.EndsAt
	e.Venue = in.Venue
	e.City = city
	e.CoverImageURL = in.CoverImageURL
	e.Status = in.Status
	e.Ticketed = in.Ticketed
	e.TicketPriceMinor = in.TicketPriceMinor
	e.Capacity = in.Capacity
	e.Tags = normalizeTags(in.Tags)
	e.Action = in.Action
	if err := domain.ValidateEventAction(e.Action); err != nil {
		return nil, err
	}
	if in.RefundPolicy != nil {
		e.TicketsRefundable = in.RefundPolicy.Refundable
		e.TicketRefundCutoffMinutes = in.RefundPolicy.CutoffMinutes
	}
	if err := validateEvent(e); err != nil {
		return nil, err
	}
	// A date of a series records WHICH content it now owns. Derived from the
	// diff against the series rather than declared by the client: the cabinet
	// sends the date as a full replace and always has, so nothing has to change
	// on the wire for an existing build — and re-typing the series text on a
	// date hands that field BACK to the series instead of freezing a copy of
	// today's wording. See domain.EventContentDiff.
	if err := f.markContentOverrides(ctx, e); err != nil {
		return nil, err
	}
	// Demote BEFORE writing the new content: the platform approved specific
	// words and dates, so changing them invalidates the decision. This ordering
	// makes both failure modes safe — a failed edit after a successful demotion
	// only costs the venue a re-review, while the reverse order could leave
	// unreviewed text live on the main screen. See usecase/promos.Update.
	//
	// PLATFORM content is exempt (domain.FeedDemotableAfterContentEdit) for the
	// reason spelled out there: there is no second party to re-review it.
	if contentChanged && domain.FeedDemotableAfterContentEdit(e.RestaurantID) {
		if err := f.feed.DemoteAfterContentEdit(ctx, domain.FeedItemEvent, eventID); err != nil {
			return nil, err
		}
	}
	if err := f.repo.Update(ctx, e); err != nil {
		return nil, err
	}
	if err := f.repo.ReplaceImages(ctx, e.ID, normalizeImages(in.Images)); err != nil {
		return nil, err
	}
	e.Images = normalizeImages(in.Images)
	return e, nil
}

// markContentOverrides recomputes e.ContentOverrides from the difference
// between this date's content and its series'.
//
// A one-off event (no rule) can own nothing — there is no series to inherit
// from — so its marker list is cleared. A date whose rule has been deleted
// (recurrence_id nulled by ON DELETE SET NULL) is left exactly as it is: it is
// an ordinary event now, and rewriting a marker list nothing reads would be
// churn. Same for a facade with no series reader wired: keep what is stored
// rather than silently un-owning a poster (see WithSeriesContent).
func (f *facade) markContentOverrides(ctx context.Context, e *domain.Event) error {
	if e.RecurrenceID == nil {
		e.ContentOverrides = nil
		return nil
	}
	if f.series == nil {
		return nil
	}
	rec, err := f.series.GetByID(ctx, *e.RecurrenceID)
	if err != nil {
		// The rule vanished between the read and here: the row is an ordinary
		// event now and the edit must not fail because of it.
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	e.ContentOverrides = domain.EventContentDiff(rec.Content(), e.Content())
	return nil
}

// ResetSeriesContent hands the named content fields of ONE date back to its
// series: the series values are copied onto the date and the override markers
// for those fields disappear, so every later edit of the series reaches this
// date again. An empty field list resets everything.
//
// It is a separate operation rather than "send the series text through Update"
// because the cabinet must be able to say "стоп, эта дата снова как все" in one
// click, without knowing what the series currently says. The authorization is
// the same resolve-the-event-then-check-its-restaurant gate every other event
// mutation uses.
func (f *facade) ResetSeriesContent(ctx context.Context, actor Actor, eventID uuid.UUID, fields []domain.EventContentField) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return nil, err
	}
	if e.RecurrenceID == nil {
		return nil, fmt.Errorf("%w: this event does not belong to a series", domain.ErrValidation)
	}
	if f.series == nil {
		return nil, fmt.Errorf("resetting series content is not available: no recurrence source is wired")
	}
	if len(fields) == 0 {
		fields = domain.EventContentFields
	}
	for _, fld := range fields {
		if !fld.Valid() {
			return nil, fmt.Errorf("%w: unknown content field %q", domain.ErrValidation, fld)
		}
	}
	rec, err := f.series.GetByID(ctx, *e.RecurrenceID)
	if err != nil {
		return nil, err
	}
	before := e.Content()
	domain.ApplyEventContent(e, rec.Content(), fields)
	e.ContentOverrides = domain.EventContentDiff(rec.Content(), e.Content())
	// Nothing to do — and, importantly, nothing to re-moderate: a reset that
	// changes no word must not cost the venue a review.
	changed := len(domain.EventContentDiff(before, e.Content())) > 0
	if changed && domain.FeedDemotableAfterContentEdit(e.RestaurantID) {
		// Same ordering and the same reason as Update: the platform approved
		// the words that are being replaced.
		if err := f.feed.DemoteAfterContentEdit(ctx, domain.FeedItemEvent, eventID); err != nil {
			return nil, err
		}
	}
	if err := f.repo.Update(ctx, e); err != nil {
		return nil, err
	}
	f.attachImages(ctx, e)
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
	// A generated occurrence leaves a tombstone behind it: once the row is gone
	// its slot is free again, and the unique (recurrence_id, starts_at) index —
	// which is what protects every occurrence that still EXISTS, in whatever
	// status the venue left it — has nothing left to protect. Without this, the
	// next worker pass would cheerfully recreate the date the venue deleted.
	if err := f.skipSlot(ctx, e, e.StartsAt); err != nil {
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
	f.attachImages(ctx, e)
	return e, nil
}

func (f *facade) ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error) {
	if err := f.authorize(ctx, actor, &restaurantID); err != nil {
		return nil, 0, err
	}
	return f.repo.ListByRestaurant(ctx, restaurantID, statuses, page, perPage)
}

func (f *facade) ListPlatformAdmin(ctx context.Context, actor Actor, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error) {
	if err := authorizePlatformContent(actor); err != nil {
		return nil, 0, err
	}
	return f.repo.ListPlatform(ctx, statuses, page, perPage)
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
	flt.City = f.canonicalCity(ctx, flt.City)
	return f.repo.ListPublicUpcoming(ctx, flt, f.clock())
}

func (f *facade) GetPublic(ctx context.Context, restaurantID, eventID uuid.UUID) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// Never leak a draft/hidden event or one belonging to another restaurant
	// through the public endpoint: it simply does not exist to a guest.
	// A PLATFORM event belongs to no restaurant, so it can never be reached
	// through a restaurant's path — it has its own (GetPublicDetail).
	if e.RestaurantID == nil || *e.RestaurantID != restaurantID || e.Status != domain.EventPublished {
		return nil, fmt.Errorf("get public event: %w", domain.ErrNotFound)
	}
	f.attachImages(ctx, e)
	return e, nil
}

// GetPublicDetail reads the event's own page. The visibility rule (published,
// not yet ended, active venue when it has one) is enforced in the repository
// query, exactly as in the listing, so the two can never disagree about the
// same event.
func (f *facade) GetPublicDetail(ctx context.Context, eventID uuid.UUID) (*domain.EventListItem, error) {
	it, err := f.repo.GetPublicByID(ctx, eventID, f.clock())
	if err != nil {
		return nil, err
	}
	f.attachImages(ctx, &it.Event)
	return it, nil
}

// attachImages дочитывает галерею одного события. Ошибку здесь НЕ поднимаем:
// карточка события живёт и без галереи (у неё есть обложка), а уронить весь
// экран из-за необязательного блока — плохой размен.
func (f *facade) attachImages(ctx context.Context, e *domain.Event) {
	byID, err := f.repo.ImagesByEvent(ctx, []uuid.UUID{e.ID})
	if err != nil {
		return
	}
	e.Images = byID[e.ID]
}

// normalizeImages выбрасывает пустые строки и подрезает список: редактор
// присылает то, что набрал руками, и один случайный пустой слот не должен
// становиться «фотографией без адреса».
func normalizeImages(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			out = append(out, trimmed)
		}
		if len(out) == maxGalleryImages {
			break
		}
	}
	return out
}

// authorize is the ONE gate every event mutation goes through, and it has two
// shapes because an event has two possible owners:
//
//   - a venue-bound event (restaurantID non-nil) — PermRestaurantManage AT THAT
//     restaurant, superadmin bypasses. Unchanged, byte for byte.
//   - a PLATFORM event (nil) — there is no restaurant to hold a permission at,
//     so the global policy decides: domain.CanManagePlatformContent. Today that
//     is the superadmin alone.
//
// Note the ordering: the platform branch is taken from the EVENT's own owner,
// never from something the caller sent, which is what keeps a venue manager
// from reaching platform content by omitting a field.
func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID *uuid.UUID) error {
	if restaurantID == nil {
		return authorizePlatformContent(actor)
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, *restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's events", domain.ErrForbidden)
	}
	return nil
}

// authorizePlatformContent gates the platform's own «афиша». A single call site
// per operation and a single policy function, so widening it to a marketer role
// is an edit to domain.PlatformContentRoles and nothing else.
func authorizePlatformContent(actor Actor) error {
	if !domain.CanManagePlatformContent(actor.Role) {
		return fmt.Errorf("%w: only the platform may manage events that belong to no venue", domain.ErrForbidden)
	}
	return nil
}

// validateEvent enforces the invariants the DB CHECKs also guard, but with a
// domain.ErrValidation (422) instead of a 500 from a constraint violation.
func validateEvent(e *domain.Event) error {
	// A platform event cannot sell tickets — see the DB CHECK
	// events_platform_not_ticketed and usecase/tickets.validatePurchasable for
	// the whole reason (a payment needs a venue to settle to). Refusing it here
	// turns a 500 from a constraint violation into a 422 that says why.
	if e.RestaurantID == nil && e.Ticketed {
		return fmt.Errorf("%w: an event with no venue cannot sell tickets", domain.ErrValidation)
	}
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
// cityPtrEqual compares two optional city overrides, nil (no override) equal
// only to nil.
func cityPtrEqual(a, b *domain.City) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

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
		!i18nEqual(in.DescriptionI18n, cur.DescriptionI18n),
		!tagsEqual(normalizeTags(in.Tags), cur.Tags):
		return true
	}
	return false
}

// maxTags bounds the «Афиша» chip list. A card shows a handful of chips; a
// caller sending hundreds is a mistake, not a use case, so the extras are
// dropped rather than persisted. Same defensive spirit as maxRefundCutoffMinutes.
const maxTags = 10

// normalizeTags trims each chip, drops the blank ones, and caps the list at
// maxTags. It always returns a non-nil slice so the domain value and the JSON
// response never carry a nil-surprise (empty = "no chips", serialized as []).
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

// tagsEqual compares two chip lists by content and order: a nil and an empty
// list read the same to a guest, so they must not count as an edit.
func tagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// actionEqual compares two call-to-action buttons. Absent equals absent; a
// button equals another only when both the caption and the destination match,
// and "the event's own page" (nil url) is a destination like any other.
func actionEqual(a, b *domain.EventAction) bool {
	if a == nil || b == nil {
		return a == b
	}
	return strings.TrimSpace(a.Label) == b.Label && strPtrEqual(a.URL, b.URL)
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

// canonicalCity turns whatever a client put in ?city= into the spelling the
// listing actually compares against — the dictionary's own name, which is what
// both restaurants.city and an event's own override are normalized to.
//
// This is what lets one server answer three generations of client at once: the
// store build sends «Алматы», a new build may send «almaty», and a stale one
// may still send a city's previous name (kept as an alias on rename). All three
// resolve to the same stored spelling. Copied deliberately from
// usecase/restaurants.canonicalCity: two endpoints that disagree about what
// ?city=almaty means are worse than a duplicated ten lines.
//
// It never fails the request. An unknown value is passed through untouched, so
// the behaviour is exactly what it was before the dictionary existed: the
// filter matches nothing. A resolver ERROR is logged and also passed through —
// a dictionary outage must not turn a browsable Афиша into a 500.
func (f *facade) canonicalCity(ctx context.Context, in *domain.City) *domain.City {
	if in == nil || f.cities == nil || strings.TrimSpace(string(*in)) == "" {
		return in
	}
	entry, err := f.cities.Resolve(ctx, string(*in))
	if err != nil {
		slog.Warn("city dictionary lookup failed, filtering events by the raw value",
			"city", string(*in), "error", err)
		return in
	}
	if entry == nil {
		return in
	}
	v := domain.City(entry.Name)
	return &v
}

// cityOverride validates and canonicalizes the city an EDITOR typed on the
// event itself. Unlike the read filter this one is strict: nothing writes here
// except our own cabinet, so an unrecognized city is a mistake to report, not a
// value to store and quietly never match.
//
//   - nil or blank → nil: "no override", the event follows its venue.
//   - a HIDDEN city cannot be assigned, mirroring restaurants.validateCity —
//     hiding a city has to actually stop it spreading.
//   - the stored value is the dictionary's spelling, not the caller's. The
//     database trigger would normalize it anyway; doing it here means the
//     response echoes what was really saved.
//
// Without a resolver wired the legacy constant check stands, so a service
// started without the dictionary still refuses garbage rather than storing an
// override that can never match.
func (f *facade) cityOverride(ctx context.Context, raw *string) (*domain.City, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	v := strings.TrimSpace(*raw)
	if f.cities == nil {
		c := domain.City(v)
		if !c.Valid() {
			return nil, fmt.Errorf("%w: unknown city %q", domain.ErrValidation, v)
		}
		return &c, nil
	}
	entry, err := f.cities.Resolve(ctx, v)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: unknown city %q", domain.ErrValidation, v)
	}
	if !entry.IsActive {
		return nil, fmt.Errorf("%w: city %q is hidden", domain.ErrValidation, entry.Code)
	}
	c := domain.City(entry.Name)
	return &c, nil
}
