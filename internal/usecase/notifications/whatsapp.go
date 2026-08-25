package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// WhatsAppSender delivers ONE approved WhatsApp template to a phone and reports
// Meta's HTTP status. It is the seam that keeps the Cloud API (and the network)
// out of the notifier, so the fan-out / dedupe / consent behaviour below is
// unit-testable without a business number.
//
// params fill the template's {{1}}…{{n}} in order; which template that is
// belongs to the sender, not here — Meta requires a brand-new template for any
// wording change, so the notifier must not name one.
type WhatsAppSender func(ctx context.Context, phone string, params []string) (statusCode int, err error)

// venueTimezoneReader resolves the IANA zone a venue's own clock runs in. A
// minimal local port (bound to postgres/notification.Venues in bootstrap): the
// notifier needs one string per event, not the restaurant aggregate.
type venueTimezoneReader interface {
	Timezone(ctx context.Context, restaurantID uuid.UUID) (string, error)
}

// venueStaffReader lists a venue's staff roster. The WhatsApp recipients are
// read from it rather than from a channel-specific table because the consent
// and the number are a property of a PERSON on the roster: `whatsapp_opt_in`
// and `whatsapp_phone` have lived on restaurant_managers since migration 0002.
type venueStaffReader interface {
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantManager, error)
}

// WhatsAppNotifier is the venue-facing WhatsApp channel: on a NEW booking it
// sends the approved «новая бронь» template to every staff member of THAT
// booking's venue who opted in and left a number, and to nobody else.
//
// It is the fifth notifier on the same dispatcher, the same booking outbox and
// the same notification_deliveries ledger as web push / Telegram / guest push /
// the in-app feed. There is no second delivery mechanism and, crucially, no
// send on the booking-creation path: the guest's request returns as soon as the
// booking and its outbox row are committed, and the WhatsApp message leaves
// later from the worker. A dead Meta endpoint can therefore never fail, slow or
// roll back a booking.
//
// Consent is per PERSON, not per venue: a manager's WhatsApp number may be
// their personal one, so an alert goes only to a staff row with
// whatsapp_opt_in = true AND a number. No opted-in staff = nothing to do and
// NOT an error — the event drains.
type WhatsAppNotifier struct {
	staff      venueStaffReader
	settings   domain.RestaurantNotificationSettingsRepository
	deliveries domain.NotificationDeliveryRepository
	zones      venueTimezoneReader
	send       WhatsAppSender
	fallback   *time.Location
	enabled    bool
	log        *slog.Logger
}

// NewWhatsAppNotifier builds the channel. Pass enabled=false (or a nil sender)
// to run it as a clean no-op — that is what an absent access token or a flipped
// kill switch means, and it must never crash or stall the worker.
//
// fallbackZone is the platform zone used when a venue stores none of its own;
// nil means UTC.
func NewWhatsAppNotifier(
	staff venueStaffReader,
	settings domain.RestaurantNotificationSettingsRepository,
	deliveries domain.NotificationDeliveryRepository,
	zones venueTimezoneReader,
	send WhatsAppSender,
	fallbackZone *time.Location,
	enabled bool,
	log *slog.Logger,
) *WhatsAppNotifier {
	if fallbackZone == nil {
		fallbackZone = time.UTC
	}
	return &WhatsAppNotifier{
		staff: staff, settings: settings, deliveries: deliveries, zones: zones,
		send: send, fallback: fallbackZone, enabled: enabled && send != nil, log: log,
	}
}

var _ Notifier = (*WhatsAppNotifier)(nil)

func (w *WhatsAppNotifier) Channel() domain.NotificationChannel { return domain.ChannelWhatsApp }

// Interested: ONLY a new booking. The approved template says «Новая бронь …
// Подтвердите или отклоните» — it is the wrong text for anything else, and Meta
// gives no way to reuse it with different wording. A cancellation channel needs
// its own approved template and its own branch here.
func (w *WhatsAppNotifier) Interested(t domain.BookingEventType) bool {
	return t == domain.EventBookingCreated
}

