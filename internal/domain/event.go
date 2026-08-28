package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EventStatus is an event's publication state, stored as VARCHAR (validated
// here, never a Postgres ENUM). An event is born a draft; a venue's staff
// (PermRestaurantManage) publishes it, and may hide it again. Only published
// events are ever served by the public listing.
type EventStatus string

const (
	// EventDraft is a work-in-progress event, invisible to guests.
	EventDraft EventStatus = "draft"
	// EventPublished is live and shown in the public upcoming-events listing.
	EventPublished EventStatus = "published"
	// EventHidden was published once but is now withdrawn from public view.
	EventHidden EventStatus = "hidden"
)

// Valid reports whether s is a known event status.
func (s EventStatus) Valid() bool {
	switch s {
	case EventDraft, EventPublished, EventHidden:
		return true
	}
	return false
}

// Event is a one-off happening (a wine dinner, a live-music night). It is
// hosted EITHER by a restaurant or by the platform itself — see RestaurantID.
// Title/Description are localized the same way the catalog is: a base scalar
// column (ru) plus an optional *_i18n jsonb map — see I18n.Resolve.
//
// Ticketed/TicketPriceMinor/Capacity are carried as FIELDS ONLY in this
// increment: the ticket purchase / payment flow is a deliberately deferred
// follow-up (see the PR). TicketPriceMinor is integer minor units (tiyin/
// cents), never a float, consistent with every money value in this codebase.
type Event struct {
	ID uuid.UUID
	// RestaurantID is the HOST venue, and nil means the PLATFORM itself hosts
	// this event — «афиша без привязки к ресторану» (migration 0085). The two
	// cases differ everywhere it matters and nowhere else:
	//
	//   set → today's event in every respect: the venue's staff manage it
	//         (PermRestaurantManage at this id), the card carries the venue,
	//         and the city is the venue's unless City overrides it.
	//   nil → only the platform may create or edit it (see usecase/events
	//         authorize), the card carries NO venue, and the city is whatever
	//         City says — nil there meaning "every city".
	//
	// A platform event can never be Ticketed: selling a ticket means a payment
	// with a payee, and the platform has no venue account to settle into (a DB
	// CHECK in 0085 enforces the same rule).
	RestaurantID    *uuid.UUID
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	StartsAt        time.Time
	EndsAt          time.Time
	// Venue is free-text location detail within (or outside) the restaurant —
	// "rooftop terrace", "banquet hall". Empty means "at the restaurant".
	Venue string
	// City OVERRIDES the city this event is shown in (migration 0084). It is an
	// override and nothing else — the normal case is nil:
	//
	//   nil + a host venue  → the event lives in the VENUE's city, resolved on
	//                         every read (COALESCE(e.city, r.city)), so it can
	//                         never go stale when the venue moves.
	//   nil + no host venue → shown in EVERY city. This is the platform
	//                         event's "везде" state, reachable since 0085.
	//   set                 → shown in that city regardless of the venue.
	//
	// The stored value is the dictionary's own spelling of the city name (the
	// same currency as Restaurant.City), normalized by the database trigger
	// trg_events_sync_city — the listing compares cities as exact strings.
	City          *City
	CoverImageURL *string
	// Images — дополнительная галерея события в порядке редактора, БЕЗ обложки:
	// обложка живёт в CoverImageURL и её читают карточки и лента. Пустой срез —
	// «галереи нет», а не «фотографий нет вообще».
	Images []string
	Status EventStatus
	// Ticketed marks an event that will (in a later increment) sell tickets.
	Ticketed bool
	// TicketPriceMinor is the per-ticket price in integer minor units. nil when
	// the event is free / not ticketed. Not charged anywhere yet.
	TicketPriceMinor *int64
	// Capacity is the maximum number of attendees, nil when unbounded/unknown.
	Capacity *int
	// Tags are short free-text chips shown under the venue·date line on the
	// «Афиша» card and the event detail ("Бранч", "Живая музыка", "Коктейли",
	// "Красивый вид"). Empty slice = no tags; never nil once read from the
	// store (the column is text[] NOT NULL DEFAULT '{}').
	Tags []string
	// TicketsRefundable / TicketRefundCutoffMinutes are the venue's OWN refund
	// rules for this event (migration 0047) — see TicketRefundPolicy and
	// TicketRefundAllowed in event_refund_policy.go. They are the rules shown to
	// a guest BEFORE purchase and frozen onto every ticket sold from here on;
	// changing them never affects an already-bought ticket.
	TicketsRefundable         bool
	TicketRefundCutoffMinutes int
	// RecurrenceID names the rule that generated this occurrence (see
	// EventRecurrence), nil for an ordinary one-off event. It is set ONCE, at
	// generation time, and is never editable: the cabinet edits an occurrence
	// like any other event, and the generator leaves an existing row alone.
	// Deleting the rule nulls this field (ON DELETE SET NULL) instead of
	// deleting the occurrence — an event that already happened, with tickets
	// sold against it, must survive its rule.
	RecurrenceID *uuid.UUID
	// ContentOverrides lists the editorial fields THIS DATE owns (migration
	// 0097). A field listed here was set on the date itself and is never
	// rewritten when the series content changes; a field that is absent is
	// inherited and kept equal to the rule's own content. Always empty for an
	// event with no RecurrenceID — a one-off event has no series to inherit
	// from, so there is nothing to override.
	//
	// See event_series_content.go for why the inheritance is materialised into
	// this row instead of resolved on read.
	ContentOverrides []EventContentField
	// Action is the optional call-to-action button on the card (migration
	// 0085): a caption plus a destination that is either this event's own page
	// or an external partner link. nil = no button, which is what every event
	// created before 0085 has.
	Action    *EventAction
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsPlatform reports whether the platform itself hosts this event (no venue).
func (e Event) IsPlatform() bool { return e.RestaurantID == nil }

// EventRestaurant is the minimal venue identity carried next to an event in the
// cross-venue public listing: enough for the guest app's Explore card (who
// hosts it and where) without pulling the whole restaurant aggregate. Name is
// localized exactly like the catalog — a base scalar (ru) plus NameI18n.
type EventRestaurant struct {
	ID       uuid.UUID
	Name     string
	NameI18n I18n
	City     City
}

// EventListItem is one row of the cross-venue public events listing: the event
// itself plus the venue that hosts it. The venue is named Restaurant, not
// Venue, because Event.Venue already means something else (the free-text room
// inside the restaurant).
//
// Restaurant is nil for a PLATFORM event (Event.RestaurantID nil): there is no
// venue to name, and a zero-valued EventRestaurant would put an all-zeros uuid
// and an empty name on a guest's card. A pointer makes the compiler ask every
// consumer what it draws in that case.
type EventListItem struct {
	Event
	Restaurant *EventRestaurant
}

// PublicEventFilter narrows the cross-venue public events listing. Every filter
// is optional; the zero value lists every visible event on the platform.
// Visibility itself is NOT a filter — published, not-yet-ended, at an active
// venue is always enforced (see EventRepository.ListPublicUpcoming).
type PublicEventFilter struct {
	// City filters by the event's EFFECTIVE city: its own override when set,
	// otherwise the host venue's city (migration 0084). An event with no
	// effective city at all — a platform event with no override — is shown for
	// every value of this filter. The value
	// is resolved through the city dictionary before it reaches the store (see
	// usecase/events canonicalCity), so a code, an alias or a city's previous
	// name all work; a value the dictionary does not know is passed through and
	// simply matches nothing.
	City *City
	// RestaurantID narrows to one venue.
	RestaurantID *uuid.UUID
	// From/To bound the event's START time (inclusive on both ends). They
	// narrow the always-on "not finished yet" rule, never widen it: a From in
	// the past does not resurrect an event that already ended.
	From *time.Time
	To   *time.Time
	Page int // 1-based; <=0 means 1
	// PerPage <=0 means the default (20). The transport layer caps it.
	PerPage int
}

// EventRepository persists restaurant events. Get* return ErrNotFound when
// absent.
type EventRepository interface {
	// Create inserts a new event. An unknown restaurant_id (FK violation) maps
	// to ErrNotFound. A nil Event.RestaurantID stores a platform event.
	Create(ctx context.Context, e *Event) error
	// GetByID returns an event by its id regardless of status (staff resolve the
	// target and its restaurant before authorizing).
	GetByID(ctx context.Context, id uuid.UUID) (*Event, error)
	// Update overwrites the mutable fields of an existing event by id —
	// INCLUDING ContentOverrides, so a date's content and the record of which
	// fields it now owns are written in one statement and can never disagree.
	// Returns ErrNotFound if id is absent.
	Update(ctx context.Context, e *Event) error
	// Delete removes an event. Returns ErrNotFound if id is absent.
	Delete(ctx context.Context, id uuid.UUID) error
	// ReplaceImages overwrites the event's gallery with urls, in the given
	// order. An empty slice clears it. The cover is NOT part of this set.
	ReplaceImages(ctx context.Context, eventID uuid.UUID, urls []string) error
	// ImagesByEvent loads galleries for several events at once — the collection
	// screen renders many of them and one query per event would be a burst.
	ImagesByEvent(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID][]string, error)
	// ListByRestaurant returns a restaurant's events for the admin cabinet,
	// optionally filtered to the given statuses (empty = all), newest-start
	// first with id as a stable tie-breaker, paginated, plus the total count.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, statuses []EventStatus, page, perPage int) ([]Event, int, error)
	// ListPlatform is ListByRestaurant for the events NOBODY hosts — the
	// platform's own «афиша» (restaurant_id IS NULL). Same ordering, same
	// status filter, same pagination contract; a separate method rather than a
	// nullable argument because "all events of no venue" and "all events of
	// some venue" are two different questions with two different authorizations.
	ListPlatform(ctx context.Context, statuses []EventStatus, page, perPage int) ([]Event, int, error)
	// ListPublishedUpcoming returns a restaurant's PUBLISHED events that have
	// not yet ended (ends_at > now), soonest first with id as a stable
	// tie-breaker, paginated, plus the total count. This is the public listing.
	ListPublishedUpcoming(ctx context.Context, restaurantID uuid.UUID, now time.Time, page, perPage int) ([]Event, int, error)
	// ListPublicUpcoming is the CROSS-VENUE public listing behind the guest
	// app's Explore screen: PUBLISHED events that have not yet ended
	// (ends_at > now) hosted by an ACTIVE restaurant, soonest first with id as
	// a stable tie-breaker, narrowed by f, paginated, plus the total count.
	// The venue is joined in (EventListItem.Restaurant) so the screen needs no
	// per-card follow-up query, same choice as the feed read model.
	//
	// A RECURRING event appears ONCE, as its nearest upcoming occurrence inside
	// the filtered window; one-off events are unaffected. The total counts the
	// collapsed set, so it agrees with what pagination actually walks. This
	// collapse belongs to the guest catalog only — the detail endpoint, tickets
	// and bookings address a single occurrence, and the cabinet listing
	// (ListByRestaurant) still shows every date.
	ListPublicUpcoming(ctx context.Context, f PublicEventFilter, now time.Time) ([]EventListItem, int, error)
	// GetPublicByID returns ONE published, not-yet-ended event addressed by its
	// own id — whoever hosts it — together with its venue when it has one
	// (nil for a platform event). It is what the event's OWN page reads, and
	// therefore what an action button targeting that page can point at; the
	// older GetByID + "does it belong to this restaurant" pair cannot express a
	// venue-less event at all. ErrNotFound for a draft/hidden/finished event:
	// to a guest it does not exist.
	GetPublicByID(ctx context.Context, id uuid.UUID, now time.Time) (*EventListItem, error)
}
