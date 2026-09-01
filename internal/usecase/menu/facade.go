package menu

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes menu reads and per-restaurant mutations. Mutating methods take
// restaurantID (from the route) and enforce that the item belongs to it (IDOR).
type Facade interface {
	// ListByRestaurant returns the venue's whole menu. It takes no language:
	// the dish SET is the same in every language, and the texts are localized
	// at the transport edge from the *_i18n maps.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.MenuItem, error)
	Get(ctx context.Context, itemID uuid.UUID) (*domain.MenuItem, error)
	Categories(ctx context.Context) ([]domain.MenuCategory, error)

	Create(ctx context.Context, restaurantID uuid.UUID, in ItemInput) (*domain.MenuItem, error)
	Update(ctx context.Context, restaurantID, itemID uuid.UUID, in ItemInput) (*domain.MenuItem, error)
	Delete(ctx context.Context, restaurantID, itemID uuid.UUID) error
	SetAvailable(ctx context.Context, restaurantID, itemID uuid.UUID, available bool) error
	// SetAvailableBulk flips availability for a set of items in one statement,
	// scoped to restaurantID (the tenant guard is enforced in SQL — items of
	// another venue are silently skipped). Returns the count actually changed.
	// This is the fast "we ran out" stop-list path.
	SetAvailableBulk(ctx context.Context, restaurantID uuid.UUID, itemIDs []uuid.UUID, available bool) (int, error)
	// SetFeatured marks one dish of restaurantID as an editorial pick (or drops
	// the mark). The tenant guard lives in SQL, so an item of another venue
	// comes back as ErrNotFound instead of being promoted.
	SetFeatured(ctx context.Context, restaurantID, itemID uuid.UUID, featured bool) error
	// ListFeatured returns the cross-venue "chef's picks" rail for one city.
	// limit is clamped here (not in the repository) because it is a transport
	// concern: an unbounded rail is a slow query a client can ask for by
	// accident.
	ListFeatured(ctx context.Context, city domain.City, limit int) ([]domain.FeaturedMenuItem, error)

	// ListHighlights resolves the «Лучшие позиции» rail of ONE venue's
	// storefront: the dishes the venue marked itself, in its own order, and
	// then — only to fill the rail up to limit — the derived dishes that rail
	// used to consist of entirely. Dishes that are unavailable or have no
	// photo never appear, marked or not. See resolveHighlights for the rule and
	// why the fallback exists. The result is always a non-nil slice: a venue
	// with nothing to show gets an empty rail, not an error, so the client can
	// hide the section.
	ListHighlights(ctx context.Context, restaurantID uuid.UUID, limit int) ([]domain.MenuItem, error)
	// SetTopPick marks or unmarks one dish of restaurantID as a «Лучшая
	// позиция». Marking takes the lowest free slot; the venue's own order is
	// changed only through ReplaceTopPicks. Marking an already marked dish is a
	// no-op (a double tap in the panel must not shuffle the rail).
	SetTopPick(ctx context.Context, restaurantID, itemID uuid.UUID, on bool) error
	// ReplaceTopPicks sets the venue's whole rail at once, in the given order:
	// itemIDs[0] becomes slot 1. An empty slice clears the rail (and the venue
	// falls back to the derived list). Atomic — a single bad id changes nothing.
	ReplaceTopPicks(ctx context.Context, restaurantID uuid.UUID, itemIDs []uuid.UUID) error
	// ListTopPicks returns what the venue has marked, in its order, INCLUDING
	// dishes that are currently unavailable — this is the panel's editor view,
	// not the guest's rail.
	ListTopPicks(ctx context.Context, restaurantID uuid.UUID) ([]domain.MenuItem, error)

	CreateCategory(ctx context.Context, in CategoryInput) (*domain.MenuCategory, error)
	UpdateCategory(ctx context.Context, id uuid.UUID, in CategoryInput) (*domain.MenuCategory, error)
	DeleteCategory(ctx context.Context, id uuid.UUID) error
}

type facade struct {
	items      domain.MenuItemRepository
	categories domain.MenuCategoryRepository
	tx         domain.TxManager
}

// NewFacade constructs the menu Facade.
func NewFacade(items domain.MenuItemRepository, categories domain.MenuCategoryRepository, tx domain.TxManager) Facade {
	return &facade{items: items, categories: categories, tx: tx}
}

