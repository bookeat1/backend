// Package kwaaka adapts Kwaaka's Table Booking API
// (bookeat-docs/kwaaka/table-booking-swagger-v1.1.0.yaml, OpenAPI 3.0.3,
// v1.1.0) to domain.KwaakaMenuSource. It implements phase 1 of the Kwaaka POS
// integration ONLY — menu + stop-list. Orders, reserves, banquets, tables and
// payment are all in the same document but out of scope here; see
// team-memory specs/bookeat-kwaaka-pos-integration.md for the phased plan.
//
// # What was actually verified, and how
//
// The credentials in ~/.bookeat/kwaaka.env (KWAAKA_BASE_URL,
// KWAAKA_PATH_PREFIX=/v1/table-booking, KWAAKA_TOKEN, KWAAKA_TEST_STORE_ID,
// KWAAKA_SERVICE=bookeat) were issued 2026-08-04 for the Table Booking API.
// The task that requested this integration assumed menu/stop-list might live
// under a DIFFERENT path prefix. Verified 2026-09-21, both from the spec and
// with a live GET against the test store, that this is wrong: Kwaaka merged
// menu into the SAME Table Booking API in v1.1.0 (it used to be a separate
// POST /hooks/{id}/menu webhook in the OLD spec, bookeat-kwaaka-api.yaml,
// which v1.1.0 removed entirely — see bookeat-docs/kwaaka/README.md). The one
// and only endpoint this package calls is:
//
//	GET {KWAAKA_BASE_URL}{KWAAKA_PATH_PREFIX}/restaurants/{kwaakaRestaurantID}/menu?service={KWAAKA_SERVICE}
//	Authorization: {KWAAKA_TOKEN}   (as-is, NOT "Bearer <token>" — apiKey scheme)
//
// A live call against the test store (KWAAKA_TEST_STORE_ID) returned 200 with
// a body matching the OpenAPI Menu schema exactly: top-level id/name/is_active
// plus sections[]/products[]/combos[], each product carrying its own
// `is_available` and each section its own `is_available`. There is no
// separate stop-list endpoint or webhook in the currently granted contract —
// availability IS the menu response, so "sync menu" and "sync stop-list" are
// the same HTTP call, done on the same schedule (usecase/kwaakasync).
//
// # What is deliberately NOT mapped (phase 1 scope)
//
//   - combos (GourmetCombo/GourmetComboGroup/GourmetComboProduct) — no BookEat
//     concept of a combo dish yet;
//   - attributes / attributes_groups (modifiers) — no BookEat concept of a
//     dish modifier yet;
//   - balance (stock count) — phase 1 only needs the boolean is_available;
//   - multi-language name/description — GourmetProduct.name/description are
//     arrays of {language_code, value}; the test store's language_code was
//     always "" (single, unlabelled entry), so mapMenu takes name[0]/
//     description[0] and stops there. TODO(verify): confirm with Kwaaka
//     whether a real venue ever sends more than one entry, and if so which
//     language_code values to expect, before this can localise into
//     MenuItem.NameI18n/DescriptionI18n instead of the base row.
package kwaaka
