package bookings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

// CreateUseCase creates bookings. It is the only place that decides which
// tables a booking occupies; the actual double-booking guarantee is the GiST
// exclusion constraint on booking_tables (spec §4.3), not this code.
type CreateUseCase interface {
	Create(ctx context.Context, actor Actor, in CreateInput) (*BookingDetails, error)
}

// CreateInput is a booking request. TableIDs and Force are manager-only fields;
// a guest request that sets them is rejected with ErrForbidden.
type CreateInput struct {
	RestaurantID uuid.UUID
	// UserID owning the booking. Nil means a guest (walk-in / phone) booking
	// created by staff. For guest self-service the transport layer sets it to
	// the authenticated user.
	UserID      *uuid.UUID
	Name        string
	Phone       string
	Email       string
	Guests      int
	StartsAt    time.Time
	Notes       *string
	Source      domain.BookingSource
	PromotionID *uuid.UUID
	EventID     *uuid.UUID
	Items       []ItemInput

	// TableIDs pins the booking to specific tables (manual placement by staff).
	TableIDs []uuid.UUID
	// Force skips availability-based table selection (spec §4.2). It does NOT
	// skip the blacklist, the anti-fraud limit, or input/window validation.
	Force bool
}

// ItemInput is one pre-ordered menu position. Price is captured at booking time
// in minor units and never rewritten when the menu changes.
type ItemInput struct {
	MenuItemID *uuid.UUID
	Name       string
	PriceMinor int64
	Currency   string
	Quantity   int
	Comment    *string
}

type createUseCase struct {
	bookings    domain.BookingRepository
	links       domain.BookingTableRepository
	items       domain.BookingItemRepository
	history     domain.BookingStatusHistoryRepository
	outbox      domain.BookingOutboxRepository
	blacklist   domain.BookingBlacklistRepository
	rateLog     domain.BookingRateLogRepository
	restaurants restaurantReader
	schedule    scheduleReader
	managers    managerChecker
	tx          domain.TxManager
	cfg         Config
}

// NewCreateUseCase constructs the booking creation usecase.
func NewCreateUseCase(
	bookings domain.BookingRepository,
	links domain.BookingTableRepository,
	items domain.BookingItemRepository,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	blacklist domain.BookingBlacklistRepository,
	rateLog domain.BookingRateLogRepository,
	restaurants restaurantReader,
	schedule scheduleReader,
	managers managerChecker,
	tx domain.TxManager,
	cfg Config,
) CreateUseCase {
	return &createUseCase{
		bookings: bookings, links: links, items: items, history: history,
		outbox: outbox, blacklist: blacklist, rateLog: rateLog,
		restaurants: restaurants, schedule: schedule, managers: managers,
		tx: tx, cfg: cfg.withDefaults(),
	}
}