// ItemInput carries mutable menu-item fields. On Update a nil pointer leaves the
// existing value unchanged; Tags nil leaves existing tags untouched, non-nil
// replaces them (opt-in, so a PATCH that omits "tags" preserves them).
type ItemInput struct {
	Name            *string
	NameI18n        domain.I18n
	Description     *string
	DescriptionI18n domain.I18n
	Price           *string
	ImageURL        *string
	IsAvailable     *bool
	Category        *string
	CategoryI18n    domain.I18n
	Subcategory     *string
	SubcategoryI18n domain.I18n
	PortionSize     *string
	PortionSizeI18n domain.I18n
	Language        *string
	DisplayOrder    *int
	Tags            *[]string
}

// CategoryInput carries mutable menu-category fields.
type CategoryInput struct {
	Name         string
	NameI18n     domain.I18n
	ParentID     *uuid.UUID
	DisplayOrder int
}

func (f *facade) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.MenuItem, error) {
	return f.items.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: restaurantID})
}

func (f *facade) Get(ctx context.Context, itemID uuid.UUID) (*domain.MenuItem, error) {
	return f.items.GetByID(ctx, itemID)
}

func (f *facade) Categories(ctx context.Context) ([]domain.MenuCategory, error) {
	return f.categories.List(ctx)
}

func (f *facade) Create(ctx context.Context, restaurantID uuid.UUID, in ItemInput) (*domain.MenuItem, error) {
	if in.Name == nil || in.Price == nil {
		return nil, domain.ErrValidation
	}
	if *in.Name == "" || !domain.ValidPrice(*in.Price) {
		return nil, domain.ErrValidation
	}
	if err := checkBaseLanguage(nil, in.Language); err != nil {
		return nil, err
	}
	m := &domain.MenuItem{ID: uuid.New(), RestaurantID: restaurantID, IsAvailable: true}
	applyItem(m, in)
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.items.Create(ctx, m); err != nil {
			return err
		}
		return f.items.ReplaceTags(ctx, m.ID, tagsOf(m.ID, in.Tags))
	})
	if err != nil {
		return nil, err
	}
	return f.items.GetByID(ctx, m.ID)
}

func (f *facade) Update(ctx context.Context, restaurantID, itemID uuid.UUID, in ItemInput) (*domain.MenuItem, error) {
	if in.Price != nil && !domain.ValidPrice(*in.Price) {
		return nil, domain.ErrValidation
	}
	if in.Name != nil && *in.Name == "" {
		return nil, domain.ErrValidation
	}
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		existing, err := f.items.GetByID(ctx, itemID)
		if err != nil {
			return err
		}
		if existing.RestaurantID != restaurantID {
			return domain.ErrNotFound // IDOR: item belongs to another restaurant
		}
		if err := checkBaseLanguage(existing.Language, in.Language); err != nil {
			return err
		}
		applyItem(existing, in)
		if err := f.items.Update(ctx, existing); err != nil {
			return err
		}
		if in.Tags != nil {
			return f.items.ReplaceTags(ctx, itemID, tagsOf(itemID, in.Tags))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f.items.GetByID(ctx, itemID)
}

func (f *facade) Delete(ctx context.Context, restaurantID, itemID uuid.UUID) error {
	return f.ownedThen(ctx, restaurantID, itemID, func(ctx context.Context) error {
		return f.items.Delete(ctx, itemID)
	})
}

func (f *facade) SetAvailable(ctx context.Context, restaurantID, itemID uuid.UUID, available bool) error {
	return f.ownedThen(ctx, restaurantID, itemID, func(ctx context.Context) error {
		return f.items.SetAvailable(ctx, itemID, available)
	})
}

func (f *facade) SetAvailableBulk(ctx context.Context, restaurantID uuid.UUID, itemIDs []uuid.UUID, available bool) (int, error) {
	return f.items.SetAvailableBulk(ctx, restaurantID, itemIDs, available)
}

func (f *facade) SetFeatured(ctx context.Context, restaurantID, itemID uuid.UUID, featured bool) error {
	return f.items.SetFeatured(ctx, restaurantID, itemID, featured)
}

// featuredLimit bounds the rail. The default matches what the main screen
// renders; the ceiling exists so a client cannot turn a home-screen rail into a
// full catalogue dump.
const (
	featuredLimitDefault = 10
	featuredLimitMax     = 50
)

func (f *facade) ListFeatured(ctx context.Context, city domain.City, limit int) ([]domain.FeaturedMenuItem, error) {
	if !city.Valid() {
		return nil, domain.WithCode(domain.CodeCityRequired, fmt.Errorf("%w: city is required", domain.ErrValidation))
	}
	switch {
	case limit <= 0:
		limit = featuredLimitDefault
	case limit > featuredLimitMax:
		limit = featuredLimitMax
	}
	return f.items.ListFeatured(ctx, domain.FeaturedMenuFilter{City: city, Limit: limit})
}

// highlightLimit bounds a venue's storefront rail. The default is what the app
// renders today; the ceiling exists so the endpoint cannot be turned into a
// full menu dump through ?limit=.
const (
	highlightLimitDefault = domain.MenuTopPickLimit
	highlightLimitMax     = 24
)

// topPickRetries bounds the optimistic retry on a lost slot race. Two managers
// marking a dish at the same instant compute the same free slot and one of them
// loses on the partial UNIQUE index; recomputing is the whole fix. Three
// attempts is far more than the rail's 8 slots can plausibly need, and a bound
// is what keeps a genuinely full rail from spinning.
const topPickRetries = 3

func (f *facade) ListHighlights(ctx context.Context, restaurantID uuid.UUID, limit int) ([]domain.MenuItem, error) {
	switch {
	case limit <= 0:
		limit = highlightLimitDefault
	case limit > highlightLimitMax:
		limit = highlightLimitMax
	}
	items, err := f.items.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: restaurantID})
	if err != nil {
		return nil, err
	}
	return resolveHighlights(items, limit), nil
}

