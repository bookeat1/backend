package bookings

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// PolicyUseCase reads and edits one venue's booking-policy overrides (spec §4.2
// level 2). Without it the columns added in wave 3 are write-only via raw SQL:
// a venue could be read as "auto_confirm = false" but never actually set that
// way through the product.
//
// Authorization matches the rest of the venue cabinet: the restaurant's own
// manager or an admin; a manager of another venue gets ErrForbidden.
type PolicyUseCase interface {
	// Get returns the stored overrides plus the effective policy they resolve to.
	Get(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*PolicyView, error)
	// Update applies a PATCH of the overrides (nil field = leave as-is) and
	// returns the resulting effective policy.
	Update(ctx context.Context, actor Actor, restaurantID uuid.UUID, in domain.BookingPolicyOverride) (*PolicyView, error)
}

// PolicyView pairs what the venue actually stores (nil = "inherit the global
// default") with the policy those overrides resolve to. The client needs both:
// the overrides to render the form, the effective values to show what the
// blank fields currently mean.
type PolicyView struct {
	Override  domain.BookingPolicyOverride
	Effective domain.BookingPolicy
}

// Bounds for the editable policy fields. They are enforced here rather than as
// DB CHECKs because the columns predate this endpoint and legacy rows may hold
// anything; resolvePolicy already ignores nonsense on read, this stops it from
// being written in the first place.
const (
	minPolicyDurationMinutes = 15
	maxPolicyDurationMinutes = 600
	maxPolicyBufferMinutes   = 240
	maxPolicyLeadMinutes     = 30 * 24 * 60
	minPolicyHorizonDays     = 1
	maxPolicyHorizonDays     = 365
	maxPolicyCancelMinutes   = 7 * 24 * 60
	minPolicySLAMinutes      = 1
	maxPolicySLAMinutes      = 24 * 60
	minPolicyMaxGuests       = 1
	maxPolicyMaxGuests       = 100
)

type policyUseCase struct {
	restaurants restaurantReader
	policies    policyWriter
	managers    managerChecker
	schedule    scheduleReader
	capacity    domain.BookingCapacityRepository
	links       domain.BookingTableRepository
	bookings    bookingLister
	tx          domain.TxManager
	cfg         Config
}

// NewPolicyUseCase constructs the venue booking-policy usecase.
//
// schedule, capacity, links, bookings and tx are consulted ONLY when the
// capacity mode or the declared capacity is being changed — every one of them
// may be nil, in which case such a change is refused instead of being performed
// blind. Plain policy edits (duration, buffer, auto-confirm…) never touch them.
func NewPolicyUseCase(
	restaurants restaurantReader,
	policies policyWriter,
	managers managerChecker,
	schedule scheduleReader,
	capacity domain.BookingCapacityRepository,
	links domain.BookingTableRepository,
	bookings bookingLister,
	tx domain.TxManager,
	cfg Config,
) PolicyUseCase {
	return &policyUseCase{
		restaurants: restaurants, policies: policies, managers: managers,
		schedule: schedule, capacity: capacity, links: links, bookings: bookings,
		tx: tx, cfg: cfg,
	}
}

func (u *policyUseCase) Get(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*PolicyView, error) {
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	return u.view(ctx, restaurantID)
}

func (u *policyUseCase) Update(ctx context.Context, actor Actor, restaurantID uuid.UUID, in domain.BookingPolicyOverride) (*PolicyView, error) {
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	in = normalizePolicyOverride(in)
	if err := validatePolicyOverride(in); err != nil {
		return nil, err
	}
	if in.BookingCapacityMode == nil && in.BookingCapacitySeats == nil {
		// A single UPDATE is atomic on its own; the read that follows only
		// builds the response, so no transaction is needed here. Existing
		// bookings are deliberately left untouched — a policy change applies to
		// future ones.
		if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
			return nil, err
		}
		return u.view(ctx, restaurantID)
	}
	if err := u.applyCapacityChange(ctx, restaurantID, in); err != nil {
		return nil, err
	}
	return u.view(ctx, restaurantID)
}

