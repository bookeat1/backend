package bookings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
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
	if err := u.applyCapacityChange(ctx, actor, restaurantID, in); err != nil {
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
//
// This wrapper only does the logging: the switch is the single most consequential
// thing the venue cabinet can do to existing reservations (it rewrites holds or
// seats parties at tables), and until now it left no trace at all. The success
// line is emitted AFTER WithinTx has returned, i.e. after the commit — a line
// written inside the transaction would survive a rollback and claim a switch
// that never happened.
func (u *policyUseCase) applyCapacityChange(ctx context.Context, actor Actor, restaurantID uuid.UUID, in domain.BookingPolicyOverride) error {
	started := time.Now()
	rep, err := u.capacityChange(ctx, restaurantID, in)
	fields := []any{
		slog.String("restaurant_id", restaurantID.String()),
		slog.String("actor_id", actor.UserID.String()),
		slog.String("from_mode", string(rep.fromMode)),
		slog.String("to_mode", string(rep.toMode)),
		slog.Int("seats_before", rep.seatsBefore),
		slog.Int("seats_after", rep.seatsAfter),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	log := logging.FromContext(ctx)
	if err != nil {
		// A lost race is NOT "that already exists". Every ErrAlreadyExists that
		// escapes capacityChange means the same thing — a concurrent writer got
		// there first and the switch was rolled back WHOLE, so nothing changed and
		// a retry will almost certainly work. The two "does not fit" refusals are
		// ErrValidation with their own message and are untouched by this.
		//
		// The 0059 deferred trigger is what makes this reachable: it refuses to
		// commit occupancy for a booking whose status another transaction is
		// changing. Left as a bare 409 it reaches the venue cabinet as "already
		// exists", i.e. exactly the ambiguity that told a guest about a booking
		// which never existed — one floor up, in the staff UI. The reason was
		// already in the log; this puts it on the wire, as a code rather than as
		// text nobody may branch on.
		if _, coded := domain.CodeOf(err); !coded && errors.Is(err, domain.ErrAlreadyExists) {
			err = domain.WithCode(domain.CodeCapacitySwitchConflict, err)
		}
		// Warn, not Error: most refusals are the venue being told its own data
		// does not allow the change. The reason is the message staff saw, so the
		// log answers "why did the toggle not work" without a reproduction; the
		// code is logged beside it so a support question can be matched to what
		// the client actually received.
		refused := append(fields, slog.String("reason", err.Error()))
		if code, coded := domain.CodeOf(err); coded {
			refused = append(refused, slog.String("code", string(code)))
		}
		log.Warn(logging.EventVenueCapacityModeRefused, refused...)
		return err
	}
	log.Warn(logging.EventVenueCapacityModeChanged,
		append(fields, slog.Int("bookings_reconciled", rep.reconciled))...)
	return nil
}

// capacityChangeReport is what the log line is built from. It is filled as the
// change proceeds and returned even on failure, so a refusal is logged with the
// state it was refused in rather than with blanks.
type capacityChangeReport struct {
	fromMode, toMode        domain.CapacityMode
	seatsBefore, seatsAfter int
	reconciled              int
}

func (u *policyUseCase) capacityChange(ctx context.Context, restaurantID uuid.UUID, in domain.BookingPolicyOverride) (capacityChangeReport, error) {
	var rep capacityChangeReport
	// Pre-filled from the request so a failure that happens before the venue is
	// even read still logs what was being asked for.
	if in.BookingCapacityMode != nil {
		rep.toMode = *in.BookingCapacityMode
	}
	if in.BookingCapacitySeats != nil {
		rep.seatsAfter = *in.BookingCapacitySeats
	}
	if u.capacity == nil || u.links == nil || u.bookings == nil || u.tx == nil || u.schedule == nil {
		return rep, fmt.Errorf("%w: capacity mode is not configured on this deployment", domain.ErrValidation)
	}
	agg, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return rep, err
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
	rep.fromMode, rep.toMode = current.CapacityMode, target.CapacityMode
	rep.seatsBefore, rep.seatsAfter = current.CapacitySeats, target.CapacitySeats
	if target.CapacityMode == domain.CapacityModeSeats && target.CapacitySeats <= 0 {
		return rep, fmt.Errorf("%w: booking_capacity_seats is required to book by total capacity",
			domain.ErrValidation)
	}

	if target.CapacityMode == domain.CapacityModeTables {
		tables, err := u.activeTables(ctx, restaurantID)
		if err != nil {
			return rep, err
		}
		if len(tables) == 0 {
			return rep, fmt.Errorf("%w: this restaurant has no active tables, so booking by tables would make every slot unbookable",
				domain.ErrValidation)
		}
		// The count is written into rep from inside the closure, and rep is
		// returned by a statement of its own afterwards: `return rep, WithinTx(…)`
		// would evaluate rep BEFORE the closure runs and always log zero.
		err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
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
			n, err := u.seatCapacityBookings(ctx, restaurantID, target, tables)
			rep.reconciled = n
			return err
		})
		if err != nil {
			// A rolled-back switch reconciled nothing, whatever the loop had
			// managed before it failed.
			rep.reconciled = 0
		}
		return rep, err
	}

	// Lowering the capacity: refuse while the venue has more guests already
	// booked for a future moment than the new number allows. Checked before the
	// write for a readable error; the bucket CHECK would refuse it anyway.
	if peak, err := u.capacity.PeakTaken(ctx, restaurantID, time.Now()); err != nil {
		return rep, err
	} else if peak != nil && peak.SeatsTaken > target.CapacitySeats {
		return rep, fmt.Errorf("%w: %d guests are already booked for %s; capacity cannot be set below that",
			domain.ErrValidation, peak.SeatsTaken, peak.BucketStart.UTC().Format(time.RFC3339))
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Same per-venue lock a booking create takes, and taken FIRST: the
		// policy write and the holds rebuilt from it must not interleave with a
		// create that read the previous policy.
		if err := u.capacity.LockVenue(ctx, restaurantID); err != nil {
			return err
		}
		if err := u.policies.UpdateBookingPolicy(ctx, restaurantID, in); err != nil {
			return err
		}
		n, err := u.rebuildHolds(ctx, restaurantID, target)
		rep.reconciled = n
		return err
	})
	if err != nil {
		rep.reconciled = 0
	}
	return rep, err
}

