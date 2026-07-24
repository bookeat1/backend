package domain

import (
	"time"

	"github.com/google/uuid"
)

// This file holds the read-model types for the superadmin platform dashboard
// (Ф1): platform-wide aggregate statistics for the global superadmin only.
// They are pure read models — no entity here is ever written; every value is
// produced by a GROUP BY / COUNT / SUM in the dashboard read repository. Money
// is always integer minor units of a SINGLE currency, never a float and never
// summed across currencies.

// PlatformOverview is the dashboard's top-line platform counters. The 7/30-day
// booking windows are always trailing from the server's "now" at query time,
// independent of any caller-supplied period.
type PlatformOverview struct {
	TotalRestaurants   int64
	ActiveRestaurants  int64
	TotalUsers         int64
	TotalBookings      int64
	BookingsLast7Days  int64
	BookingsLast30Days int64
}

// BookingStatusCount is one status bucket of the bookings breakdown.
type BookingStatusCount struct {
	Status BookingStatus
	Count  int64
}

// BookingsBreakdown is booking counts grouped by status over a period. ByStatus
// carries every known status (zero-filled) in a stable order, so the shape is
// identical on an empty platform.
type BookingsBreakdown struct {
	From     time.Time
	To       time.Time
	Total    int64
	ByStatus []BookingStatusCount
}

// MoneyAggregate is a summed money figure and the number of rows it covers, in
// integer minor units of ONE currency. It is read straight from the stored
// amounts (payments.amount_minor / payment_refunds.amount_minor) — the
// dashboard never recomputes money, it only sums what the payment flow already
// recorded.
type MoneyAggregate struct {
	AmountMinor int64
	Count       int64
}

// PaymentsGMV is captured gross merchandise value (money processed through the
// platform) and refunded totals over a period, for a single currency. NOTE on
// naming: this is money PROCESSED, not BookEat revenue — under the subscription
// model BookEat's own take is ~zero, so "GMV" here means the guest→restaurant
// money that flowed, not platform income.
type PaymentsGMV struct {
	From     time.Time
	To       time.Time
	Currency string
	Captured MoneyAggregate
	Refunded MoneyAggregate
}

// TopRestaurant is one row of the top-restaurants ranking. Depending on the
// ranking dimension, either BookingsCount (by=bookings) or GMVMinor
// (by=gmv, single currency) is the ordered metric; both are populated.
type TopRestaurant struct {
	RestaurantID  uuid.UUID
	Name          string
	NameI18n      I18n
	BookingsCount int64
	GMVMinor      int64
}
