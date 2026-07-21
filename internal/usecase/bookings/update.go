package bookings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// UpdateUseCase is PATCH /bookings/{id} (spec §7): moving a booking in time,
// changing the party size, reassigning tables. It is a venue-staff power — a
// guest who wants another time cancels and books again, which keeps the
// anti-fraud log and the cancellation deadline meaningful.
type UpdateUseCase interface {
	Update(ctx context.Context, actor Actor, id uuid.UUID, in UpdateInput) (*BookingDetails, error)
}

// UpdateInput carries only the fields being changed; nil means "leave as is".
// TableIDs is nil for "keep / recompute", and an explicit (possibly empty)
// slice for "set exactly these".
type UpdateInput struct {
	StartsAt *time.Time
	Guests   *int
	Notes    *string
	TableIDs []uuid.UUID
	Force    bool
}

type updateUseCase struct {
	bookings    domain.BookingRepository
	links       domain.BookingTableRepository
	outbox      domain.BookingOutboxRepository
	restaurants restaurantReader
	schedule    scheduleReader
	managers    managerChecker
	tx          domain.TxManager
	cfg         Config
}

// NewUpdateUseCase constructs the booking amendment usecase.
func NewUpdateUseCase(
	bookings domain.BookingRepository,
	links domain.BookingTableRepository,
	outbox domain.BookingOutboxRepository,
	restaurants restaurantReader,
	schedule scheduleReader,
	managers managerChecker,
	tx domain.TxManager,
	cfg Config,
) UpdateUseCase {
	return &updateUseCase{
		bookings: bookings, links: links, outbox: outbox, restaurants: restaurants,
		schedule: schedule, managers: managers, tx: tx, cfg: cfg.withDefaults(),
	}
}

func (u *updateUseCase) Update(ctx context.Context, actor Actor, id uuid.UUID, in UpdateInput) (*BookingDetails, error) {
	b, err := u.bookings.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	acc, err := authorize(ctx, u.managers, actor, b)
	if err != nil {
		return nil, err
	}
	if !acc.staff() {
		return nil, fmt.Errorf("%w: only the restaurant can amend a booking", domain.ErrForbidden)
	}
	// A booking that no longer holds its tables cannot be moved: rescheduling a
	// cancelled or completed visit would resurrect it without a status change.
	if !b.Status.HoldsTable() {
		return nil, fmt.Errorf("%w: booking is %s", domain.ErrInvalidStatus, b.Status)
	}

	rest, err := u.restaurants.GetByID(ctx, b.RestaurantID)
	if err != nil {
		return nil, err
	}
	policy := resolvePolicy(rest.Restaurant, u.cfg)
	sched, err := loadSchedule(ctx, u.schedule, b.RestaurantID)
	if err != nil {
		return nil, err
	}

	moved := false
	if in.Guests != nil {
		if *in.Guests <= 0 {
			return nil, fmt.Errorf("%w: guests must be positive", domain.ErrValidation)
		}
		if *in.Guests > policy.MaxGuestsPerBooking {
			return nil, fmt.Errorf("%w: at most %d guests per booking", domain.ErrValidation, policy.MaxGuestsPerBooking)
		}
		moved = moved || *in.Guests != b.Guests
		b.Guests = *in.Guests
	}
	if in.Notes != nil {
		notes := strings.TrimSpace(*in.Notes)
		b.Notes = &notes
	}
	if in.StartsAt != nil {
		start := in.StartsAt.UTC()
		if !in.Force && !withinOpeningHours(sched, start, policy) {
			return nil, fmt.Errorf("%w: the restaurant is closed at this time", domain.ErrValidation)
		}
		if windowReason(start, policy, time.Now()) == ReasonHorizon {
			return nil, fmt.Errorf("%w: bookings open at most %d days ahead",
				domain.ErrValidation, policy.HorizonDays)
		}
		moved = moved || !start.Equal(b.StartsAt)
		b.StartsAt = start
		b.EndsAt = start.Add(policy.Duration)
	}
	b.UpdatedAt = time.Now()

	links, relink, err := u.resolveLinks(ctx, b, in, policy, sched, moved)
	if err != nil {
		return nil, err
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.bookings.Update(ctx, b); err != nil {
			return err
		}
		if relink {
			// ReplaceForBooking deletes this booking's own links first, so the
			// exclusion constraint cannot fire against the booking's previous
			// slot when it is merely shifted by a few minutes.
			if err := u.links.ReplaceForBooking(ctx, b.ID, links); err != nil {
				return err
			}
		}
		return publish(ctx, u.outbox, b, domain.EventBookingUpdated, b.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: the selected time slot was just taken", domain.ErrAlreadyExists)
		}
		return nil, err
	}

	details := &BookingDetails{Booking: *b}
	if details.Tables, err = u.links.ListByBooking(ctx, b.ID); err != nil {
		return nil, err
	}
	return details, nil
}