// Create runs the checks in a fixed order — cheap and unconditional first,
// data-dependent last:
//
//	input → policy → contact normalization → blacklist → anti-fraud →
//	booking window → table selection → one transaction (booking + tables +
//	items + history + outbox), plus auto-confirmation.
func (u *createUseCase) Create(ctx context.Context, actor Actor, in CreateInput) (*BookingDetails, error) {
	if err := validateCreate(in); err != nil {
		return nil, err
	}
	acc, err := resolveAccess(ctx, u.managers, actor, in.RestaurantID)
	if err != nil {
		return nil, err
	}
	// Manual placement and forced placement are staff powers, checked before
	// anything reads data (spec §4.2: "the guest has no such option").
	if !acc.staff() && (in.Force || len(in.TableIDs) > 0) {
		return nil, fmt.Errorf("%w: manual placement is restricted to venue staff", domain.ErrForbidden)
	}

	rest, err := u.restaurants.GetByID(ctx, in.RestaurantID)
	if err != nil {
		return nil, err
	}
	if !rest.IsActive {
		return nil, fmt.Errorf("%w: restaurant is not accepting bookings", domain.ErrForbidden)
	}
	policy := resolvePolicy(rest.Restaurant, u.cfg)
	if in.Guests > policy.MaxGuestsPerBooking {
		return nil, fmt.Errorf("%w: at most %d guests per booking", domain.ErrValidation, policy.MaxGuestsPerBooking)
	}

	normalizedPhone := phone.Normalize(in.Phone)
	if normalizedPhone == "" {
		return nil, fmt.Errorf("%w: phone required", domain.ErrValidation)
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))

	// Blacklist: venue-scoped and global entries, matched on the normalized
	// contacts — matching raw input would never hit.
	entry, err := u.blacklist.Match(ctx, domain.BlacklistQuery{
		RestaurantID:    &in.RestaurantID,
		UserID:          in.UserID,
		PhoneNormalized: normalizedPhone,
		Email:           email,
	})
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if entry != nil {
		return nil, fmt.Errorf("%w: guest is blacklisted", domain.ErrForbidden)
	}

	// Anti-fraud. The attempt is logged before the work is done and outside the
	// transaction below, so a failed attempt still counts (same reasoning as
	// the OTP attempt counter).
	if err := u.checkRate(ctx, normalizedPhone); err != nil {
		return nil, err
	}
	if err := u.rateLog.Create(ctx, &domain.BookingRateLogEntry{
		ID: uuid.New(), UserID: in.UserID, PhoneNormalized: &normalizedPhone,
		Email: nullable(email), RestaurantID: &in.RestaurantID,
		Action: domain.RateLogCreate, CreatedAt: time.Now(),
	}); err != nil {
		return nil, err
	}

	startsAt := in.StartsAt.UTC()
	sched, err := loadSchedule(ctx, u.schedule, in.RestaurantID)
	if err != nil {
		return nil, err
	}
	if err := u.checkWindow(startsAt, policy, sched); err != nil {
		return nil, err
	}

	tables, err := u.selectTables(ctx, in, policy, sched, startsAt)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: in.RestaurantID, UserID: in.UserID,
		Name: strings.TrimSpace(in.Name), Phone: in.Phone, Email: email,
		PhoneNormalized: normalizedPhone, Guests: in.Guests,
		StartsAt: startsAt, EndsAt: startsAt.Add(policy.Duration),
		Status: domain.BookingPending, Source: in.Source, Notes: in.Notes,
		PromotionID: in.PromotionID, EventID: in.EventID,
		CreatedByAdmin: acc.staff(), ForcedPlacement: in.Force,
		CreatedAt: now, UpdatedAt: now,
	}

	slotFrom, slotTo := occupancyWindow(startsAt, policy)
	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.bookings.Create(ctx, b); err != nil {
			return err
		}
		if len(tables) > 0 {
			links := make([]domain.BookingTable, 0, len(tables))
			for _, t := range tables {
				links = append(links, domain.BookingTable{
					ID: uuid.New(), BookingID: b.ID, TableID: t.ID,
					SlotStart: slotFrom, SlotEnd: slotTo, CreatedAt: now,
				})
			}
			if err := u.links.Create(ctx, links); err != nil {
				return err
			}
		}
		if len(in.Items) > 0 {
			if err := u.items.Create(ctx, buildItems(b.ID, in.Items, now)); err != nil {
				return err
			}
		}
		if err := recordTransition(ctx, u.history, u.outbox, b, nil,
			acc.actorType(), actorID(actor), nil, now); err != nil {
			return err
		}
		if !policy.AutoConfirm {
			return nil
		}
		// Auto-confirmation is a real transition: its own status update,
		// history row and event, inside the same transaction.
		from := b.Status
		b.Status = domain.BookingConfirmed
		b.ConfirmedAt = &now
		if err := u.bookings.UpdateStatus(ctx, b.ID, domain.BookingConfirmed, now); err != nil {
			return err
		}
		return recordTransition(ctx, u.history, u.outbox, b, &from,
			domain.ActorSystem, nil, strPtr("auto-confirm"), now)
	})
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: the selected time slot was just taken", domain.ErrAlreadyExists)
		}
		return nil, err
	}

	details := &BookingDetails{Booking: *b}
	if details.Items, err = u.items.ListByBooking(ctx, b.ID); err != nil {
		return nil, err
	}
	if details.Tables, err = u.links.ListByBooking(ctx, b.ID); err != nil {
		return nil, err
	}
	return details, nil
}

func (u *createUseCase) checkRate(ctx context.Context, normalizedPhone string) error {
	n, err := u.rateLog.CountSince(ctx, normalizedPhone, domain.RateLogCreate, time.Now().Add(-u.cfg.RateWindow))
	if err != nil {
		return err
	}
	if n >= u.cfg.RateLimit {
		return fmt.Errorf("%w: too many booking attempts, try again later", domain.ErrValidation)
	}
	return nil
}

// checkWindow enforces the lead time, the horizon and the venue's opening
// hours / bookable slots. All calendar maths happens in the venue's timezone.
func (u *createUseCase) checkWindow(startsAt time.Time, policy domain.BookingPolicy, sched schedule) error {
	switch windowReason(startsAt, policy, time.Now()) {
	case ReasonTooSoon:
		return fmt.Errorf("%w: bookings must be made at least %d minutes ahead",
			domain.ErrValidation, int(policy.Lead/time.Minute))
	case ReasonHorizon:
		return fmt.Errorf("%w: bookings open at most %d days ahead",
			domain.ErrValidation, policy.HorizonDays)
	}
	if !isBookableStart(sched, startsAt, policy, u.cfg.SlotStep) {
		return fmt.Errorf("%w: the restaurant is not open for bookings at this time", domain.ErrValidation)
	}
	return nil
}