// resolveHighlights is the ONLY place the rail's rule lives.
//
//  1. The venue's own marks win, in the venue's own order (top_pick_position).
//  2. What is left of the rail is filled with the derivation that used to BE
//     the rail: available dishes in the venue's display_order (items already
//     arrive in that order from the repository).
//  3. Nothing unavailable is ever returned, marked or not — a stop-listed dish
//     in a "best of" rail is an invitation to order something the kitchen has
//     run out of. A deleted dish cannot appear at all: the mark lives on the
//     dish row, so it dies with it.
//  4. Nothing WITHOUT A PHOTO is ever returned, marked or not (2026-09-01,
//     owner's call): the rail is a shop window and a row of grey placeholders
//     is worse than a shorter rail. This applies to the venue's own marks too —
//     a marked dish keeps its slot in the database and comes back the moment a
//     photo is uploaded, it is only hidden from guests meanwhile. The cabinet
//     view (ListTopPicks) still shows it, so the venue can see what it marked
//     and why it is not on the storefront.
//
// Why the fallback and not an empty rail: today NO venue has marked anything,
// and the rail is part of the storefront layout. Dropping it to nothing for
// every venue on the day this ships would be a visible regression traded for a
// purity nobody asked for. A venue that wants a curated rail marks dishes and
// the derived tail shrinks to what its marks leave over; a venue that marks 8
// dishes never sees a derived dish at all.
//
// The photo rule does narrow that fallback, and deliberately so: most of the
// imported catalog has no photo (811 dishes of 2376 on 2026-08-24), so a venue
// whose available dishes are all photo-less now gets an EMPTY rail instead of a
// row of grey placeholders. Empty is a valid answer here — the endpoint returns
// an empty list, never an error and never null, and the app hides the section.
func resolveHighlights(items []domain.MenuItem, limit int) []domain.MenuItem {
	picked := make([]domain.MenuItem, 0, domain.MenuTopPickLimit)
	derived := make([]domain.MenuItem, 0, limit)
	for _, m := range items {
		if !m.IsAvailable || !m.HasImage() {
			continue
		}
		if m.TopPickPosition != nil {
			picked = append(picked, m)
			continue
		}
		derived = append(derived, m)
	}
	sort.SliceStable(picked, func(i, j int) bool {
		return *picked[i].TopPickPosition < *picked[j].TopPickPosition
	})
	out := picked
	if len(out) > limit {
		return out[:limit]
	}
	for _, m := range derived {
		if len(out) >= limit {
			break
		}
		out = append(out, m)
	}
	return out
}

func (f *facade) ListTopPicks(ctx context.Context, restaurantID uuid.UUID) ([]domain.MenuItem, error) {
	return f.items.ListTopPicks(ctx, restaurantID)
}

func (f *facade) SetTopPick(ctx context.Context, restaurantID, itemID uuid.UUID, on bool) error {
	if !on {
		// The repository's restaurant_id predicate is the tenant guard; an id
		// of another venue is ErrNotFound, never a silent no-op.
		return f.items.SetTopPickPosition(ctx, restaurantID, itemID, nil)
	}
	var err error
	for attempt := 0; attempt < topPickRetries; attempt++ {
		err = f.tx.WithinTx(ctx, func(ctx context.Context) error {
			picks, err := f.items.ListTopPicks(ctx, restaurantID)
			if err != nil {
				return err
			}
			taken := make(map[int]bool, len(picks))
			for _, p := range picks {
				if p.ID == itemID {
					// Already on the rail: keep its place. Re-marking must not
					// move a dish the venue deliberately ordered.
					return nil
				}
				if p.TopPickPosition != nil {
					taken[*p.TopPickPosition] = true
				}
			}
			slot := 0
			for i := 1; i <= domain.MenuTopPickLimit; i++ {
				if !taken[i] {
					slot = i
					break
				}
			}
			if slot == 0 {
				return domain.WithCode(domain.CodeMenuTopPicksLimit,
					fmt.Errorf("%w: a venue may mark at most %d dishes as top picks", domain.ErrValidation, domain.MenuTopPickLimit))
			}
			return f.items.SetTopPickPosition(ctx, restaurantID, itemID, &slot)
		})
		if !errors.Is(err, domain.ErrAlreadyExists) {
			return err
		}
	}
	return err
}

