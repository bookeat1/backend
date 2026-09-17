// Package foryou is the personalization on top of «Выбрали для вас»
// (usecase/homepicks) — spec foodie-personalization-v1-20260916.md, task
// BE-2. It answers GET /restaurants/picks for BOTH kinds of caller the route
// serves under OptionalAuth: a guest with an ACTIVE taste profile (§5.4) sees
// data.mode = "for_you" with a `match` block per card; everyone else (no
// token, an empty/inactive profile, or a profile that matched nothing in this
// city — §5.5.4/criterion 9) sees EXACTLY the rail usecase/homepicks already
// answers with, labelled "editorial" or "popular".
//
// This package owns the scoring assembly (candidates → domain.ScoreTasteMatch
// → sort → domain.DiversifyByKey → pad from the fallback) and nothing else:
// the taste profile itself is usecase/tastematch.Loader's read (never
// recomposed here — see that package's own doc on why three separate
// assemblies would drift), the actual points come from domain.ScoreTasteMatch
// (BE-1), and the non-personalized rail is usecase/homepicks's. Keeping this
// package thin over three existing pieces is what stops
// /restaurants/picks, /feed and /events from each growing their own
// "personalize a venue list" logic.
package foryou

import (
	"context"
	"log/slog"
	"math"
	"sort"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/homepicks"
)

// Mode is the rail's resolution outcome, echoed as data.mode (spec §5.6/§5.8).
// Only ModeForYou is decided in this package; ModeEditorial/ModePopular are
// domain.HomePicksMode (usecase/homepicks's own vocabulary) relabelled —
// see fromRailMode.
type Mode string

const (
	ModeForYou    Mode = "for_you"
	ModeEditorial Mode = "editorial"
	ModePopular   Mode = "popular"
)

// fallbackPopularCode is BE-2's own reason code for a padding card (spec
// §3.7/§5.5.4: "match.reasons = [{code: fallback_popular, points: 0}]"). It
// is deliberately NOT one of domain's TasteSignalCode constants
// (taste_match.go) — ScoreTasteMatch never produces it, because it does not
// describe a taste signal at all, only "this card is here to fill the row".
// domain.TasteSignalCode is just a defined string type, so constructing this
// value does not require touching BE-1's file.
const fallbackPopularCode domain.TasteSignalCode = "fallback_popular"

// Match is one candidate's taste-match explanation — nil on an Item outside
// ModeForYou (spec §5.6: "match присутствует только при mode = for_you").
type Match struct {
	Score   int
	Reasons []domain.TasteMatchReason
}

// Item is one rail card: the ordinary catalog row plus its optional Match.
type Item struct {
	domain.RestaurantListItem
	Match *Match
}

// Result is GET /restaurants/picks' whole answer, before the transport layer
// maps it to JSON.
type Result struct {
	Items []Item
	Mode  Mode
}

// profileLoader is the slice of usecase/tastematch this package needs.
type profileLoader interface {
	LoadTasteProfile(ctx context.Context, userID uuid.UUID) (domain.TasteProfile, error)
}

// catalog is the slice of usecase/restaurants this package needs: one
// unpaginated, city-filtered read of ACTIVE venues with cuisines/features
// loaded — everything domain.VenueTasteSignals needs to be built from. Same
// shape usecase/homepicks declares its own catalog port as; not reused
// directly on purpose — each usecase names the dependency it needs (see
// CLAUDE.md's "declare a local port").
type catalog interface {
	List(ctx context.Context, f domain.RestaurantFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error)
}

// rail is the slice of usecase/homepicks this package needs: the SAME
// editorial→popular fallback the plain rail answers with (for degrading, and
// for padding a for_you rail that is shorter than the limit), plus the
// manual-pick membership for the editorial_pick bonus (§5.3 row 5).
type rail interface {
	GuestResolved(ctx context.Context, city string, limit int) ([]domain.RestaurantListItem, domain.HomePicksMode, error)
	ManualPickIDs(ctx context.Context, city string) (map[uuid.UUID]bool, error)
}

// Facade is this package's whole surface: one guest read.
type Facade struct {
	loader  profileLoader
	catalog catalog
	rail    rail
}

// NewFacade builds the personalization usecase.
func NewFacade(loader profileLoader, catalog catalog, rail rail) *Facade {
	return &Facade{loader: loader, catalog: catalog, rail: rail}
}

