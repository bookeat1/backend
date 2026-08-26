// Package promos is the application logic for restaurant promos (Ф2). Same
// shape as usecase/events: admin CRUD gated by PermRestaurantManage at the
// promo's own restaurant (superadmin bypasses), and a public listing that
// shows only published promos whose validity window contains now.
package promos

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for the admin CRUD actions.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant".
// Bound to restaurants.ManagerUseCase in bootstrap.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// feedModerator pulls an item off the main-screen feed when its content
// changes. Minimal local port (bound to the feed repository in bootstrap): the
// promos usecase must not know the whole FeedRepository, only this one effect.
type feedModerator interface {
	DemoteAfterContentEdit(ctx context.Context, kind domain.FeedItemKind, itemID uuid.UUID) error
}

// Facade exposes admin CRUD and public read operations for promos.
type Facade interface {
	Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Promo, error)
	Update(ctx context.Context, actor Actor, promoID uuid.UUID, in UpdateInput) (*domain.Promo, error)
	Delete(ctx context.Context, actor Actor, promoID uuid.UUID) error
	GetAdmin(ctx context.Context, actor Actor, promoID uuid.UUID) (*domain.Promo, error)
	ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error)
	// ListPlatformAdmin returns the PLATFORM's own promos (no venue), any
	// status, for the platform cabinet. Authorized by
	// domain.CanManagePlatformContent — there is no restaurant to check a
	// per-venue permission at.
	ListPlatformAdmin(ctx context.Context, actor Actor, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error)

	// ListPublic returns a restaurant's published promos whose validity window
	// contains now, paginated. No authorization.
	ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Promo, int, error)
	// ListPublicActive is the cross-venue guest listing — the promo twin of
	// events.ListPublicUpcoming, and the only public read that can show a
	// PLATFORM promo (one reached through no restaurant's path). No
	// authorization.
	ListPublicActive(ctx context.Context, f domain.PublicPromoFilter) ([]domain.PromoListItem, int, error)
	// GetPublicDetail returns ONE promo that a guest may see right now, with
	// its venue when it has one. No authorization.
	GetPublicDetail(ctx context.Context, promoID uuid.UUID) (*domain.PromoListItem, error)
}

// CreateInput carries a new promo's fields. Status defaults to draft when empty.
type CreateInput struct {
	// RestaurantID is the venue running the offer. nil creates a PLATFORM
	// promo, which only domain.CanManagePlatformContent roles may do.
	RestaurantID    *uuid.UUID
	Title           string
	TitleI18n       domain.I18n
	Description     string
	DescriptionI18n domain.I18n
	StartsAt        time.Time
	EndsAt          time.Time
	Terms           string
	CoverImageURL   *string
	DiscountPercent *int
	Status          domain.PromoStatus
	// City overrides the city the promo is shown in — the same field, the same
	// meaning and the same strictness as events.CreateInput.City: nil/blank
	// means "wherever the venue is" (and, for a platform promo, "everywhere"),
	// a value is resolved through the city dictionary, and an unknown or hidden
	// city is ErrValidation.
	City *string
	// Images — галерея акции в порядке редактора, без обложки.
	Images []string
}

// UpdateInput carries a promo's mutable fields (full replace).
type UpdateInput struct {
	Title           string
	TitleI18n       domain.I18n
	Description     string
	DescriptionI18n domain.I18n
	StartsAt        time.Time
	EndsAt          time.Time
	Terms           string
	CoverImageURL   *string
	DiscountPercent *int
	Status          domain.PromoStatus
	// City replaces the city override, full replace like the rest of this
	// struct: nil or blank CLEARS it and the promo goes back to following its
	// venue — which is exactly what every promo did before migration 0085, so
	// an older cabinet build that never sends the field keeps working.
	City *string
	// Images заменяет галерею целиком; пустой список её очищает.
	Images []string
}

// cityResolver is the minimal slice of usecase/cities this package needs:
// "which dictionary entry does this written spelling mean". Declared here and
// bound in bootstrap/deps.go — the same seam usecase/events uses, deliberately
// identical so the two content types cannot disagree about what a city is.
//
// A nil *domain.CityEntry with a nil error means "no such city": a normal
// answer for a filter value typed by a client, a validation error on a write.
type cityResolver interface {
	Resolve(ctx context.Context, raw string) (*domain.CityEntry, error)
}

