// Command menu-import upserts a parsed menu JSON file (the `<venue>-menu-*/
// menu.json` shape produced by the venue-menu recon parsers) into the
// existing menu_items/menu_categories tables of ONE restaurant.
//
// It is idempotent and additive-only — "left-join semantics", per Damir:
//   - Dishes matched by restaurant + normalized name (trim, case-insensitive)
//     are UPDATED in place: price, section (category), description and
//     unit (portion_size) are refreshed from the file.
//   - Dishes in the file with no match are INSERTED (available by default).
//   - Dishes already in the DB but ABSENT from the file are left completely
//     untouched — never deleted, never deactivated. The file is a source of
//     NEW/CURRENT data, never a source of removals.
//   - Section labels are matched to menu_categories by trimmed
//     case-insensitive name; missing ones are created (flat, no parent).
//   - A file row with no usable price (missing/negative — money is never
//     guessed) is SKIPPED and reported, not applied and not treated as a
//     fatal error for the rest of the file: a wine-list scan the source PDF
//     genuinely never printed a price for should not block every other dish.
//
// Usage:
//
//	go run ./cmd/menu-import -file path/to/menu.json -restaurant-id <uuid> [-dry-run]
//	go run ./cmd/menu-import -file path/to/menu.json -restaurant-name "Koktobe Terrace" [-dry-run]
//
// -dry-run prints the to-insert/to-update/unchanged counts (and which
// sections would be created) and writes NOTHING.
//
// Multiple files for the same venue (e.g. Mongol's menu-part1.json +
// menu-part2.json + wine.json) can be run one after another; each run only
// ever adds to what the previous one left, per the additive rule above.
package main
