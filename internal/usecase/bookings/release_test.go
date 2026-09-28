package bookings

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeReleaseRepo is the minimal domain.BookingReleaseRepository double this
// package's fakes.go does not already carry (release.go is the only user).
type fakeReleaseRepo struct {
	ok  bool
	err error
}

func (f *fakeReleaseRepo) Release(_ context.Context, _ uuid.UUID, _ time.Time) (bool, error) {
	return f.ok, f.err
}

func (f *fakeReleaseRepo) ClaimUnpaidHidden(_ context.Context, _ time.Time, _ int) ([]domain.Booking, error) {
	return nil, nil
}

// fakeHoldNotice is the PreorderHoldNotice double: held reports whether a live
// pre-order hold exists, amount is what it returns when it does.
type fakeHoldNotice struct {
	held   bool
	amount domain.Money
	err    error
}

func (f *fakeHoldNotice) LivePreorderHold(_ context.Context, _ uuid.UUID) (domain.Money, bool, error) {
	return f.amount, f.held, f.err
}

// releasePayload is the subset of bookingPayload this file asserts on,
// decoded straight from the outbox row JSON — the same contract
// notifications.outboxPayload decodes on the consumer side.
type releasePayload struct {
	HoldAmountMinor       *int64          `json:"hold_amount_minor"`
	HoldCurrency          domain.Currency `json:"hold_currency"`
	VenueAnswerDeadlineAt *time.Time      `json:"venue_answer_deadline_at"`
}

func decodeReleasePayload(t *testing.T, raw []byte) releasePayload {
	t.Helper()
	var p releasePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	return p
}

func newTestReleaser(t *testing.T, agg *domain.RestaurantAggregate, holds PreorderHoldNotice, opts ...ReleaserOption) (
	*releaser, *fakeBookings, *fakeReleaseRepo, *fakeOutbox, uuid.UUID,
) {
	t.Helper()
	bookingID := uuid.New()
	b := &domain.Booking{
		ID:           bookingID,
		RestaurantID: uuid.New(),
		Status:       domain.BookingPending,
		StartsAt:     time.Now().Add(3 * time.Hour),
		EndsAt:       time.Now().Add(5 * time.Hour),
		Guests:       2,
	}
	bookings := newFakeBookings(b)
	release := &fakeReleaseRepo{ok: true}
	outbox := &fakeOutbox{}
	history := &fakeHistory{}
	rests := &fakeRestaurants{agg: agg}

	allOpts := opts
	if holds != nil {
		allOpts = append([]ReleaserOption{WithReleaserVenueNotice(holds, 24 * time.Hour)}, opts...)
	}
	r := NewReleaser(bookings, release, history, outbox, rests, Config{}, allOpts...).(*releaser)
	return r, bookings, release, outbox, bookingID
}

// TestReleaseForPayment_HoldNoticeCarriesAmountAndDeadline is the guard for
// spec preorder-hold-capture-on-confirm-20260924 §criterion 25: the outbox
// event that hands a held pre-order booking to the venue must carry the hold's
// amount and the D_venue deadline, so the venue notification can quote both.
func TestReleaseForPayment_HoldNoticeCarriesAmountAndDeadline(t *testing.T) {
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{}} // ConfirmOnCreate default false
	holds := &fakeHoldNotice{held: true, amount: domain.Money{AmountMinor: 966000, Currency: domain.CurrencyKZT}}
	r, _, _, outbox, bookingID := newTestReleaser(t, agg, holds)

	before := time.Now()
	if err := r.ReleaseForPayment(context.Background(), bookingID); err != nil {
		t.Fatalf("ReleaseForPayment: %v", err)
	}

	if len(outbox.created) != 1 {
		t.Fatalf("want exactly one outbox event, got %d", len(outbox.created))
	}
	ev := outbox.created[0]
	if ev.EventType != domain.EventBookingCreated {
		t.Fatalf("event type = %s, want booking.created", ev.EventType)
	}
	p := decodeReleasePayload(t, ev.Payload)
	if p.HoldAmountMinor == nil || *p.HoldAmountMinor != 966000 {
		t.Fatalf("hold_amount_minor = %v, want 966000", p.HoldAmountMinor)
	}
	if p.HoldCurrency != domain.CurrencyKZT {
		t.Fatalf("hold_currency = %q, want KZT", p.HoldCurrency)
	}
	if p.VenueAnswerDeadlineAt == nil {
		t.Fatal("venue_answer_deadline_at missing")
	}
	// releasedAt is `now` inside ReleaseForPayment (not injectable); the
	// binding constraint here is DefaultConfirmSLA (2h, see ports.go), well
	// inside the booking's own start time and the 24h confirmMax passed above.
	// Assert closeness rather than equality to tolerate the wall-clock skew
	// between `before` and the call's own `time.Now()`.
	wantAround := before.Add(defaultConfirmSLA)
	if diff := p.VenueAnswerDeadlineAt.Sub(wantAround); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("venue_answer_deadline_at = %s, want close to %s (diff %s)",
			p.VenueAnswerDeadlineAt, wantAround, diff)
	}
}

