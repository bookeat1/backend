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

// Event is a one-off happening a restaurant hosts (a wine dinner, a live-music
// night). Title/Description are localized the same way the catalog is: a base
// scalar column (ru) plus an optional *_i18n jsonb map — see I18n.Resolve.
//
// Ticketed/TicketPriceMinor/Capacity are carried as FIELDS ONLY in this
// increment: the ticket purchase / payment flow is a deliberately deferred
// follow-up (see the PR). TicketPriceMinor is integer minor units (tiyin/
// cents), never a float, consistent with every money value in this codebase.
type Event struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	StartsAt        time.Time
	EndsAt          time.Time
	// Venue is free-text location detail within (or outside) the restaurant —
	// "rooftop terrace", "banquet hall". Empty means "at the restaurant".
	Venue         string
	CoverImageURL *string
	Status        EventStatus
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
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

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
type EventListItem struct {
	Event
	Restaurant EventRestaurant
}

// PublicEventFilter narrows the cross-venue public events listing. Every filter
// is optional; the zero value lists every visible event on the platform.
// Visibility itself is NOT a filter — published, not-yet-ended, at an active
// venue is always enforced (see EventRepository.ListPublicUpcoming).
type PublicEventFilter struct {
	// City filters by the HOST RESTAURANT's city (events carry no city of
	// their own). An unknown city value simply matches nothing.
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
	// to ErrNotFound.
	Create(ctx context.Context, e *Event) error
	// GetByID returns an event by its id regardless of status (staff resolve the
	// target and its restaurant before authorizing).
	GetByID(ctx context.Context, id uuid.UUID) (*Event, error)
	// Update overwrites the mutable fields of an existing event by id. Returns
	// ErrNotFound if id is absent.
	Update(ctx context.Context, e *Event) error
	// Delete removes an event. Returns ErrNotFound if id is absent.
	Delete(ctx context.Context, id uuid.UUID) error
	// ListByRestaurant returns a restaurant's events for the admin cabinet,
	// optionally filtered to the given statuses (empty = all), newest-start
	// first with id as a stable tie-breaker, paginated, plus the total count.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, statuses []EventStatus, page, perPage int) ([]Event, int, error)
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
	ListPublicUpcoming(ctx context.Context, f PublicEventFilter, now time.Time) ([]EventListItem, int, error)
}