// applyCapacityChange writes a capacity-mode / capacity-size change together
// with everything that has to stay true afterwards. It is the one policy edit
// that is NOT a lone column write, because the two modes account for occupancy
// in two different places and the venue's existing bookings have to survive the
// move between them.
//
// What happens to bookings, explicitly:
//
//   - tables → seats: every FUTURE booking that still holds a seat is given
//     capacity holds computed from its own time and party size, inside the same
//     transaction as the mode switch. Without that backfill those bookings
//     would be invisible to the new engine and the venue would be sold their
//     seats a second time. If they do not fit into the newly declared capacity,
//     the whole switch is refused — the venue is told which moment is over
//     capacity instead of quietly losing guests.
//   - seats → tables: every table-less booking that still holds space is SEATED
//     at real tables inside the same transaction as the mode switch, and its
//     capacity holds are dropped — booking_tables and its exclusion constraint
//     become the one authority for them, exactly as for every other booking of a
//     table-mode venue. Switching is refused unless the venue has active tables,
//     and refused again if any of those bookings cannot be seated at all.
//   - raising / lowering the declared capacity: existing bookings are never
//     touched. Lowering below what is already sold for a future moment is
//     refused, because the alternative is a booking the venue has confirmed and
//     can no longer honour.
func (u *policyUseCase) applyCapacityChange(ctx context.Context, restaurantID uuid.UUID, in domain.BookingPolicyOverride) error {
	if u.capacity == nil || u.links == nil || u.bookings == nil || u.tx == nil || u.schedule == nil {
		return fmt.Errorf("%w: capacity mode is not configured on this deployment", domain.ErrValidation)
	}
	agg, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return err
	}
	current := resolvePolicy(agg.Restaurant, u.cfg)

	// The target state is the patch applied on top of what is stored today: a
	// venue may set the capacity now and flip the mode later, or vice versa.
	target := current
	if in.BookingCapacityMode != nil {
		target.CapacityMode = *in.BookingCapacityMode
	}
	if in.BookingCapacitySeats != nil {
		target.CapacitySeats = *in.BookingCapacitySeats
	}
	if target.CapacityMode == domain.CapacityModeSeats && target.CapacitySeats <= 0 {
		return fmt.Errorf("%w: booking_capacity_seats is required to book by total capacity",
			domain.ErrValidation)
	}

	if target.CapacityMode == domain.CapacityModeTables {
		tables, err := u.activeTables(ctx, restaurantID)
		if err != nil {
			return err
		}
		if len(tables) == 0 {
			return fmt.Errorf("%w: this restaurant has no active tables, so booking by tables would make every slot unbookable",
				domain.ErrValidation)
		}
		return u.tx.WithinTx(ctx, func(ctx context.Context) error {
			// Same lock, taken FIRST, and for the same reason as the seats
			// branch below: this switch reads the venue's live bookings and
			// writes the placement derived from them, so it must not interleave
			// with a create that read the previous policy. With the lock held a
			// concurrent table-less create either commits before this switch
			// (and is seated by it) or re-reads the policy under the lock, sees
			// table mode and refuses itself — see create.go.
			if err := u.capacity.LockVenue(ctx, restaurantID); err != nil {
				return err
			}
			if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
				return err
			}
			return u.seatCapacityBookings(ctx, restaurantID, target, tables)
		})
	}

	// Lowering the capacity: refuse while the venue has more guests already
	// booked for a future moment than the new number allows. Checked before the
	// write for a readable error; the bucket CHECK would refuse it anyway.
	if peak, err := u.capacity.PeakTaken(ctx, restaurantID, time.Now()); err != nil {
		return err
	} else if peak != nil && peak.SeatsTaken > target.CapacitySeats {
		return fmt.Errorf("%w: %d guests are already booked for %s; capacity cannot be set below that",
			domain.ErrValidation, peak.SeatsTaken, peak.BucketStart.UTC().Format(time.RFC3339))
	}

	return u.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Same per-venue lock a booking create takes, and taken FIRST: the
		// policy write and the holds rebuilt from it must not interleave with a
		// create that read the previous policy.
		if err := u.capacity.LockVenue(ctx, restaurantID); err != nil {
			return err
		}
		if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
			return err
		}
		return u.rebuildHolds(ctx, restaurantID, target)
	})
}

