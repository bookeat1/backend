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
// There are TWO sources of recipients, and the alert goes to their UNION:
//
//  1. the venue's own number — restaurant_notification_settings.whatsapp_phone,
//     the field the owner fills in the admin panel's «Уведомления в WhatsApp»
//     card and next to which the panel shows «Подключено». Before this it was
//     read only for the kill switch and never used as an address, so the card
//     was a control that lied: a number was saved, a green badge appeared, and
//     nothing was ever sent to it.
//  2. every staff row that gave PERSONAL consent — restaurant_managers with
//     whatsapp_opt_in = true AND a number. A manager's WhatsApp is their own
//     handset, so this consent cannot be inherited from the venue.
//
// Neither source is required. No number anywhere = nothing to do and NOT an
// error — the event drains.
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

	recipients, err := w.recipients(ctx, e.RestaurantID, cfg.Phone)
	if err != nil {
		return fmt.Errorf("whatsapp: read venue staff: %w", err)
	}
	if len(recipients) == 0 {
		// The venue set no number of its own and nobody on the roster consented.
		// Not a failure — the venue simply has this channel unused, and the
		// event must still drain.
		w.log.Info("whatsapp skipped: venue has no number and no opted-in staff",
			slog.String("booking_id", e.BookingID.String()),
			slog.String("restaurant_id", e.RestaurantID.String()))
		return nil
	}

	params := w.templateParams(ctx, e)

	var retryable error
	for _, r := range recipients {
		// The dedupe target is the NUMBER (see whatsAppDedupeTarget), not the
		// staff row: the same handset must get one message even when it is
		// reachable through two different rows, and a redelivery must re-alert
		// nobody who already received this booking.
		already, err := w.deliveries.AlreadyDelivered(ctx, e.OutboxEventID, domain.ChannelWhatsApp, r.targetID)
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
			if err := w.deliveries.RecordDelivered(ctx, e.OutboxEventID, domain.ChannelWhatsApp, r.targetID); err != nil {
				return fmt.Errorf("whatsapp: record delivery: %w", err)
			}
			w.log.Info("whatsapp: booking alert sent",
				slog.String("booking_id", e.BookingID.String()),
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("phone", logging.MaskPhone(r.phone)),
				r.logSource(),
				slog.Int("status", status))
		case permanentWhatsAppFailure(status):
			// Meta will reject this identically on every retry: a number with no
			// WhatsApp account, parameters the template does not accept, an
			// expired token. Logged loudly and consumed.
			w.log.Error("whatsapp: send permanently rejected, giving up on this recipient",
				slog.String("booking_id", e.BookingID.String()),
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("phone", logging.MaskPhone(r.phone)),
				r.logSource(),
				slog.Int("status", status),
				slog.String("error", sendErr.Error()))
		default:
			w.log.Warn("whatsapp: send failed, event left for retry",
				slog.String("booking_id", e.BookingID.String()),
				slog.String("restaurant_id", e.RestaurantID.String()),
				slog.String("phone", logging.MaskPhone(r.phone)),
				r.logSource(),
				slog.Int("status", status),
				slog.String("error", sendErr.Error()))
			retryable = fmt.Errorf("whatsapp: send to restaurant %s: %w", e.RestaurantID, sendErr)
		}
	}
	return retryable
}

// whatsAppRecipientSource says where a number came from. It is log-only: the
// two sources are equal citizens once the number is resolved.
type whatsAppRecipientSource string

const (
	// sourceVenue is restaurant_notification_settings.whatsapp_phone — the
	// number the owner typed into the admin panel.
	sourceVenue whatsAppRecipientSource = "venue"
	// sourceStaff is a restaurant_managers row with personal consent.
	sourceStaff whatsAppRecipientSource = "staff"
)

// recipient is one resolved target: an E.164 number, the dedupe identity
// derived from it, and — for a staff row — which row it came from, so support
// can tell whose consent produced a message.
type recipient struct {
	targetID  uuid.UUID
	phone     string
	source    whatsAppRecipientSource
	managerID uuid.UUID // uuid.Nil for the venue's own number
}