// Guest answers GET /restaurants/picks for one (possibly anonymous) caller.
// userID is nil for a request with no valid bearer token (criterion 5).
//
// Every failure along the personalized path — the profile read, the
// candidate read, the manual-pick read — degrades to the SAME fallback an
// anonymous guest gets, logged at warn, never surfaced as an error: the main
// screen must always open (criterion 10, spec §3.10's "picks_handler" rule).
func (f *Facade) Guest(ctx context.Context, userID *uuid.UUID, city string, limit int) (Result, error) {
	limit = normalizeLimit(limit)

	if userID != nil {
		profile, err := f.loader.LoadTasteProfile(ctx, *userID)
		switch {
		case err != nil:
			slog.Warn("foryou: taste profile load failed, degrading to the editorial/popular rail",
				"user_id", userID.String(), "error", err)
		case profileActive(profile):
			matched, err := f.matchedCandidates(ctx, profile, city)
			if err != nil {
				slog.Warn("foryou: candidate read failed, degrading to the editorial/popular rail",
					"user_id", userID.String(), "city", city, "error", err)
				break
			}
			if len(matched) > 0 {
				return f.assembleForYou(ctx, matched, city, limit)
			}
			// criterion 9: an active profile with every candidate scoring 0
			// taste points is NOT for_you — falls through below, exactly
			// like an anonymous guest.
		}
	}

	items, mode, err := f.rail.GuestResolved(ctx, city, limit)
	if err != nil {
		return Result{}, err
	}
	return Result{Items: plainItems(items), Mode: fromRailMode(mode)}, nil
}

// scoredItem is one candidate that matched (core taste score > 0), carrying
// everything the response needs plus the sort/diversify keys.
type scoredItem struct {
	item    domain.RestaurantListItem
	total   int
	reasons []domain.TasteMatchReason
}

// matchedCandidates reads every active, non-hidden venue of city (spec
// §5.5.1), scores each against profile, keeps only the ones with a positive
// CORE taste score (domain.TasteMatchMaxCoreScore's own four signals —
// cuisine/diet/budget/booked_similar; see its doc comment, which names this
// exact caller), and returns them sorted (§5.5.2: Total ↓, is_popular ↓,
// display_order ↑, id ↑ — the FULL ScoreTasteMatch total, editorial_pick and
// popular included, so those really do act as tie-breaking bonuses) and
// diversified (§5.5.3, by main cuisine).
//
// A candidate that scores 0 on the core signals is dropped here, even if
// editorial_pick/is_popular would give it a positive FULL total — §5.5.4's
// "карточки с Total вкуса > 0" is deliberately narrower than plain "Total":
// a venue that is merely popular is not a personalized match, and must not
// crowd out the fallback padding's own, explicit fallback_popular labelling.
func (f *Facade) matchedCandidates(ctx context.Context, profile domain.TasteProfile, city string) ([]scoredItem, error) {
	cityFilter := domain.City(city)
	items, _, err := f.catalog.List(ctx, domain.RestaurantFilter{
		City:        &cityFilter,
		Unpaginated: true,
	}, domain.VenueStateFilter{})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	manual, err := f.rail.ManualPickIDs(ctx, city)
	if err != nil {
		return nil, err
	}

	out := make([]scoredItem, 0, len(items))
	for _, it := range items {
		if it.Restaurant.HiddenFromHome {
			// §5.5.1's candidate set is is_active (already true — ListActive
			// never returns an inactive venue for a public caller) AND NOT
			// hidden_from_home. The catalog listing does not filter the
			// latter itself (see internal/infrastructure/postgres/restaurant
			// repository's own comment on other callers), so it is filtered
			// here, in Go, same posture as feed_ranking's own eligibility
			// check.
			continue
		}
		signals := domain.VenueTasteSignals{
			RestaurantID:  it.Restaurant.ID,
			CuisineCodes:  cuisineCodes(it.Cuisines),
			PriceCategory: it.Restaurant.PriceCategory,
			FeatureCodes:  featureCodes(it.Features),
			IsPopular:     boolVal(it.Restaurant.IsPopular),
			EditorialPick: manual[it.Restaurant.ID],
			// Rating/ReviewCount: left at the zero value. RestaurantListItem
			// carries no review aggregate (unlike feed's own FeedItemRow),
			// and §5.1 records zero reviews platform-wide today — venue_rating
			// is 0 for every candidate either way, so fetching an aggregate
			// that would only ever read 0 is not worth a 5th query.
		}
		total, reasons := domain.ScoreTasteMatch(profile, signals)
		if coreTasteScore(reasons) <= 0 {
			continue
		}
		out = append(out, scoredItem{item: it, total: total, reasons: reasons})
	}
	sortScored(out)
	return domain.DiversifyByKey(out, mainCuisineKey), nil
}