// rebuildHolds (re)writes the capacity holds of every future booking that still
// holds a seat, so the bucket counters describe the venue's real load under the
// declared capacity. ReplaceForBooking rather than Create: a venue that has
// switched modes back and forth must not trip over the holds of the previous
// round.
func (u *policyUseCase) rebuildHolds(ctx context.Context, restaurantID uuid.UUID, policy domain.BookingPolicy) error {
	list, err := u.liveBookings(ctx, restaurantID, policy)
	if err != nil {
		return err
	}
	for i := range list {
		b := list[i]
		if b.Guests > policy.CapacitySeats {
			return fmt.Errorf("%w: booking of %d guests on %s does not fit a capacity of %d",
				domain.ErrValidation, b.Guests, b.StartsAt.UTC().Format(time.RFC3339), policy.CapacitySeats)
		}
		holds := buildCapacityHolds(&b, policy, time.Now())
		if err := u.capacity.ReplaceForBooking(ctx, b.ID, holds); err != nil {
			if errors.Is(err, domain.ErrAlreadyExists) {
				return fmt.Errorf("%w: the bookings already accepted for %s exceed a capacity of %d",
					domain.ErrValidation, b.StartsAt.UTC().Format(time.RFC3339), policy.CapacitySeats)
			}
			return err
		}
	}
	return nil
}

// seatCapacityBookings is the seats → tables counterpart of rebuildHolds, and it
// exists because the switch used to do NOTHING here (review finding,
// 25.07.2026): it checked that the venue had a table and wrote the policy.
//
// A table-less booking owns no booking_tables row — create.go never writes one
// in seats mode — and the table engine reads nothing else (ListBusy joins
// booking_tables alone). So the instant the venue was back in table mode, every
// table read FREE at a time a confirmed party was already sitting there, and the
// next ordinary guest was sold the same seats. Reachable with three product
// actions: book table-lessly, add a table, flip the mode.
//
// The fix gives every such booking a real placement, in this same locked
// transaction, so the guarantee is again the GiST exclusion constraint on
// booking_tables and not a check in Go. Two consequences worth naming:
//
//   - a booking that CANNOT be seated at all refuses the whole switch, naming the
//     booking. That is the actionable half: the venue adds a table (or moves that
//     one booking) and switches again. Silently leaving it unseated is what the
//     bug did, and quietly cancelling a confirmed guest is not ours to do.
//   - WAITLISTED bookings are placed too, exactly as 0057 gives them capacity
//     holds: they cost nothing while they wait (migration 0058 derives
//     booking_tables.active from the booking status, so their links land
//     inactive) and confirming one then goes through the same exclusion check as
//     everybody else. A waitlisted booking with no links at all would come back
//     from the waiting list confirmed and invisible to the constraint.
//
// The booking's capacity holds are dropped once it holds a table: the seats
// ledger no longer governs this venue, nothing maintains those rows any more (a
// later amendment in table mode moves the links, not the holds), and a stale
// hold reports occupancy at a time the guest is no longer expected. A switch
// back to seats mode rebuilds them from the bookings themselves.
func (u *policyUseCase) seatCapacityBookings(
	ctx context.Context,
	restaurantID uuid.UUID,
	policy domain.BookingPolicy,
	tables []domain.RestaurantTable,
) error {
	list, err := u.liveBookings(ctx, restaurantID, policy)
	if err != nil {
		return err
	}
	// Earliest first, ties broken by id: which table a party ends up at must not
	// depend on the page order of a listing. Placement order also decides
	// whether a tight table list fits at all, so it has to be reproducible — a
	// venue that retries the same switch must get the same answer.
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].StartsAt.Equal(list[j].StartsAt) {
			return list[i].StartsAt.Before(list[j].StartsAt)
		}
		return list[i].ID.String() < list[j].ID.String()
	})
	now := time.Now()
	for i := range list {
		b := &list[i]
		own, err := u.links.ListByBooking(ctx, b.ID)
		if err != nil {
			return err
		}
		if len(own) > 0 {
			// Already governed by the exclusion constraint — a leftover
			// placement from before the venue ever went table-less, or from an
			// earlier round of switching. Re-seating it would move a guest for
			// no reason.
			continue
		}
		from, to := occupancyWindow(b.StartsAt, policy)
		// Read inside the transaction and per booking, so the placements made a
		// moment ago in this same loop are part of "busy".
		busy, err := u.links.ListBusy(ctx, restaurantID, from, to)
		if err != nil {
			return err
		}
		picked := pickTables(freeTables(tables, busy, from, to), b.Guests)
		if len(picked) == 0 {
			return fmt.Errorf("%w: the booking of %d guests on %s (%s) cannot be seated at any free table; add tables that fit it, or move or cancel it, before switching to booking by tables",
				domain.ErrValidation, b.Guests, b.StartsAt.UTC().Format(time.RFC3339), b.ID)
		}
		links := make([]domain.BookingTable, 0, len(picked))
		for _, t := range picked {
			links = append(links, domain.BookingTable{
				ID: uuid.New(), BookingID: b.ID, TableID: t.ID,
				SlotStart: from, SlotEnd: to, CreatedAt: now,
			})
		}
		// The DB decides, as everywhere else in this package: if the slot was
		// taken meanwhile the exclusion constraint says so and the whole switch
		// rolls back.
		if err := u.links.Create(ctx, links); err != nil {
			return err
		}
		if err := u.capacity.ReplaceForBooking(ctx, b.ID, nil); err != nil {
			return err
		}
	}
	return nil
}

