package bookings

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// BookingDetails is a booking with the collections the detail screen needs.
type BookingDetails struct {
	Booking domain.Booking
	Items   []domain.BookingItem
	Tables  []domain.BookingTable
	// FreeCancelDeadline is the absolute moment free cancellation ends for this
	// booking: starts_at − the venue's free_cancel_window_minutes (the SAME
	// per-restaurant window the money path uses — single source of truth). The
	// client renders a live countdown from it. It is nil for a booking that can
	// no longer be cancelled (a terminal status); when the deadline has already
	// PASSED it is still returned (non-nil) so the app can show the "paid
	// cancellation" state — the client compares it to now, this layer does not
	// null it out. See the facade Get doc for the free-booking judgement.
	FreeCancelDeadline *time.Time
}

// freeCancelDeadlineResolver derives starts_at − free_cancel_window_minutes for
// a booking. Bound in bootstrap to the SAME adapter the payment settlement uses
// (payments.FreeCancelDeadlineFor over restaurants.free_cancel_window_minutes),
// so the countdown the guest sees and the money decision can never disagree.
type freeCancelDeadlineResolver interface {
	CancelDeadlineFor(ctx context.Context, booking domain.Booking) (time.Time, error)
}

// Facade exposes booking reads plus the chat and survey side-channels.
// Every method resolves the caller's access to the booking (or the venue)
// before reading anything — there is no implicit allow.
type Facade interface {
	Get(ctx context.Context, actor Actor, id uuid.UUID) (*BookingDetails, error)
	ListMine(ctx context.Context, actor Actor, f domain.BookingFilter) ([]domain.Booking, int, error)
	ListByRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, f domain.BookingFilter) ([]domain.Booking, int, error)
	History(ctx context.Context, actor Actor, bookingID uuid.UUID) ([]domain.BookingStatusChange, error)

	Messages(ctx context.Context, actor Actor, bookingID uuid.UUID) ([]domain.BookingMessage, error)
	PostMessage(ctx context.Context, actor Actor, bookingID uuid.UUID, text string) (*domain.BookingMessage, error)
	MarkMessagesRead(ctx context.Context, actor Actor, bookingID uuid.UUID) (int, error)

	Survey(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.RestaurantSurvey, error)
	SubmitSurvey(ctx context.Context, actor Actor, bookingID uuid.UUID, in SurveyInput) (*domain.RestaurantSurvey, error)
}

// SurveyInput is the post-visit questionnaire submitted by a guest.
type SurveyInput struct {
	RatingOverall  int
	RatingFood     int
	RatingService  int
	RatingAmbience int
	NPS            int
	Comment        *string
	Dismissed      bool
}

type facade struct {
	bookings   domain.BookingRepository
	links      domain.BookingTableRepository
	items      domain.BookingItemRepository
	messages   domain.BookingMessageRepository
	surveys    domain.RestaurantSurveyRepository
	history    domain.BookingStatusHistoryRepository
	outbox     domain.BookingOutboxRepository
	managers   managerChecker
	tx         domain.TxManager
	freeCancel freeCancelDeadlineResolver
}

// FacadeOption configures optional facade dependencies without breaking the
// constructor's existing positional callers (tests pass none).
type FacadeOption func(*facade)

// WithFreeCancelDeadlineResolver wires the per-restaurant free-cancel deadline
// used to populate BookingDetails.FreeCancelDeadline. Left nil in tests / when
// payments are not wired, in which case the field is simply omitted (nil).
func WithFreeCancelDeadlineResolver(r freeCancelDeadlineResolver) FacadeOption {
	return func(f *facade) { f.freeCancel = r }
}