// TestReleaseForPayment_OmitsHoldNoticeWhenConfirmOnCreate: a venue that
// auto-confirms on create never has to "answer" the venue's own booking, so
// there is no deadline to quote — even though the pre-order hold is live.
func TestReleaseForPayment_OmitsHoldNoticeWhenConfirmOnCreate(t *testing.T) {
	confirmOnCreate := true
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		BookingPolicy: domain.BookingPolicyOverride{ConfirmOnCreate: &confirmOnCreate},
	}}
	holds := &fakeHoldNotice{held: true, amount: domain.Money{AmountMinor: 500000, Currency: domain.CurrencyKZT}}
	r, bookings, _, outbox, bookingID := newTestReleaser(t, agg, holds)

	if err := r.ReleaseForPayment(context.Background(), bookingID); err != nil {
		t.Fatalf("ReleaseForPayment: %v", err)
	}

	if len(outbox.created) != 2 {
		t.Fatalf("want booking.created + booking.confirmed, got %d events", len(outbox.created))
	}
	p := decodeReleasePayload(t, outbox.created[0].Payload)
	if p.HoldAmountMinor != nil || p.VenueAnswerDeadlineAt != nil {
		t.Fatalf("confirm-on-create booking must carry no hold notice, got %+v", p)
	}
	// The existing auto-confirm behaviour must survive the refactor untouched.
	if bookings.byID[bookingID].Status != domain.BookingConfirmed {
		t.Fatalf("status = %s, want confirmed", bookings.byID[bookingID].Status)
	}
}

// TestReleaseForPayment_OmitsHoldNoticeWhenNoLiveHold: an ordinary booking (no
// pre-order, or one whose payment is not a live authorized hold) gets the
// plain booking.created event it always got — no money line invented.
func TestReleaseForPayment_OmitsHoldNoticeWhenNoLiveHold(t *testing.T) {
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{}}
	holds := &fakeHoldNotice{held: false}
	r, _, _, outbox, bookingID := newTestReleaser(t, agg, holds)

	if err := r.ReleaseForPayment(context.Background(), bookingID); err != nil {
		t.Fatalf("ReleaseForPayment: %v", err)
	}
	p := decodeReleasePayload(t, outbox.created[0].Payload)
	if p.HoldAmountMinor != nil || p.VenueAnswerDeadlineAt != nil {
		t.Fatalf("booking with no live hold must carry no hold notice, got %+v", p)
	}
}

// TestReleaseForPayment_NilHoldsOptionIsSafe: a Releaser built without
// WithReleaserVenueNotice (every call site before this change, and any future
// one that skips it) must behave exactly as before — no panic, no hold notice.
func TestReleaseForPayment_NilHoldsOptionIsSafe(t *testing.T) {
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{}}
	r, _, _, outbox, bookingID := newTestReleaser(t, agg, nil)

	if err := r.ReleaseForPayment(context.Background(), bookingID); err != nil {
		t.Fatalf("ReleaseForPayment: %v", err)
	}
	p := decodeReleasePayload(t, outbox.created[0].Payload)
	if p.HoldAmountMinor != nil || p.VenueAnswerDeadlineAt != nil {
		t.Fatalf("nil holds reader must carry no hold notice, got %+v", p)
	}
}