// liveBookings lists every booking of the venue that may still be holding space
// — the statuses that need holds, and only those recent enough to matter.
//
// A booking that STARTED before now but has not finished still occupies the
// room, so the walk has to reach backwards, not just forward. Listing from now
// (the filter means starts_at >= from) silently dropped every in-progress visit:
// switch a 40-seat venue to seats mode at 20:30 while a party of 30 is seated
// until 22:00, and the ledger reads empty for those buckets — the next party of
// 30 is accepted and the room is oversold by exactly the guests already sitting
// in it. Reaching back one full occupancy window (duration + both buffers, plus
// one bucket for the outward rounding) covers any booking whose window still
// touches the future; anything older cannot be holding a seat now.
//
// The whole set is loaded rather than streamed page by page, because the
// seats → tables direction has to place the bookings in a deterministic global
// order. It is bounded by one venue's live bookings inside its booking horizon,
// and this runs on an owner clicking a settings toggle, not on a request path.
func (u *policyUseCase) liveBookings(
	ctx context.Context,
	restaurantID uuid.UUID,
	policy domain.BookingPolicy,
) ([]domain.Booking, error) {
	lookback := policy.Duration + 2*policy.Buffer + domain.CapacityBucket
	from := time.Now().Add(-lookback)
	var out []domain.Booking
	for page := 1; ; page++ {
		list, total, err := u.bookings.List(ctx, domain.BookingFilter{
			RestaurantID: &restaurantID,
			Statuses:     statusesNeedingHolds(),
			From:         &from,
			Page:         page,
			PerPage:      capacityBackfillPage,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
		if len(list) == 0 || page*capacityBackfillPage >= total {
			return out, nil
		}
	}
}

// activeTables is the venue's bookable table list: active, and with a capacity
// worth seating somebody at. Same filter loadSchedule applies, kept here because
// this usecase needs the tables without the opening hours.
func (u *policyUseCase) activeTables(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantTable, error) {
	tables, err := u.schedule.ListTables(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RestaurantTable, 0, len(tables))
	for _, t := range tables {
		if t.IsActive && t.Capacity > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

// capacityBackfillPage is the page size of the mode-switch backfill. It matches
// the repository's own cap (100), so asking for more would silently return
// fewer rows than the loop expects.
const capacityBackfillPage = 100

// statusesNeedingHolds is the set of bookings the mode-switch backfill must give
// capacity holds to. It is StatusesHoldingTable PLUS waitlist, and the waitlist
// is the whole point: a waitlisted booking holds no seat TODAY, but confirming
// it re-claims one, and the re-claim is expressed as flipping its holds back to
// active. A booking with no hold rows at all flips nothing — it would come back
// from the waiting list confirmed and completely invisible to the ledger, which
// is exactly the hole review found.
//
// The holds written for a waitlisted booking cost nothing: migration 0057
// derives `active` from the booking's status inside the database, so they land
// inactive and only start counting when the booking is confirmed — under the
// venue's capacity as it stands at that moment.
func statusesNeedingHolds() []domain.BookingStatus {
	return append(domain.StatusesHoldingTable(), domain.BookingWaitlist)
}

func (u *policyUseCase) view(ctx context.Context, restaurantID uuid.UUID) (*PolicyView, error) {
	agg, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	return &PolicyView{
		Override:  agg.BookingPolicy,
		Effective: resolvePolicy(agg.Restaurant, u.cfg),
	}, nil
}

// validatePolicyOverride checks the fields the caller actually provided. Omitted
// (nil) fields are not validated: they are not being written.
func validatePolicyOverride(o domain.BookingPolicyOverride) error {
	if o.Timezone != nil {
		tz := strings.TrimSpace(*o.Timezone)
		if tz == "" {
			return fmt.Errorf("%w: timezone must not be empty", domain.ErrValidation)
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("%w: unknown timezone %q", domain.ErrValidation, tz)
		}
	}
	if m := o.BookingCapacityMode; m != nil && !m.Valid() {
		return fmt.Errorf("%w: booking_capacity_mode must be %q or %q",
			domain.ErrValidation, domain.CapacityModeTables, domain.CapacityModeSeats)
	}
	// The lower bound is 1, not 0: "we seat nobody" is not a configuration, it
	// is a venue that should be deactivated. The upper bound is deliberately
	// generous — see maxCapacitySeats — because its job is to catch a typo, not
	// to second-guess how big a banquet hall may be.
	if s := o.BookingCapacitySeats; s != nil && (*s < 1 || *s > maxCapacitySeats) {
		return fmt.Errorf("%w: booking_capacity_seats must be between 1 and %d",
			domain.ErrValidation, maxCapacitySeats)
	}
	checks := []struct {
		name     string
		val      *int
		min, max int
	}{
		{"booking_duration_minutes", o.BookingDurationMinutes, minPolicyDurationMinutes, maxPolicyDurationMinutes},
		{"booking_buffer_minutes", o.BookingBufferMinutes, 0, maxPolicyBufferMinutes},
		{"booking_lead_minutes", o.BookingLeadMinutes, 0, maxPolicyLeadMinutes},
		{"booking_horizon_days", o.BookingHorizonDays, minPolicyHorizonDays, maxPolicyHorizonDays},
		{"cancel_deadline_minutes", o.CancelDeadlineMinutes, 0, maxPolicyCancelMinutes},
		{"confirm_sla_minutes", o.ConfirmSLAMinutes, minPolicySLAMinutes, maxPolicySLAMinutes},
		{"max_guests_per_booking", o.MaxGuestsPerBooking, minPolicyMaxGuests, maxPolicyMaxGuests},
	}
	for _, c := range checks {
		if c.val == nil {
			continue
		}
		if *c.val < c.min || *c.val > c.max {
			return fmt.Errorf("%w: %s must be between %d and %d", domain.ErrValidation, c.name, c.min, c.max)
		}
	}
	return nil
}

// normalizePolicyOverride trims the timezone so a stray space can't produce a
// value that validates but fails time.LoadLocation on read.
func normalizePolicyOverride(o domain.BookingPolicyOverride) domain.BookingPolicyOverride {
	if o.Timezone != nil {
		tz := strings.TrimSpace(*o.Timezone)
		o.Timezone = &tz
	}
	return o
}
