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
	Capacity  *int
	CreatedAt time.Time
	UpdatedAt time.Time
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
}
