// Package kwaakaorders sends a booking's pre-order to the venue's POS (Kwaaka
// phase 2) as a table order: claim (pick the booking + a table from the venue's
// pool), send (leased, retried POST with a stable order_id), cancel and
// reschedule sweeps, and webhook/poll status application.
//
// Rules that hold across the package (ADR-048/049/050):
//   - HTTP never happens inside a database transaction.
//   - Lock order is booking → venue settings row, never the reverse, and never
//     the shared advisory venue lock.
//   - A Kwaaka failure NEVER touches bookings, booking_items, payments or the
//     ledger: it only moves our own row and raises one alert to the venue.
//   - Every outcome is written with a compare-and-set on (status, attempts).
package kwaakaorders