// NewFacade constructs the bookings Facade.
func NewFacade(
	bookings domain.BookingRepository,
	links domain.BookingTableRepository,
	items domain.BookingItemRepository,
	messages domain.BookingMessageRepository,
	surveys domain.RestaurantSurveyRepository,
	history domain.BookingStatusHistoryRepository,
	outbox domain.BookingOutboxRepository,
	managers managerChecker,
	tx domain.TxManager,
	opts ...FacadeOption,
) Facade {
	f := &facade{
		bookings: bookings, links: links, items: items, messages: messages,
		surveys: surveys, history: history, outbox: outbox, managers: managers, tx: tx,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func (f *facade) Get(ctx context.Context, actor Actor, id uuid.UUID) (*BookingDetails, error) {
	b, _, err := f.load(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	out := &BookingDetails{Booking: *b}
	if out.Items, err = f.items.ListByBooking(ctx, id); err != nil {
		return nil, err
	}
	if out.Tables, err = f.links.ListByBooking(ctx, id); err != nil {
		return nil, err
	}
	out.FreeCancelDeadline = f.freeCancelDeadline(ctx, b)
	return out, nil
}

// freeCancelDeadline computes the booking's free-cancellation deadline for the
// client countdown, or nil.
//
// It is returned only for a booking that can still be cancelled (a pending /
// confirmed / waitlisted booking); a terminal booking (arrived, completed,
// cancelled, no-show, rejected) is past cancellation, so the countdown is
// meaningless and the field is omitted.
//
// JUDGEMENT (free vs paid booking): a booking with NO prepayment has no money
// at stake, so the deadline is arguably irrelevant there. This facade does not
// carry the payment repository, and adding it only to null out a timestamp
// would couple the read path to payments for no functional gain — so the
// deadline IS returned for any active booking, and the client (which already
// knows the booking's payment state) renders the countdown only when money is
// at stake. Documented rather than silently coupled.
//
// A resolver error is swallowed (nil) on purpose: a booking read must not fail
// because a per-restaurant policy lookup hiccuped; the countdown is auxiliary.
func (f *facade) freeCancelDeadline(ctx context.Context, b *domain.Booking) *time.Time {
	if f.freeCancel == nil {
		return nil
	}
	switch b.Status {
	case domain.BookingPending, domain.BookingConfirmed, domain.BookingWaitlist:
	default:
		return nil
	}
	deadline, err := f.freeCancel.CancelDeadlineFor(ctx, *b)
	if err != nil {
		return nil
	}
	return &deadline
}

// ListMine returns the caller's own bookings. The user filter is overwritten
// with the actor's id on purpose: a client-supplied user_id must never widen
// the result set.
func (f *facade) ListMine(ctx context.Context, actor Actor, flt domain.BookingFilter) ([]domain.Booking, int, error) {
	if actor.UserID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if err := validateFilter(flt); err != nil {
		return nil, 0, err
	}
	uid := actor.UserID
	flt.UserID = &uid
	flt.RestaurantID = nil
	return f.bookings.List(ctx, flt)
}

// ListByRestaurant is the venue calendar: managers of that restaurant and
// admins only. The restaurant filter is pinned to the route parameter.
func (f *facade) ListByRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, flt domain.BookingFilter) ([]domain.Booking, int, error) {
	if _, err := requireStaff(ctx, f.managers, actor, restaurantID); err != nil {
		return nil, 0, err
	}
	if err := validateFilter(flt); err != nil {
		return nil, 0, err
	}
	rid := restaurantID
	flt.RestaurantID = &rid
	flt.UserID = nil
	return f.bookings.List(ctx, flt)
}

func (f *facade) History(ctx context.Context, actor Actor, bookingID uuid.UUID) ([]domain.BookingStatusChange, error) {
	if _, _, err := f.load(ctx, actor, bookingID); err != nil {
		return nil, err
	}
	return f.history.ListByBooking(ctx, bookingID)
}

func (f *facade) Messages(ctx context.Context, actor Actor, bookingID uuid.UUID) ([]domain.BookingMessage, error) {
	if _, _, err := f.load(ctx, actor, bookingID); err != nil {
		return nil, err
	}
	return f.messages.ListByBooking(ctx, bookingID)
}

// PostMessage appends to the booking thread. The sender type is derived from
// the caller's relation to the booking, never from the request body.
func (f *facade) PostMessage(ctx context.Context, actor Actor, bookingID uuid.UUID, text string) (*domain.BookingMessage, error) {
	b, acc, err := f.load(ctx, actor, bookingID)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: message required", domain.ErrValidation)
	}
	now := time.Now()
	m := &domain.BookingMessage{
		ID: uuid.New(), BookingID: bookingID, SenderType: acc.senderType(),
		SenderID: actorID(actor), Message: text, CreatedAt: now,
	}
	err = f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.messages.Create(ctx, m); err != nil {
			return err
		}
		return publish(ctx, f.outbox, b, domain.EventBookingMessage, now)
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (f *facade) MarkMessagesRead(ctx context.Context, actor Actor, bookingID uuid.UUID) (int, error) {
	_, acc, err := f.load(ctx, actor, bookingID)
	if err != nil {
		return 0, err
	}
	return f.messages.MarkRead(ctx, bookingID, acc.senderType(), time.Now())
}

func (f *facade) Survey(ctx context.Context, actor Actor, bookingID uuid.UUID) (*domain.RestaurantSurvey, error) {
	if _, _, err := f.load(ctx, actor, bookingID); err != nil {
		return nil, err
	}
	return f.surveys.GetByBooking(ctx, bookingID)
}

// SubmitSurvey stores the post-visit questionnaire. Only the guest who owns the
// booking may rate it, and only once the visit actually happened.
func (f *facade) SubmitSurvey(ctx context.Context, actor Actor, bookingID uuid.UUID, in SurveyInput) (*domain.RestaurantSurvey, error) {
	b, acc, err := f.load(ctx, actor, bookingID)
	if err != nil {
		return nil, err
	}
	if !acc.owner {
		return nil, fmt.Errorf("%w: only the guest can rate a visit", domain.ErrForbidden)
	}
	if b.Status != domain.BookingCompleted && b.Status != domain.BookingArrived {
		return nil, fmt.Errorf("%w: the visit has not happened yet", domain.ErrInvalidStatus)
	}
	if err := validateSurvey(in); err != nil {
		return nil, err
	}
	s := &domain.RestaurantSurvey{
		ID: uuid.New(), BookingID: &bookingID, RestaurantID: b.RestaurantID,
		UserID: actor.UserID, RatingOverall: in.RatingOverall, RatingFood: in.RatingFood,
		RatingService: in.RatingService, RatingAmbience: in.RatingAmbience, NPS: in.NPS,
		Comment: in.Comment, Dismissed: in.Dismissed, CreatedAt: time.Now(),
	}
	if err := f.surveys.Create(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// load fetches a booking and resolves the caller's access to it. Every facade
// method starts here.
func (f *facade) load(ctx context.Context, actor Actor, id uuid.UUID) (*domain.Booking, access, error) {
	b, err := f.bookings.GetByID(ctx, id)
	if err != nil {
		return nil, access{}, err
	}
	acc, err := authorize(ctx, f.managers, actor, b)
	if err != nil {
		return nil, access{}, err
	}
	return b, acc, nil
}

func validateFilter(f domain.BookingFilter) error {
	for _, s := range f.Statuses {
		if !s.Valid() {
			return fmt.Errorf("%w: unknown status %q", domain.ErrValidation, s)
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return fmt.Errorf("%w: 'from' must be before 'to'", domain.ErrValidation)
	}
	return nil
}

func validateSurvey(in SurveyInput) error {
	if !domain.ValidRating(in.RatingOverall) || !domain.ValidRating(in.RatingFood) ||
		!domain.ValidRating(in.RatingService) || !domain.ValidRating(in.RatingAmbience) {
		return fmt.Errorf("%w: ratings must be between 1 and 5", domain.ErrValidation)
	}
	if !domain.ValidNPS(in.NPS) {
		return fmt.Errorf("%w: nps must be between 0 and 10", domain.ErrValidation)
	}
	return nil
}