// assembleForYou turns an already sorted+diversified matched list into the
// for_you Result, padding the tail from the fallback rail when matched is
// shorter than limit (§5.5.4/criterion 8).
func (f *Facade) assembleForYou(ctx context.Context, matched []scoredItem, city string, limit int) (Result, error) {
	items := make([]Item, 0, limit)
	shown := make(map[uuid.UUID]bool, limit)
	for _, s := range matched {
		if len(items) >= limit {
			break
		}
		items = append(items, Item{
			RestaurantListItem: s.item,
			Match:              &Match{Score: s.total, Reasons: s.reasons},
		})
		shown[s.item.Restaurant.ID] = true
	}

	if len(items) < limit {
		pad, _, err := f.rail.GuestResolved(ctx, city, limit)
		if err != nil {
			// A padding-source failure must not sink an otherwise-good
			// for_you rail (spec §3.7: the row is never SHORTER than
			// min(limit, available) as a best effort, not a hard guarantee
			// against a downstream failure) — it just comes back short.
			slog.Warn("foryou: fallback padding read failed, for_you rail served shorter than the limit",
				"city", city, "error", err)
		} else {
			for _, it := range pad {
				if len(items) >= limit {
					break
				}
				if shown[it.Restaurant.ID] {
					continue
				}
				items = append(items, Item{
					RestaurantListItem: it,
					Match: &Match{Score: 0, Reasons: []domain.TasteMatchReason{{
						Code:   fallbackPopularCode,
						Points: 0,
						Detail: "padded from the editorial/popular fallback, not a taste match",
					}}},
				})
				shown[it.Restaurant.ID] = true
			}
		}
	}
	return Result{Items: items, Mode: ModeForYou}, nil
}

// coreTasteCodes is domain.TasteMatchMaxCoreScore's own four signals, by
// code — kept in lock-step with that constant's doc comment ("A caller that
// needs the guest-preference sub-score alone... sums just those four
// reasons' Points"). This package is that caller.
var coreTasteCodes = map[domain.TasteSignalCode]bool{
	domain.TasteSignalCuisineMatch:         true,
	domain.TasteSignalCuisineMatchImplicit: true,
	domain.TasteSignalDietMatch:            true,
	domain.TasteSignalBudgetMatch:          true,
	domain.TasteSignalBookedSimilar:        true,
}

func coreTasteScore(reasons []domain.TasteMatchReason) int {
	total := 0
	for _, r := range reasons {
		if coreTasteCodes[r.Code] {
			total += r.Points
		}
	}
	return total
}

// dietsWithVenueAxis are the wizard diet ids §5.2's table gives a venue-side
// axis to (halal/vegan/no_gluten/pescetarian — see domain's dietAxes); the
// other five (kosher/keto/low_carb/paleo/no_lactose) and no_diet never make a
// profile active on their own (spec §5.4: "≥ 1 диета с осью заведения").
var dietsWithVenueAxis = map[string]bool{
	domain.FoodieDietHalal:       true,
	domain.FoodieDietVegan:       true,
	domain.FoodieDietNoGluten:    true,
	domain.FoodieDietPescetarian: true,
}

// profileActive implements spec §5.4 exactly: at least one of four
// independent conditions, not a threshold on any single one.
func profileActive(p domain.TasteProfile) bool {
	if len(p.CuisineCodes) > 0 {
		return true
	}
	if p.Budget != nil {
		return true
	}
	for _, d := range p.Diets {
		if dietsWithVenueAxis[d] {
			return true
		}
	}
	// "явных кухонь нет, но есть ≥ 1 не отменённая бронь" — CuisineCodes is
	// already known empty at this point (checked first above).
	return len(p.BookedRestaurantIDs) > 0
}

// sortScored orders candidates by spec §5.5.2: Total ↓, is_popular ↓,
// display_order ↑ (NULLs last, matching the catalog's own ORDER BY), id ↑ —
// a total order, so the result is deterministic regardless of the catalog
// read's own row order.
func sortScored(items []scoredItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.total != b.total {
			return a.total > b.total
		}
		ap, bp := boolVal(a.item.Restaurant.IsPopular), boolVal(b.item.Restaurant.IsPopular)
		if ap != bp {
			return ap
		}
		ad, bd := displayOrderRank(a.item.Restaurant.DisplayOrder), displayOrderRank(b.item.Restaurant.DisplayOrder)
		if ad != bd {
			return ad < bd
		}
		return a.item.Restaurant.ID.String() < b.item.Restaurant.ID.String()
	})
}

func displayOrderRank(d *int) int {
	if d == nil {
		return math.MaxInt
	}
	return *d
}

// mainCuisineKey is DiversifyByKey's grouping key (§5.5.3): the venue's first
// cuisine in position order, or "none" for a venue with no dictionary links.
func mainCuisineKey(s scoredItem) string {
	if len(s.item.Cuisines) == 0 {
		return "none"
	}
	return s.item.Cuisines[0].Code
}

func cuisineCodes(cs []domain.Cuisine) []string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Code)
	}
	return out
}

func featureCodes(fs []domain.VenueFeature) []string {
	if len(fs) == 0 {
		return nil
	}
	out := make([]string, 0, len(fs))
	for _, ft := range fs {
		out = append(out, ft.Code)
	}
	return out
}

func boolVal(b *bool) bool { return b != nil && *b }

func plainItems(items []domain.RestaurantListItem) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, Item{RestaurantListItem: it})
	}
	return out
}

func fromRailMode(m domain.HomePicksMode) Mode {
	if m == domain.HomePicksModeEditorial {
		return ModeEditorial
	}
	return ModePopular
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return homepicks.DefaultLimit
	}
	if limit > homepicks.MaxLimit {
		return homepicks.MaxLimit
	}
	return limit
}