type facade struct {
	repo  domain.PromoRepository
	perms permissionChecker
	feed  feedModerator
	// cities is the optional city-dictionary resolver (see WithCityResolver).
	// Nil unless wired; ?city= then compares the raw string to the stored
	// spelling, and a written override is validated against the two legacy
	// constants — the pre-dictionary behaviour, never a 500.
	cities cityResolver
	clock  func() time.Time
}

// Option tunes the facade. Variadic so every existing positional caller (and
// test) keeps compiling — the same pattern usecase/events uses.
type Option func(*facade)

// WithCityResolver teaches the promos usecase the city dictionary (migration
// 0081): ?city= accepts a code, an alias or a historical spelling, and a
// promo's own city override is validated against the dictionary instead of two
// constants compiled into the binary.
func WithCityResolver(r cityResolver) Option {
	return func(f *facade) { f.cities = r }
}

// NewFacade constructs the promos Facade.
func NewFacade(repo domain.PromoRepository, perms permissionChecker, feed feedModerator, opts ...Option) Facade {
	f := &facade{repo: repo, perms: perms, feed: feed, clock: time.Now}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (f *facade) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Promo, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = domain.PromoDraft
	}
	city, err := f.cityOverride(ctx, in.City)
	if err != nil {
		return nil, err
	}
	p := &domain.Promo{
		RestaurantID:    in.RestaurantID,
		City:            city,
		Title:           strings.TrimSpace(in.Title),
		TitleI18n:       in.TitleI18n,
		Description:     in.Description,
		DescriptionI18n: in.DescriptionI18n,
		StartsAt:        in.StartsAt,
		EndsAt:          in.EndsAt,
		Terms:           in.Terms,
		CoverImageURL:   in.CoverImageURL,
		DiscountPercent: in.DiscountPercent,
		Status:          status,
	}
	if err := validatePromo(p); err != nil {
		return nil, err
	}
	if err := f.repo.Create(ctx, p); err != nil {
		return nil, err
	}
	// Галерея — отдельная таблица, пишется после того, как акция получила id.
	// Ошибка тут не отменяет саму акцию: она уже создана и показывается с
	// обложкой (см. тот же порядок в usecase/events).
	if err := f.repo.ReplaceImages(ctx, p.ID, normalizeImages(in.Images)); err != nil {
		return nil, err
	}
	p.Images = normalizeImages(in.Images)
	return p, nil
}

func (f *facade) Update(ctx context.Context, actor Actor, promoID uuid.UUID, in UpdateInput) (*domain.Promo, error) {
	p, err := f.repo.GetByID(ctx, promoID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, p.RestaurantID); err != nil {
		return nil, err
	}
	// Whether the CARD's content actually changed is decided before anything is
	// overwritten. Update carries Status too, and hiding then re-publishing an
	// approved promo goes through this same method — demoting for that would
	// send a venue back to the moderation queue for touching nothing a
	// moderator ever read.
	city, err := f.cityOverride(ctx, in.City)
	if err != nil {
		return nil, err
	}
	// The city override is moderated content too: it decides WHICH city's main
	// screen the approved card can reach. Compared on the RESOLVED value, so
	// re-saving «almaty» over a stored «Алматы» is not an edit.
	contentChanged := promoContentChanged(*p, in) || !cityPtrEqual(city, p.City)

	p.Title = strings.TrimSpace(in.Title)
	p.TitleI18n = in.TitleI18n
	p.Description = in.Description
	p.DescriptionI18n = in.DescriptionI18n
	p.StartsAt = in.StartsAt
	p.EndsAt = in.EndsAt
	p.Terms = in.Terms
	p.CoverImageURL = in.CoverImageURL
	p.DiscountPercent = in.DiscountPercent
	p.Status = in.Status
	p.City = city
	if err := validatePromo(p); err != nil {
		return nil, err
	}
	// Demote BEFORE writing the new content, not after: the platform approved
	// specific words, so changing them invalidates the decision. Ordered this
	// way the failure modes are both safe — a failed edit after a successful
	// demotion only costs the venue a re-review, whereas a failed demotion
	// after a successful edit would leave unreviewed text live on the main
	// screen. A transaction is deliberately not used: the safe ordering already
	// gives the guarantee that matters, without dragging a tx manager into a
	// simple CRUD facade. The residual window (a moderator approving in the
	// milliseconds between the demotion and the write) is known, self-healing on
	// the next edit, and judged not worth a tx here.
	if contentChanged {
		if err := f.feed.DemoteAfterContentEdit(ctx, domain.FeedItemPromo, promoID); err != nil {
			return nil, err
		}
	}
	if err := f.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	if err := f.repo.ReplaceImages(ctx, p.ID, normalizeImages(in.Images)); err != nil {
		return nil, err
	}
	p.Images = normalizeImages(in.Images)
	return p, nil
}