// resolveLinks decides the booking's table set after the amendment. It returns
// relink=false when nothing about the placement changed, so a pure notes edit
// does not touch booking_tables at all.
func (u *updateUseCase) resolveLinks(
	ctx context.Context,
	b *domain.Booking,
	in UpdateInput,
	policy domain.BookingPolicy,
	sched schedule,
	moved bool,
) ([]domain.BookingTable, bool, error) {
	if in.TableIDs == nil && !moved {
		return nil, false, nil
	}
	from, to := occupancyWindow(b.StartsAt, policy)
	now := time.Now()

	build := func(tables []domain.RestaurantTable) []domain.BookingTable {
		out := make([]domain.BookingTable, 0, len(tables))
		for _, t := range tables {
			out = append(out, domain.BookingTable{
				ID: uuid.New(), BookingID: b.ID, TableID: t.ID,
				SlotStart: from, SlotEnd: to, CreatedAt: now,
			})
		}
		return out
	}

	if in.TableIDs != nil {
		if len(in.TableIDs) > maxCombinedTables {
			return nil, false, fmt.Errorf("%w: at most %d tables per booking", domain.ErrValidation, maxCombinedTables)
		}
		byID := make(map[uuid.UUID]domain.RestaurantTable, len(sched.tables))
		for _, t := range sched.tables {
			byID[t.ID] = t
		}
		picked := make([]domain.RestaurantTable, 0, len(in.TableIDs))
		seats := 0
		for _, id := range in.TableIDs {
			t, ok := byID[id]
			if !ok {
				return nil, false, fmt.Errorf("%w: table %s does not belong to this restaurant", domain.ErrValidation, id)
			}
			picked = append(picked, t)
			seats += t.Capacity
		}
		if seats < b.Guests && !in.Force {
			return nil, false, fmt.Errorf("%w: selected tables seat %d, %d guests requested",
				domain.ErrValidation, seats, b.Guests)
		}
		return build(picked), true, nil
	}
	if in.Force {
		return nil, true, nil // unassigned seating, seated by hand
	}

	busy, err := u.links.ListBusy(ctx, b.RestaurantID, from, to)
	if err != nil {
		return nil, false, err
	}
	// The booking's own current links are part of "busy" — drop them, or a
	// booking could never be moved by less than its own duration.
	own, err := u.links.ListByBooking(ctx, b.ID)
	if err != nil {
		return nil, false, err
	}
	picked := pickTables(freeTables(sched.tables, excludeOwn(busy, own), from, to), b.Guests)
	if len(picked) == 0 {
		return nil, false, fmt.Errorf("%w: no table available for %d guests at this time",
			domain.ErrAlreadyExists, b.Guests)
	}
	return build(picked), true, nil
}

// excludeOwn removes the intervals produced by the booking's own links, matched
// on table and exact slot bounds.
func excludeOwn(busy []domain.TableBusyInterval, own []domain.BookingTable) []domain.TableBusyInterval {
	if len(own) == 0 {
		return busy
	}
	out := make([]domain.TableBusyInterval, 0, len(busy))
	for _, b := range busy {
		mine := false
		for _, o := range own {
			if o.TableID == b.TableID && o.SlotStart.Equal(b.From) && o.SlotEnd.Equal(b.To) {
				mine = true
				break
			}
		}
		if !mine {
			out = append(out, b)
		}
	}
	return out
}