// logSource renders the origin of a recipient for a log line: "venue", or the
// staff row id that consented.
func (r recipient) logSource() slog.Attr {
	if r.source == sourceStaff {
		return slog.String("manager_id", r.managerID.String())
	}
	return slog.String("source", string(r.source))
}

// whatsAppDedupeNamespace seeds the derived delivery targets below. A fixed,
// arbitrary constant — it only has to stay the same forever, which is why it is
// written out here rather than generated.
var whatsAppDedupeNamespace = uuid.MustParse("2b7d2a24-9e2b-4d5f-9a3f-6a3a1a5f7c11")

// whatsAppDedupeTarget maps an E.164 number to the uuid this channel writes
// into notification_deliveries.target_id.
//
// Why the NUMBER and not the row it came from. The ledger's unique key is
// (outbox_event_id, channel, target_id), so target_id decides what "the same
// recipient" means. For WhatsApp that is a handset, not a database row: the
// owner's personal number is very often BOTH the venue's number and their own
// staff row, and two rows keyed separately would ring the same phone twice for
// one booking. Keying by number also survives the roster changing between two
// attempts at the same event — a staff row deleted, or a number moved to a
// different person, would otherwise look like a brand-new recipient on the
// retry and re-send.
//
// Stability: this is a pure function of the number, so attempt #2 of an event
// computes exactly the same target as attempt #1 and the ledger's pre-check
// (and its unique index) still stop the duplicate.
//
// Collision with a real staff row id is impossible, not merely unlikely: this
// is a version-5 (name-based) uuid, while every id in this database is version
// 4 (uuid.New() in Go, gen_random_uuid() in Postgres). The version nibble
// differs, so the two spaces cannot intersect.
func whatsAppDedupeTarget(e164 string) uuid.UUID {
	return uuid.NewSHA1(whatsAppDedupeNamespace, []byte("whatsapp:"+e164))
}

// recipients resolves everyone this venue's booking alert must reach: the
// venue's own number first, then every staff row with personal consent.
//
// Numbers are normalized to E.164 and deduped by the normalized value, so the
// owner who appears in both places — or two staff rows carrying the same number
// written differently — produces exactly ONE message.
func (w *WhatsAppNotifier) recipients(ctx context.Context, restaurantID uuid.UUID, venuePhone string) ([]recipient, error) {
	staff, err := w.staff.ListByRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(staff)+1)
	out := make([]recipient, 0, len(staff)+1)

	add := func(raw string, source whatsAppRecipientSource, managerID uuid.UUID) {
		normalized, ok := w.normalizeRecipientPhone(raw)
		if !ok {
			w.log.Warn("whatsapp: unusable number, skipped",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("source", string(source)),
				slog.String("manager_id", managerID.String()),
				slog.String("phone", logging.MaskPhone(normalized)))
			return
		}
		if _, dup := seen[normalized]; dup {
			return
		}
		seen[normalized] = struct{}{}
		out = append(out, recipient{
			targetID:  whatsAppDedupeTarget(normalized),
			phone:     normalized,
			source:    source,
			managerID: managerID,
		})
	}

	// The venue's own number goes first: it is the address the owner sees as
	// «Подключено» in the panel, so when it collides with a staff number it is
	// the one that wins the single message.
	if strings.TrimSpace(venuePhone) != "" {
		add(venuePhone, sourceVenue, uuid.Nil)
	}
	for _, m := range staff {
		if !m.WhatsappOptIn || m.WhatsappPhone == nil {
			continue
		}
		add(*m.WhatsappPhone, sourceStaff, m.ID)
	}
	return out, nil
}

// normalizeRecipientPhone converts a stored number to E.164 and rejects a
// half-typed one. "+" plus 11 digits is the shortest number this market has;
// anything shorter can only ever earn a permanent rejection from Meta.
func (w *WhatsAppNotifier) normalizeRecipientPhone(raw string) (string, bool) {
	normalized := phone.Normalize(strings.TrimSpace(raw))
	if len(normalized) < 12 || !strings.HasPrefix(normalized, "+") {
		return normalized, false
	}
	return normalized, true
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