func (f *facade) ReplaceTopPicks(ctx context.Context, restaurantID uuid.UUID, itemIDs []uuid.UUID) error {
	if len(itemIDs) > domain.MenuTopPickLimit {
		return domain.WithCode(domain.CodeMenuTopPicksLimit,
			fmt.Errorf("%w: a venue may mark at most %d dishes as top picks, got %d",
				domain.ErrValidation, domain.MenuTopPickLimit, len(itemIDs)))
	}
	seen := make(map[uuid.UUID]bool, len(itemIDs))
	for _, id := range itemIDs {
		if seen[id] {
			// Not de-duplicated silently: an ordered list that names the same
			// dish twice means the panel and the server disagree about the
			// rail, and guessing which slot was meant is guessing.
			return fmt.Errorf("%w: duplicate dish %s in the top picks order", domain.ErrValidation, id)
		}
		seen[id] = true
	}
	return f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := f.items.ClearTopPicks(ctx, restaurantID); err != nil {
			return err
		}
		for i, id := range itemIDs {
			slot := i + 1
			if err := f.items.SetTopPickPosition(ctx, restaurantID, id, &slot); err != nil {
				return err
			}
		}
		return nil
	})
}

// ownedThen verifies itemID belongs to restaurantID (IDOR) then runs fn.
func (f *facade) ownedThen(ctx context.Context, restaurantID, itemID uuid.UUID, fn func(context.Context) error) error {
	existing, err := f.items.GetByID(ctx, itemID)
	if err != nil {
		return err
	}
	if existing.RestaurantID != restaurantID {
		return domain.ErrNotFound
	}
	return fn(ctx)
}