func (f *facade) Delete(ctx context.Context, actor Actor, promoID uuid.UUID) error {
	p, err := f.repo.GetByID(ctx, promoID)
	if err != nil {
		return err
	}
	if err := f.authorize(ctx, actor, p.RestaurantID); err != nil {
		return err
	}
	return f.repo.Delete(ctx, promoID)
}

func (f *facade) GetAdmin(ctx context.Context, actor Actor, promoID uuid.UUID) (*domain.Promo, error) {
	p, err := f.repo.GetByID(ctx, promoID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, p.RestaurantID); err != nil {
		return nil, err
	}
	if byID, err := f.repo.ImagesByPromo(ctx, []uuid.UUID{p.ID}); err == nil {
		p.Images = byID[p.ID]
	}
	return p, nil
}

// normalizeImages — то же правило, что у событий: пустые строки выбрасываем,
// длину ограничиваем.
func normalizeImages(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			out = append(out, trimmed)
		}
		if len(out) == maxGalleryImages {
			break
		}
	}
	return out
}

// maxGalleryImages — потолок на галерею одной акции.
const maxGalleryImages = 20

func (f *facade) ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error) {
	if err := f.authorize(ctx, actor, &restaurantID); err != nil {
		return nil, 0, err
	}
	return f.repo.ListByRestaurant(ctx, restaurantID, statuses, page, perPage)
}

func (f *facade) ListPlatformAdmin(ctx context.Context, actor Actor, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error) {
	if err := authorizePlatformContent(actor); err != nil {
		return nil, 0, err
	}
	return f.repo.ListPlatform(ctx, statuses, page, perPage)
}

// ListPublicActive reads the cross-venue listing. Visibility lives in the
// repository query so no filter here can widen it; this method only resolves
// the caller's city through the dictionary and supplies the clock.
func (f *facade) ListPublicActive(ctx context.Context, flt domain.PublicPromoFilter) ([]domain.PromoListItem, int, error) {
	flt.City = f.canonicalCity(ctx, flt.City)
	return f.repo.ListPublicActive(ctx, flt, f.clock())
}

// GetPublicDetail reads one promo's own page under the listing's exact
// visibility rule.
func (f *facade) GetPublicDetail(ctx context.Context, promoID uuid.UUID) (*domain.PromoListItem, error) {
	it, err := f.repo.GetPublic(ctx, promoID, f.clock())
	if err != nil {
		return nil, err
	}
	if byID, err := f.repo.ImagesByPromo(ctx, []uuid.UUID{it.ID}); err == nil {
		it.Images = byID[it.ID]
	}
	return it, nil
}

func (f *facade) ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Promo, int, error) {
	return f.repo.ListActive(ctx, restaurantID, f.clock(), page, perPage)
}

// authorize is the ONE gate every promo mutation goes through — twin of
// usecase/events.authorize, and for the same reason:
//
//   - a venue-bound promo → PermRestaurantManage at THAT restaurant, superadmin
//     bypasses. Unchanged.
//   - a PLATFORM promo (nil) → no restaurant exists to hold a permission at, so
//     the global policy decides (domain.CanManagePlatformContent).
//
// The owner is always read off the STORED promo, never off caller input.
func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID *uuid.UUID) error {
	if restaurantID == nil {
		return authorizePlatformContent(actor)
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, *restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's promos", domain.ErrForbidden)
	}
	return nil
}

// authorizePlatformContent gates the platform's own акции. One policy function,
// one call site per operation — widening it to a marketer role is an edit to
// domain.PlatformContentRoles and nothing else.
func authorizePlatformContent(actor Actor) error {
	if !domain.CanManagePlatformContent(actor.Role) {
		return fmt.Errorf("%w: only the platform may manage promos that belong to no venue", domain.ErrForbidden)
	}
	return nil
}

