package notifications

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// venueClock resolves the zone a venue's own clock runs in, for the guest
// channels that quote a booking time (mobile push, in-app feed). It mirrors
// WhatsAppNotifier.venueLocation: an unreadable or empty stored zone FALLS BACK
// to the platform zone (BOOKING_TIMEZONE_FALLBACK, default Asia/Almaty) rather
// than failing, because this is only the wording of a message. A substitution
// caused by a bad value is logged.
type venueClock struct {
	zones    venueTimezoneReader
	fallback *time.Location
	log      *slog.Logger
}

func newVenueClock(zones venueTimezoneReader, fallback *time.Location, log *slog.Logger) venueClock {
	if fallback == nil {
		fallback = time.UTC
	}
	return venueClock{zones: zones, fallback: fallback, log: log}
}

func (c venueClock) location(ctx context.Context, restaurantID uuid.UUID) *time.Location {
	if c.zones == nil {
		return c.fallback
	}
	tz, err := c.zones.Timezone(ctx, restaurantID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			c.log.Warn("guest notify: could not read venue timezone, using the platform zone",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("error", err.Error()))
		}
		return c.fallback
	}
	if strings.TrimSpace(tz) == "" {
		return c.fallback
	}
	loc, err := domain.LoadVenueLocation(tz)
	if err != nil {
		c.log.Warn("guest notify: venue timezone is unusable, using the platform zone",
			slog.String("restaurant_id", restaurantID.String()),
			slog.String("timezone", tz),
			slog.String("error", err.Error()))
		return c.fallback
	}
	return loc
}