// rebuildHolds (re)writes the capacity holds of every future booking that still
// holds a seat, so the bucket counters describe the venue's real load under the
// declared capacity. ReplaceForBooking rather than Create: a venue that has
// switched modes back and forth must not trip over the holds of the previous
// round.
//
// A booking that already owns real booking_tables links gets holds too, and
// keeps those links. That is deliberate, and it is NOT the mirror of what
// seatCapacityBookings does in the other direction: in seats mode the table
// engine is not the authority — availabilityUseCase.Day takes the seats branch
// and reads restaurant_capacity_buckets only, and create.go's seats path never
// calls ListBusy — so a booking with a link but no hold would be invisible and
// its seats sold a second time. The leftover links are inert while the venue is
// table-less (nothing in seats mode reads them); seatCapacityBookings is what
// clears the holds on the way back.
//
// It returns how many bookings it rewrote, for the log line.
func (u *policyUseCase) rebuildHolds(ctx context.Context, restaurantID uuid.UUID, policy domain.BookingPolicy) (int, error) {
	list, err := u.liveBookings(ctx, restaurantID, policy)
	if err != nil {
		return 0, err
	}
	for i := range list {
		b := list[i]
		// Both refusals below name the booking, not just its time: a venue with
		// two parties in the same slot cannot act on "a booking of 6 guests at
		// 19:00" — see the mirror message in seatCapacityBookings.
		if b.Guests > policy.CapacitySeats {
			return 0, fmt.Errorf("%w: the booking of %d guests on %s (%s) does not fit a capacity of %d",
				domain.ErrValidation, b.Guests, b.StartsAt.UTC().Format(time.RFC3339), b.ID, policy.CapacitySeats)
		}
		holds := buildCapacityHolds(&b, policy, time.Now())
		if err := u.capacity.ReplaceForBooking(ctx, b.ID, holds); err != nil {
			if errors.Is(err, domain.ErrAlreadyExists) {
				// The bucket CHECK refused THIS booking's holds, so the offending
				// booking is the one in hand and the overloaded moment lies inside
				// its own occupancy window. Both are free to report — no extra
				// query — and without them staff are told a capacity is too small
				// but not for which reservation.
				from, to := occupancyWindow(b.StartsAt, policy)
				return 0, fmt.Errorf("%w: the booking of %d guests on %s (%s) does not fit a capacity of %d — the bookings already accepted between %s and %s fill it; cancel or move a booking in that window, or set a higher capacity",
					domain.ErrValidation, b.Guests, b.StartsAt.UTC().Format(time.RFC3339), b.ID,
					policy.CapacitySeats, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
			}
			return 0, err
		}
	}
	return len(list), nil
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
// The booking's capacity holds are dropped once it holds a table — whether this
// switch seated it or it arrived already placed: the seats
// ledger no longer governs this venue, nothing maintains those rows any more (a
// later amendment in table mode moves the links, not the holds), and a stale
// hold reports occupancy at a time the guest is no longer expected. A switch
// back to seats mode rebuilds them from the bookings themselves.
func (u *policyUseCase) seatCapacityBookings(
	ctx context.Context,
	restaurantID uuid.UUID,
	policy domain.BookingPolicy,
	tables []domain.RestaurantTable,
) (int, error) {
	list, err := u.liveBookings(ctx, restaurantID, policy)
	if err != nil {
		return 0, err
	}
	// Earliest first, ties broken by id: which table a party ends up at must not
	// depend on the order a listing happened to return. Placement order also
	// decides whether a tight table list fits at all, so it has to be
	// reproducible — a venue that retries the same switch must get the same
	// answer. liveBookings already reads in exactly this order; the sort is kept
	// so the guarantee belongs to this function and not to an ORDER BY somebody
	// may later "optimise" a layer away.
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
			return 0, err
		}
		if len(own) > 0 {
			// Already governed by the exclusion constraint — a leftover
			// placement from before the venue ever went table-less, or from an
			// earlier round of switching. Re-seating it would move a guest for
			// no reason.
			//
			// The capacity side still has to be reconciled, and skipping it here
			// was a leak (re-review of the previous commit): a booking created in
			// TABLES mode keeps its real link through a tables → seats switch
			// (rebuildHolds deliberately does not move it — in seats mode the
			// ledger is the sole authority and those guests must still occupy
			// seats), so on the way back it arrives with BOTH a link and holds.
			// Left alone, its holds outlive the mode that maintained them and the
			// buckets report occupancy nobody is taking.
			if err := u.capacity.ReplaceForBooking(ctx, b.ID, nil); err != nil {
				return 0, err
			}
			continue
		}
		from, to := occupancyWindow(b.StartsAt, policy)
		// Read inside the transaction and per booking, so the placements made a
		// moment ago in this same loop are part of "busy".
		busy, err := u.links.ListBusy(ctx, restaurantID, from, to)
		if err != nil {
			return 0, err
		}
		picked := pickTables(freeTables(tables, busy, from, to), b.Guests)
		if len(picked) == 0 {
			return 0, fmt.Errorf("%w: the booking of %d guests on %s (%s) cannot be seated at any free table; add tables that fit it, or move or cancel it, before switching to booking by tables",
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
			return 0, err
		}
		if err := u.capacity.ReplaceForBooking(ctx, b.ID, nil); err != nil {
			return 0, err
		}
	}
	return len(list), nil
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
// The whole set is read by ONE statement — never paged. Two reasons, and the
// first one is a correctness bug this replaced (review finding, 25.07.2026):
//
//   - the enclosing transaction is READ COMMITTED (sqltx.Manager takes no
//     TxOptions), so each statement gets a fresh snapshot. The old loop asked
//     for `ORDER BY starts_at DESC, id LIMIT 100 OFFSET n`, and LockVenue does
//     not serialize STATUS changes — status.go never takes it. So a guest
//     cancelling a booking that sat on an earlier page, while a venue with more
//     than 100 live bookings switched mode, shifted the offset window by one and
//     exactly one booking was never returned. That booking then came out of the
//     switch with no capacity holds (seats mode: invisible to the ledger) or no
//     booking_tables row (tables mode: invisible to the GiST constraint), and
//     its seats were sold a second time. One snapshot cannot skip a row.
//   - the seats → tables direction has to place the bookings in a deterministic
//     global order, which a set assembled from independent snapshots does not
//     have either.
//
// The price of a single statement is that the set must be bounded, hence the
// hard limit: a venue that reaches it is refused loudly instead of quietly
// running past the 15s write timeout with the venue lock in hand.
func (u *policyUseCase) liveBookings(
	ctx context.Context,
	restaurantID uuid.UUID,
	policy domain.BookingPolicy,
) ([]domain.Booking, error) {
	lookback := policy.Duration + 2*policy.Buffer + domain.CapacityBucket
	from := time.Now().Add(-lookback)
	// Asks for ONE ROW MORE than may be processed: with a plain cap, "exactly
	// MaxReconcileBookings rows" cannot be told apart from "the set is
	// truncated", the caller must assume the unsafe reading, and a venue sitting
	// on exactly 300 live bookings is locked out of switching modes for no real
	// reason. See domain.ReconcileProbeLimit.
	out, err := u.bookings.ListLiveForReconcile(
		ctx, restaurantID, from, statusesNeedingHolds(), domain.ReconcileProbeLimit)
	if err != nil {
		return nil, err
	}
	if len(out) > domain.MaxReconcileBookings {
		// The probe row came back, so the set really is truncated and nothing
		// about it can be trusted — refuse before touching a single hold or link.
		//
		// Coded, because a venue cabinet must not show this as a generic conflict:
		// unlike a lost race it does NOT resolve by retrying, it needs fewer live
		// bookings in the window (or support), and staff can only be told that if
		// the client can recognise the case.
		return nil, domain.WithCode(domain.CodeCapacitySwitchTooManyBookings,
			fmt.Errorf("%w: this restaurant has more than %d live bookings in the affected period, and a capacity-mode change can only be reconciled for up to %d at once; nothing was changed — please contact support to have the change applied for a venue this size",
				domain.ErrValidation, domain.MaxReconcileBookings, domain.MaxReconcileBookings))
	}
	return out, nil
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
	// The venue's zone is validated in the DOMAIN, by the same rule every
	// money-path reader resolves it with: an empty string, the server's "Local",
	// an abbreviation ("KZT", "EST") or a name this host does not know is not
	// storable. The column behind it is a plain varchar with no CHECK, so this
	// is the only gate there is.
	if o.Timezone != nil {
		if _, err := domain.NormalizeVenueTimezone(*o.Timezone); err != nil {
			return err
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
