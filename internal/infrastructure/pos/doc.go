// Package pos wires BookEat's point-of-sale integrations behind a single
// domain port, using the ADAPTER (anti-corruption layer) pattern — the same
// shape as infrastructure/payment.
//
// # The choice: one port, one adapter per POS
//
// The domain owns exactly one interface, domain.POSConnector, expressed purely
// in BookEat terms (bookings, tables, occupancy windows — never an iiko
// "organizationId" or an r_keeper "objectId"). Each POS is an isolated adapter
// under this package that translates that vocabulary to and from its own
// protocol and hides every provider quirk, id and secret behind it. Adding a
// fifth POS is a new sub-package plus one line in bootstrap; it must not touch
// domain.POSConnector or any of its call sites. That isolation is the whole
// point: a change in the iiko contract cannot ripple into r_keeper, and the
// availability engine never learns which POS a table span came from.
//
// # The four adapters
//
//   - iiko    — the FIRST POS to be implemented. iiko granted test access;
//     credentials are pending, so its methods are stubs today but it already
//     carries a real env config (iiko/config.go) so wiring is a matter of
//     filling method bodies once the credentials and iikoTransport contract
//     land.
//   - rkeeper — template stub.
//   - poster  — template stub.
//   - kwaaka  — template stub. Kwaaka is an AGGREGATOR that itself fronts iiko /
//     r_keeper / Poster; from BookEat's side that fan-out is Kwaaka's problem,
//     so it is just another adapter behind the same port with no special
//     casing here.
//
// Every stub method returns an error wrapping domain.ErrNotImplemented (via the
// package-local ErrNotWired) rather than guessing a protocol — the same honesty
// discipline as infrastructure/payment/partnerspay. Each adapter's package doc
// lists what is still missing.
//
// # Registry
//
// Registry (registry.go) maps a POSProvider code to the connector this process
// actually has, rejecting duplicate Name()s at construction. It is deliberately
// thinner than payment.Registry: there is NO enabled/default/priority table.
//
// # Intentionally NOT here (follow-up, needs a migration)
//
// The restaurant→POS binding (which venue talks to which POS, with which
// organisation/terminal ids) and its per-venue enable/disable switch will need
// a persisted registry table, exactly like payment_providers. That is a
// deliberate follow-up requiring a schema migration and is out of scope for
// this scaffold — the team does not add speculative DB structure. This package
// is interfaces and template adapters only; no migration ships with it.
package pos