func (f *facade) CreateCategory(ctx context.Context, in CategoryInput) (*domain.MenuCategory, error) {
	if in.Name == "" {
		return nil, domain.ErrValidation
	}
	c := &domain.MenuCategory{ID: uuid.New(), Name: in.Name, NameI18n: in.NameI18n, ParentID: in.ParentID, DisplayOrder: in.DisplayOrder}
	if err := f.categories.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (f *facade) UpdateCategory(ctx context.Context, id uuid.UUID, in CategoryInput) (*domain.MenuCategory, error) {
	if in.Name == "" {
		return nil, domain.ErrValidation
	}
	if in.ParentID != nil {
		if err := f.checkNoCycle(ctx, id, *in.ParentID); err != nil {
			return nil, err
		}
	}
	c := &domain.MenuCategory{ID: id, Name: in.Name, NameI18n: in.NameI18n, ParentID: in.ParentID, DisplayOrder: in.DisplayOrder}
	if err := f.categories.Update(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (f *facade) DeleteCategory(ctx context.Context, id uuid.UUID) error {
	return f.categories.Delete(ctx, id)
}

// checkNoCycle rejects assigning parentID to category id when doing so would
// make id its own ancestor — a self-reference or a longer loop — which would
// make a parent-chain traversal spin forever. It walks the existing parent
// links up from parentID looking for id, bounded by the category count so a
// pre-existing cycle can't hang the check either.
func (f *facade) checkNoCycle(ctx context.Context, id, parentID uuid.UUID) error {
	if parentID == id {
		return domain.ErrValidation
	}
	cats, err := f.categories.List(ctx)
	if err != nil {
		return err
	}
	parent := make(map[uuid.UUID]*uuid.UUID, len(cats))
	for _, c := range cats {
		parent[c.ID] = c.ParentID
	}
	cur := &parentID
	for steps := 0; cur != nil && steps <= len(cats); steps++ {
		if *cur == id {
			return domain.ErrValidation
		}
		cur = parent[*cur]
	}
	return nil
}

// applyItem copies the non-nil fields of in onto m.
// checkBaseLanguage keeps the panel from creating a dish the guest can never
// see.
//
// Visibility rule (repository.baseRowsPredicate): the guest listing serves a
// venue's BASE rows — language NULL or 'ru' — because part of the imported data
// stores a translation as a separate row, and listing those next to their
// originals is the same dish twice. That rule is per VENUE, so a NEW dish
// labelled 'en' at a venue that already has base rows would be filtered out of
// every language at once, while still looking perfectly normal in the cabinet.
// Nobody would ever get a signal.
//
// So the write side enforces what the read side assumes: a dish row IS the base
// row, and translations go into the *_i18n maps (which is also the only place
// the panel and the app read them from). A non-base label is refused loudly with
// its own code instead of being silently rewritten to 'ru' — an editor who typed
// "en" meant something, and quietly storing the text as Russian would put the
// wrong words on the guest's screen.
//
// The one allowed non-base value is the one the row ALREADY has: the 124
// imported Kazakh copy rows are visible in the cabinet, and refusing to save an
// edit of a legacy row would make those dishes uneditable. Such a row is already
// out of the listing; keeping its label changes nothing for the guest.
func checkBaseLanguage(existing, in *string) error {
	if in == nil || isBaseLanguage(*in) {
		return nil
	}
	if existing != nil && canonicalLanguage(*existing) == canonicalLanguage(*in) {
		return nil
	}
	return domain.WithCode(domain.CodeMenuItemLanguageNotBase,
		fmt.Errorf("%w: a menu item is always the base (ru) row; put translations into name_i18n/description_i18n", domain.ErrValidation))
}

// isBaseLanguage reports whether the label marks a row the guest listing serves.
// It mirrors the SQL predicate (NULL or 'ru', case-insensitive) — the two must
// agree, or the panel would accept a dish the listing hides.
func isBaseLanguage(l string) bool {
	l = strings.TrimSpace(l)
	return l == "" || strings.EqualFold(l, domain.LocaleRU)
}

// canonicalLanguage folds the historical spellings together so that re-saving a
// legacy row does not fail on 'kz' vs 'kk' (the panel echoes back whatever it
// was given, and migration 0100 rewrote the stored value).
func canonicalLanguage(l string) string {
	if norm := domain.NormalizeLocale(l); norm != "" {
		return norm
	}
	return strings.ToLower(strings.TrimSpace(l))
}

func applyItem(m *domain.MenuItem, in ItemInput) {
	if in.Name != nil {
		m.Name = *in.Name
	}
	if in.NameI18n != nil {
		m.NameI18n = in.NameI18n
	}
	if in.Description != nil {
		m.Description = *in.Description
	}
	if in.DescriptionI18n != nil {
		m.DescriptionI18n = in.DescriptionI18n
	}
	if in.Price != nil {
		m.Price = *in.Price
	}
	if in.ImageURL != nil {
		m.ImageURL = in.ImageURL
	}
	if in.IsAvailable != nil {
		m.IsAvailable = *in.IsAvailable
	}
	if in.Category != nil {
		m.Category = in.Category
	}
	if in.CategoryI18n != nil {
		m.CategoryI18n = in.CategoryI18n
	}
	if in.Subcategory != nil {
		m.Subcategory = in.Subcategory
	}
	if in.SubcategoryI18n != nil {
		m.SubcategoryI18n = in.SubcategoryI18n
	}
	if in.PortionSize != nil {
		m.PortionSize = in.PortionSize
	}
	if in.PortionSizeI18n != nil {
		m.PortionSizeI18n = in.PortionSizeI18n
	}
	if in.Language != nil {
		// Normalize the label so the data stops disagreeing with the code
		// ('kz' from the old import vs 'kk' everywhere else — see migration
		// 0100). An unrecognized code is kept verbatim rather than dropped:
		// losing what the editor typed is worse than storing an odd label,
		// and the label no longer selects rows for anybody.
		lang := *in.Language
		if norm := domain.NormalizeLocale(lang); norm != "" {
			lang = norm
		}
		m.Language = &lang
	}
	if in.DisplayOrder != nil {
		m.DisplayOrder = in.DisplayOrder
	}
	if m.Price == "" {
		m.Price = "0"
	}
}

// tagsOf builds MenuItemTag rows from the input tag strings (nil → empty),
// de-duplicating so a body like ["halal","halal"] doesn't trip the
// UNIQUE(menu_item_id, tag) constraint (which would surface as a 500).
func tagsOf(itemID uuid.UUID, tags *[]string) []domain.MenuItemTag {
	if tags == nil {
		return nil
	}
	seen := make(map[string]bool, len(*tags))
	out := make([]domain.MenuItemTag, 0, len(*tags))
	for _, t := range *tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, domain.MenuItemTag{MenuItemID: itemID, Tag: t})
	}
	return out
}
