package domain

// DiversifyByKey re-orders an already-ranked list so no more than two
// consecutive items share the same grouping key, per spec
// foodie-personalization-v1-20260916.md §5.5.3. It is generic over the key
// (a restaurant's main cuisine for /restaurants/picks, a card's restaurant id
// for /feed's own rule) so BE-2 and BE-3 both call this ONE function instead
// of each re-implementing the pass.
//
// The pass is a DETERMINISTIC, GREEDY walk, never a sort: items is assumed
// already in the caller's desired priority order (score desc, then
// tie-breaks), and DiversifyByKey only ever pulls a LATER item forward to
// break up a run of three — it never reorders two items that do not need to
// move relative to each other, and never drops or duplicates an item.
//
//	Walk position by position. About to place the next item:
//	  - if the two items already placed share ITS key too (a third in a row
//	    would result), scan forward in what's left for the first item with a
//	    DIFFERENT key and place that one instead;
//	  - if nothing in what's left has a different key, place it anyway — the
//	    rule cannot be satisfied without inventing content, so the caller's
//	    priority order wins over the diversity rule at that point (§5.5.3
//	    says so implicitly: "если такой нет — ставим как есть").
//
// keyOf must be a pure function of one item (no shared mutable state), or the
// "same input → same output" property callers rely on breaks.
func DiversifyByKey[T any](items []T, keyOf func(T) string) []T {
	if len(items) <= 2 {
		out := make([]T, len(items))
		copy(out, items)
		return out
	}

	remaining := make([]T, len(items))
	copy(remaining, items)
	out := make([]T, 0, len(items))

	var lastKey, prevKey string
	var haveLast, havePrev bool
	for len(remaining) > 0 {
		idx := 0
		if havePrev && haveLast && lastKey == prevKey && keyOf(remaining[0]) == lastKey {
			for i := 1; i < len(remaining); i++ {
				if keyOf(remaining[i]) != lastKey {
					idx = i
					break
				}
			}
		}
		chosen := remaining[idx]
		remaining = append(remaining[:idx], remaining[idx+1:]...)
		out = append(out, chosen)

		prevKey, lastKey = lastKey, keyOf(chosen)
		havePrev, haveLast = haveLast, true
	}
	return out
}