// isBookableStart reports whether startsAt is one of the venue's bookable start
// times. Both the start's own calendar day and the previous one are expanded,
// so a venue closing past midnight (18:00–02:00) still accepts a 01:00 start.
func isBookableStart(sched schedule, startsAt time.Time, policy domain.BookingPolicy, step time.Duration) bool {
	loc := policyLocation(policy)
	local := startsAt.In(loc)
	day := startOfDay(local, loc)
	for _, d := range []time.Time{day, day.AddDate(0, 0, -1)} {
		for _, c := range candidateStarts(sched, d, policy, step) {
			if c.Equal(startsAt) {
				return true
			}
		}
	}
	return false
}

// selectTables resolves the tables the booking will occupy.
//
//   - explicit TableIDs (staff only): validated to belong to the venue and to
//     be active; capacity is still enforced unless Force is set;
//   - Force without TableIDs: no links are created at all. The exclusion
//     constraint is a hard invariant — a forced booking is recorded as
//     unassigned seating (forced_placement = true) and the manager seats the
//     party by hand;
//   - otherwise: the free tables at that slot are picked automatically
//     (single table, or a combination of up to three).
func (u *createUseCase) selectTables(
	ctx context.Context,
	in CreateInput,
	policy domain.BookingPolicy,
	sched schedule,
	startsAt time.Time,
) ([]domain.RestaurantTable, error) {
	if len(in.TableIDs) > 0 {
		if len(in.TableIDs) > maxCombinedTables {
			return nil, fmt.Errorf("%w: at most %d tables per booking", domain.ErrValidation, maxCombinedTables)
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
				return nil, fmt.Errorf("%w: table %s does not belong to this restaurant", domain.ErrValidation, id)
			}
			picked = append(picked, t)
			seats += t.Capacity
		}
		if seats < in.Guests && !in.Force {
			return nil, fmt.Errorf("%w: selected tables seat %d, %d guests requested",
				domain.ErrValidation, seats, in.Guests)
		}
		return picked, nil
	}
	if in.Force {
		return nil, nil // unassigned seating, see doc comment
	}

	from, to := occupancyWindow(startsAt, policy)
	busy, err := u.links.ListBusy(ctx, in.RestaurantID, from, to)
	if err != nil {
		return nil, err
	}
	picked := pickTables(freeTables(sched.tables, busy, from, to), in.Guests)
	if len(picked) == 0 {
		return nil, fmt.Errorf("%w: no table available for %d guests at this time",
			domain.ErrAlreadyExists, in.Guests)
	}
	return picked, nil
}

func validateCreate(in CreateInput) error {
	if in.RestaurantID == uuid.Nil {
		return fmt.Errorf("%w: restaurant required", domain.ErrValidation)
	}
	if strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("%w: name required", domain.ErrValidation)
	}
	if in.Guests <= 0 {
		return fmt.Errorf("%w: guests must be positive", domain.ErrValidation)
	}
	if in.StartsAt.IsZero() {
		return fmt.Errorf("%w: starts_at required", domain.ErrValidation)
	}
	if !in.Source.Valid() {
		return fmt.Errorf("%w: unknown booking source", domain.ErrValidation)
	}
	for _, it := range in.Items {
		if strings.TrimSpace(it.Name) == "" || it.Quantity <= 0 || it.PriceMinor < 0 {
			return fmt.Errorf("%w: invalid pre-ordered item", domain.ErrValidation)
		}
	}
	return nil
}

func buildItems(bookingID uuid.UUID, in []ItemInput, now time.Time) []domain.BookingItem {
	out := make([]domain.BookingItem, 0, len(in))
	for _, it := range in {
		currency := it.Currency
		if currency == "" {
			currency = "KZT"
		}
		out = append(out, domain.BookingItem{
			ID: uuid.New(), BookingID: bookingID, MenuItemID: it.MenuItemID,
			ItemName: it.Name, PriceMinor: it.PriceMinor, Currency: currency,
			Quantity: it.Quantity, Status: domain.BookingItemPending,
			Comment: it.Comment, CreatedAt: now, UpdatedAt: now,
		})
	}
	return out
}

func actorID(a Actor) *uuid.UUID {
	if a.UserID == uuid.Nil {
		return nil
	}
	id := a.UserID
	return &id
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func strPtr(s string) *string { return &s }
