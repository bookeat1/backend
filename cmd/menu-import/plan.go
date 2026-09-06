package main

import (
	"backend-core/internal/domain"
)

// Plan is the outcome of diffing a parsed menu file against a restaurant's
// current menu_items rows. It never contains anything to delete/deactivate:
// this importer is strictly additive/updating (left-join semantics) — an
// existing dish absent from the file is left untouched, see doc.go.
type Plan struct {
	// ToInsert are new domain.MenuItem values (ID unset) built from file rows
	// that had no matching existing row.
	ToInsert []domain.MenuItem
	// ToUpdate pairs an existing row (with its DB identity/state preserved)
	// against the file row that changed one of the imported fields.
	ToUpdate []domain.MenuItem
	// Unchanged counts file rows that matched an existing row byte-for-byte on
	// every imported field — nothing to write.
	Unchanged int
	// Sections lists every distinct, non-empty section label seen in the file,
	// in first-seen order — the caller uses this to create missing
	// menu_categories rows.
	Sections []string
	// Skipped are file rows BuildPlan refused to turn into a domain.MenuItem
	// (currently: missing/negative price — money is never guessed). These are
	// reported, never silently dropped, but they must not block the rest of a
	// large file: real parsed venue menus contain the odd row the source PDF
	// genuinely never printed a price for (e.g. a wine list scan), and failing
	// the whole import over one bad line would be worse than skipping it.
	Skipped []SkippedItem
}

// SkippedItem is a file row BuildPlan could not map to a domain.MenuItem.
type SkippedItem struct {
	Name string
	Err  error
}

// changed reports whether applying the file row onto a copy of the existing
// row would change anything Update actually writes.
func changed(existing domain.MenuItem, updated domain.MenuItem) bool {
	if existing.Name != updated.Name || existing.Price != updated.Price ||
		existing.Description != updated.Description {
		return true
	}
	if !samePtr(existing.Category, updated.Category) ||
		!samePtr(existing.PortionSize, updated.PortionSize) ||
		!samePtr(existing.ImageURL, updated.ImageURL) {
		return true
	}
	if !sameI18n(existing.NameI18n, updated.NameI18n) {
		return true
	}
	return false
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameI18n(a, b domain.I18n) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// BuildPlan diffs parsed file rows against a restaurant's existing menu items.
//
// Matching key is (restaurant implicit — existing is already scoped to it) +
// NormalizeName(dish name): trim + case-fold, per Damir's rule. Within one
// name, file rows are paired to existing DB rows IN ORDER (a stable FIFO
// queue per name) rather than picking the "closest" one — both venue files
// and live data contain the odd repeated dish name (e.g. the same drink
// listed under two sections), and a queue keeps the pairing deterministic
// without having to invent a tie-breaker. A file row that runs out of
// existing rows to pair with becomes an insert; an existing row that is
// never claimed is left exactly as it is — it is never queued for delete or
// deactivation.
//
// A name repeated in the FILE ITSELF with no (or no more) existing DB rows to
// pair with is NOT queued as a second insert: menu_items has a unique
// constraint on (restaurant_id, name), so two fresh inserts of the same name
// in one run would violate it. Instead the later file row overwrites the
// pending insert already staged for that name (last-row-in-the-file wins) —
// seen in practice on real venue files (e.g. a dish repeated verbatim under
// a mis-split page).
func BuildPlan(existing []domain.MenuItem, parsed []ParsedItem) (Plan, error) {
	byName := make(map[string][]domain.MenuItem, len(existing))
	for _, m := range existing {
		key := NormalizeName(m.Name)
		byName[key] = append(byName[key], m)
	}

	var plan Plan
	seenSection := make(map[string]bool)
	pendingInsertIdx := make(map[string]int)
	for _, p := range parsed {
		if sec := normalizeSection(p.Section); sec != "" && !seenSection[sec] {
			seenSection[sec] = true
			plan.Sections = append(plan.Sections, sec)
		}

		key := NormalizeName(p.Name)
		queue := byName[key]

		if len(queue) == 0 {
			m := domain.MenuItem{IsAvailable: true}
			if err := p.ToDomainFields(&m); err != nil {
				plan.Skipped = append(plan.Skipped, SkippedItem{Name: p.Name, Err: err})
				continue
			}
			if idx, ok := pendingInsertIdx[key]; ok {
				plan.ToInsert[idx] = m
			} else {
				pendingInsertIdx[key] = len(plan.ToInsert)
				plan.ToInsert = append(plan.ToInsert, m)
			}
			continue
		}

		match := queue[0]
		byName[key] = queue[1:]

		updated := match
		if err := p.ToDomainFields(&updated); err != nil {
			plan.Skipped = append(plan.Skipped, SkippedItem{Name: p.Name, Err: err})
			continue
		}
		if changed(match, updated) {
			plan.ToUpdate = append(plan.ToUpdate, updated)
		} else {
			plan.Unchanged++
		}
	}
	return plan, nil
}