// Notify fans the alert out to the venue's opted-in staff.
//
// Return contract (the dispatcher's): nil = this event is durably handled and
// may be marked published; an error = leave it unpublished for the next tick.
// A per-recipient PERMANENT rejection (a 4xx: no WhatsApp account on that
// number, malformed parameters, a dead token) is logged and swallowed — retried
// forever it would never succeed and would wedge the outbox for every other
// channel. Only a transient failure (429/5xx/transport) is worth another tick.
func (w *WhatsAppNotifier) Notify(ctx context.Context, e Event) error {
	if !w.enabled {
		w.log.Info("whatsapp skipped: channel disabled or not configured",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	// The venue's own kill switch (restaurant_notification_settings, migration
	// 0072). A missing row reads as enabled, so a venue that never touched its
	// settings still gets alerts.
	cfg, err := w.settings.WhatsAppSettings(ctx, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("whatsapp: read settings: %w", err)
	}
	if !cfg.Enabled {
		w.log.Info("whatsapp skipped: channel switched off for this venue",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	recipients, err := w.recipients(ctx, e.RestaurantID)
	if err != nil {
		return fmt.Errorf("whatsapp: read venue staff: %w", err)
	}
	if len(recipients) == 0 {
		// Nobody consented, or nobody left a number. Not a failure — the venue
		// simply has this channel unused, and the event must still drain.
		w.log.Info("whatsapp skipped: no opted-in staff with a number",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	params := w.templateParams(ctx, e)

	var retryable error
	for _, r := range recipients {
		// The dedupe target is the STAFF ROW: two managers of one venue must
		// both be alerted, and a redelivery must re-alert neither.
		already, err := w.deliveries.AlreadyDelivered(ctx, e.OutboxEventID, domain.ChannelWhatsApp, r.managerID)
		if err != nil {
			return fmt.Errorf("whatsapp: check delivery: %w", err)
		}
		if already {
			continue
		}

		status, sendErr := w.send(ctx, r.phone, params)
		switch {
		case sendErr == nil:
			// Record AFTER success (at-least-once): a crash here re-sends on the
			// next tick, it never drops the alert.
			if err := w.deliveries.RecordDelivered(ctx, e.OutboxEventID, domain.ChannelWhatsApp, r.managerID); err != nil {
				return fmt.Errorf("whatsapp: record delivery: %w", err)
			}
			w.log.Info("whatsapp: booking alert sent",
				slog.String("booking_id", e.BookingID.String()),
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("manager_id", r.managerID.String()),
				slog.String("phone", logging.MaskPhone(r.phone)),
				slog.Int("status", status))
		case permanentWhatsAppFailure(status):
			// Meta will reject this identically on every retry: a number with no
			// WhatsApp account, parameters the template does not accept, an
			// expired token. Logged loudly and consumed.
			w.log.Error("whatsapp: send permanently rejected, giving up on this recipient",
				slog.String("booking_id", e.BookingID.String()),
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("manager_id", r.managerID.String()),
				slog.String("phone", logging.MaskPhone(r.phone)),
				slog.Int("status", status),
				slog.String("error", sendErr.Error()))
		default:
			w.log.Warn("whatsapp: send failed, event left for retry",
				slog.String("booking_id", e.BookingID.String()),
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("manager_id", r.managerID.String()),
				slog.String("phone", logging.MaskPhone(r.phone)),
				slog.Int("status", status),
				slog.String("error", sendErr.Error()))
			retryable = fmt.Errorf("whatsapp: send to restaurant %s: %w", e.RestaurantID, sendErr)
		}
	}
	return retryable
}

// recipient is one resolved target: which staff row consented, and to which
// number. The staff row id is the dedupe key; the number is E.164.
type recipient struct {
	managerID uuid.UUID
	phone     string
}

// recipients resolves the venue's opted-in staff. Numbers are normalized to
// E.164 and DEDUPED by number: a venue where the owner is also listed as a
// manager (or where two rows carry the same phone written differently) must get
// one message, not two identical ones on the same handset.
func (w *WhatsAppNotifier) recipients(ctx context.Context, restaurantID uuid.UUID) ([]recipient, error) {
	staff, err := w.staff.ListByRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(staff))
	out := make([]recipient, 0, len(staff))
	for _, m := range staff {
		if !m.WhatsappOptIn || m.WhatsappPhone == nil {
			continue
		}
		normalized := phone.Normalize(strings.TrimSpace(*m.WhatsappPhone))
		// "+" plus 11 digits is the shortest number this market has; anything
		// shorter is a half-typed value, and sending to it can only produce a
		// permanent rejection.
		if len(normalized) < 12 || !strings.HasPrefix(normalized, "+") {
			w.log.Warn("whatsapp: staff row has an unusable number, skipped",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("manager_id", m.ID.String()),
				slog.String("phone", logging.MaskPhone(normalized)))
			continue
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, recipient{managerID: m.ID, phone: normalized})
	}
	return out, nil
}

// templateParams fills the approved template's four placeholders, in order:
// {{1}} when, {{2}} how many guests, {{3}} guest name, {{4}} guest phone.
func (w *WhatsAppNotifier) templateParams(ctx context.Context, e Event) []string {
	name := e.GuestName
	if strings.TrimSpace(name) == "" {
		name = "Гость"
	}
	guestPhone := e.GuestPhone
	if strings.TrimSpace(guestPhone) == "" {
		// The template has no way to omit a parameter, and Meta rejects an empty
		// one, so an absent number is spelled out rather than left blank.
		guestPhone = "не указан"
	}
	return []string{
		formatRussianDateTime(e.StartsAt.In(w.venueLocation(ctx, e.RestaurantID))),
		fmt.Sprintf("%d", e.Guests),
		name,
		guestPhone,
	}
}

// venueLocation resolves the zone the venue's own clock runs in.
//
// Unlike the money-bearing readers (payouts, special days) this one FALLS BACK
// instead of failing: the value here is the wording of a notification, and an
// unreadable stored zone must not be the reason a venue never learns about a
// booking. The substitution is logged so the bad value still surfaces.
func (w *WhatsAppNotifier) venueLocation(ctx context.Context, restaurantID uuid.UUID) *time.Location {
	tz, err := w.zones.Timezone(ctx, restaurantID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			w.log.Warn("whatsapp: could not read venue timezone, using the platform zone",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("error", err.Error()))
		}
		return w.fallback
	}
	if strings.TrimSpace(tz) == "" {
		return w.fallback
	}
	loc, err := domain.LoadVenueLocation(tz)
	if err != nil {
		w.log.Warn("whatsapp: venue timezone is unusable, using the platform zone",
			slog.String("restaurant_id", restaurantID.String()),
			slog.String("timezone", tz),
			slog.String("error", err.Error()))
		return w.fallback
	}
	return loc
}

// russianMonthsGenitive are the forms a date reads in: «25 августа», not
// «25 август».
var russianMonthsGenitive = [...]string{
	"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря",
}

// formatRussianDateTime renders an instant the way a person reads it aloud:
// «25 августа в 19:30». t must already be in the target zone.
func formatRussianDateTime(t time.Time) string {
	return fmt.Sprintf("%d %s в %02d:%02d",
		t.Day(), russianMonthsGenitive[int(t.Month())-1], t.Hour(), t.Minute())
}

// permanentWhatsAppFailure classifies Meta's answer. A 4xx other than 429 can
// never succeed on a retry with the same message: an unreachable number, a
// parameter the template refuses, a revoked token. 429 (rate limit), 5xx and a
// transport failure (status 0) are transient and worth another tick.
func permanentWhatsAppFailure(status int) bool {
	return status >= 400 && status < 500 && status != 429
}