// cityOverride validates and canonicalizes the city an EDITOR typed on the
// promo itself. Copied deliberately from usecase/events.cityOverride: two
// content types that share one ?city= filter must not disagree about what a
// city is, and a shared helper would mean one usecase importing the other.
//
//   - nil or blank → nil: no override, the promo follows its venue (and a
//     platform promo with no override runs in every city);
//   - a HIDDEN city cannot be assigned — hiding a city must actually stop it
//     spreading;
//   - the stored value is the dictionary's spelling, so the response echoes
//     what the database trigger would have normalized anyway.
func (f *facade) cityOverride(ctx context.Context, raw *string) (*domain.City, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	v := strings.TrimSpace(*raw)
	if f.cities == nil {
		c := domain.City(v)
		if !c.Valid() {
			return nil, fmt.Errorf("%w: unknown city %q", domain.ErrValidation, v)
		}
		return &c, nil
	}
	entry, err := f.cities.Resolve(ctx, v)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: unknown city %q", domain.ErrValidation, v)
	}
	if !entry.IsActive {
		return nil, fmt.Errorf("%w: city %q is hidden", domain.ErrValidation, entry.Code)
	}
	c := domain.City(entry.Name)
	return &c, nil
}

// canonicalCity turns whatever a client put in ?city= into the spelling the
// listing compares against. It never fails the request: an unknown value is
// passed through and simply matches nothing, and a resolver ERROR is logged and
// passed through too — a dictionary outage must not turn a browsable list into
// a 500. Same contract as usecase/events.canonicalCity.
func (f *facade) canonicalCity(ctx context.Context, in *domain.City) *domain.City {
	if in == nil || f.cities == nil || strings.TrimSpace(string(*in)) == "" {
		return in
	}
	entry, err := f.cities.Resolve(ctx, string(*in))
	if err != nil {
		slog.Warn("city dictionary lookup failed, filtering promos by the raw value",
			"city", string(*in), "error", err)
		return in
	}
	if entry == nil {
		return in
	}
	v := domain.City(entry.Name)
	return &v
}

// cityPtrEqual compares two optional city overrides; nil equals only nil.
func cityPtrEqual(a, b *domain.City) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func validatePromo(p *domain.Promo) error {
	if p.Title == "" {
		return fmt.Errorf("%w: title is required", domain.ErrValidation)
	}
	if !p.Status.Valid() {
		return fmt.Errorf("%w: unknown promo status %q", domain.ErrValidation, p.Status)
	}
	if p.StartsAt.IsZero() || p.EndsAt.IsZero() {
		return fmt.Errorf("%w: starts_at and ends_at are required", domain.ErrValidation)
	}
	if !p.EndsAt.After(p.StartsAt) {
		return fmt.Errorf("%w: ends_at must be after starts_at", domain.ErrValidation)
	}
	// The DB CHECK is the last line of defense; validating here turns a raw
	// constraint violation into a clean 422 with a readable message. Nil is
	// valid (no discount badge); a set value must be a real percentage.
	if p.DiscountPercent != nil && (*p.DiscountPercent < 0 || *p.DiscountPercent > 100) {
		return fmt.Errorf("%w: discount_percent must be between 0 and 100", domain.ErrValidation)
	}
	return nil
}

// promoContentChanged reports whether this update touches anything a moderator
// actually reviewed: the words shown on the card and the window it runs in.
// Status is excluded on purpose — publishing or hiding is the venue's own
// lever over its card and changes nothing a moderator read.
func promoContentChanged(cur domain.Promo, in UpdateInput) bool {
	return strings.TrimSpace(in.Title) != cur.Title ||
		in.Description != cur.Description ||
		in.Terms != cur.Terms ||
		!strPtrEqual(in.CoverImageURL, cur.CoverImageURL) ||
		!intPtrEqual(in.DiscountPercent, cur.DiscountPercent) ||
		!in.StartsAt.Equal(cur.StartsAt) ||
		!in.EndsAt.Equal(cur.EndsAt) ||
		!i18nEqual(in.TitleI18n, cur.TitleI18n) ||
		!i18nEqual(in.DescriptionI18n, cur.DescriptionI18n)
}

// strPtrEqual compares two optional strings by value: two nils are equal, and a
// nil never equals a set value ("the picture was removed" IS an edit).
func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// intPtrEqual compares two optional ints by value: two nils are equal, and a
// nil never equals a set value ("the discount was removed" IS an edit).
func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// i18nEqual compares two localized maps by content: a nil map and an empty one
// mean the same thing to a reader, so they must not count as an edit.
func i18nEqual(a, b domain.I18n) bool {
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
