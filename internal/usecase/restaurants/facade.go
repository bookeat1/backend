package restaurants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes catalog reads and admin mutations.
type Facade interface {
	// List returns one page of the catalog plus the total number of venues
	// matching BOTH filters. vs (the server-computed venue state: open now /
	// accepts online bookings) is a separate argument on purpose — see
	// domain.VenueStateFilter for why it cannot travel inside the SQL filter.
	List(ctx context.Context, f domain.RestaurantFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error)
	// Search runs the full-text + fuzzy catalog search (a distinct endpoint
	// from List, which keeps its existing response shape untouched).
	Search(ctx context.Context, f domain.RestaurantSearchFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error)
	Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error)
	Categories(ctx context.Context) ([]domain.RestaurantCategory, error)
	Create(ctx context.Context, in SaveInput) (*domain.RestaurantAggregate, error)
	Update(ctx context.Context, id uuid.UUID, in SaveInput) (*domain.RestaurantAggregate, error)
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
	SubmitPartnership(ctx context.Context, in PartnershipInput) error
}

type facade struct {
	repo       domain.RestaurantRepository
	related    domain.RestaurantRelatedRepository
	categories domain.RestaurantCategoryRepository
	partners   domain.PartnershipRequestRepository
	tx         domain.TxManager

	// venue is the optional public-venue-state enrichment (see
	// WithVenueState). Nil unless wired; the catalog then simply omits the
	// schedule / bookability fields rather than guessing them.
	venue *VenueState

	// availability answers "could this venue seat the party on that date" for a
	// whole page at once. Optional: unwired, a request that ASKS to filter by
	// it is refused (see filterByAvailability) rather than answered with the
	// unfiltered catalog.
	availability availabilityFilter
}

// availabilityFilter is the minimal slice of the booking engine this package
// needs. Declared here, bound in bootstrap/deps.go to
// usecase/bookings.AvailabilitySearch, so the catalog keeps its one-way
// dependency on the booking layer down to one method.
type availabilityFilter interface {
	Filter(ctx context.Context, venues []domain.Restaurant, q domain.AvailabilitySearch) (map[uuid.UUID]bool, error)
}

// WithAvailabilityFilter enables the "гости + дата" catalog filter.
func WithAvailabilityFilter(a availabilityFilter) FacadeOption {
	return func(f *facade) { f.availability = a }
}

// FacadeOption configures optional facade dependencies without breaking the
// constructor's existing positional callers (tests pass none).
type FacadeOption func(*facade)

// WithVenueState enables the guest-facing venue state on the catalog reads: the
// structured weekly schedule, the server-computed "open now" flag, and the
// "can this venue take an online booking at all" flag. Left unwired, those JSON
// fields are absent — never guessed.
//
// The same *VenueState must be given to every other endpoint that serves
// domain.RestaurantListItem rows (today: usecase/favorites), or the same venue
// reads differently on two screens.
func WithVenueState(v *VenueState) FacadeOption {
	return func(f *facade) { f.venue = v }
}

// NewFacade constructs the restaurants Facade.
func NewFacade(
	repo domain.RestaurantRepository,
	related domain.RestaurantRelatedRepository,
	categories domain.RestaurantCategoryRepository,
	partners domain.PartnershipRequestRepository,
	tx domain.TxManager,
	opts ...FacadeOption,
) Facade {
	f := &facade{repo: repo, related: related, categories: categories, partners: partners, tx: tx}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// SaveInput carries mutable restaurant fields plus inline collections for
// create/update. Every scalar field and collection is a pointer so the facade
// can distinguish "absent from the request" (nil → preserve on Update) from
// "explicitly provided". On Update the facade loads the existing row and
// overlays only the provided fields (read-modify-write), so a PATCH that omits
// a field never wipes it — including server-managed columns this input can't
// even address (kwaaka_restaurant_id, hidden_from_home), which are always
// carried over from the stored row.
type SaveInput struct {
	CategoryID    *uuid.UUID
	Name          *string
	NameI18n      domain.I18n
	Description   *string
	CuisineType   *string
	Address       *string
	OpeningHours  *string
	City          *string
	PriceCategory *string
	// PriceMin / PriceMax are the average-check range in whole tenge. Applied
	// independently on Update (read-modify-write); the final merged pair is
	// validated by validatePriceRange (both-or-neither, 0 <= min <= max).
	PriceMin     *int
	PriceMax     *int
	Email        *string
	Phone        *string
	Latitude     *float64
	Longitude    *float64
	IsActive     *bool // nil = leave is_active unchanged (Update) / default true (Create)
	IsNew        *bool
	IsPopular    *bool
	IsPremium    *bool
	DisplayOrder *int

	Images      *[]domain.Image // nil = collection not provided (preserve on Update)
	Features    *[]domain.Feature
	Tags        *[]domain.Tag
	SocialLinks *[]domain.SocialLink
}

// PartnershipInput is a public partnership lead submission.
type PartnershipInput struct {
	RestaurantName string
	ContactName    string
	Email          string
	Phone          string
	Address        string
	CuisineType    *string
	Description    *string
	AdditionalInfo *string
}

func (f *facade) List(ctx context.Context, flt domain.RestaurantFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error) {
	if !vs.Active() {
		items, total, err := f.repo.ListActive(ctx, flt)
		if err != nil {
			return nil, 0, err
		}
		f.venue.AttachList(ctx, items)
		return items, total, nil
	}
	scan := flt
	scan.Unpaginated = true
	items, matched, err := f.repo.ListActive(ctx, scan)
	if err != nil {
		return nil, 0, err
	}
	f.venue.AttachList(ctx, items)
	return f.pageByVenueState(ctx, items, matched, vs, flt.Page, flt.PerPage)
}

func (f *facade) Search(ctx context.Context, flt domain.RestaurantSearchFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error) {
	if !vs.Active() {
		items, total, err := f.repo.Search(ctx, flt)
		if err != nil {
			return nil, 0, err
		}
		f.venue.AttachList(ctx, items)
		return items, total, nil
	}
	scan := flt
	scan.Unpaginated = true
	items, matched, err := f.repo.Search(ctx, scan)
	if err != nil {
		return nil, 0, err
	}
	f.venue.AttachList(ctx, items)
	return f.pageByVenueState(ctx, items, matched, vs, flt.Page, flt.PerPage)
}

// errVenueStateUnavailable is the one refusal for "you asked me to filter by
// the venue state and I could not compute it". 503 rather than a 200 with an
// unfiltered list, and a narrow code so the client can retry or fall back to
// browsing — but never present what it got as filtered.
func errVenueStateUnavailable() error {
	return domain.WithCode(domain.CodeCatalogVenueStateUnavailable,
		fmt.Errorf("%w: venue state could not be computed", domain.ErrUnavailable))
}

// pageByVenueState applies a domain.VenueStateFilter to an ALREADY ENRICHED,
// unpaginated candidate set and cuts the requested page out of what survives.
//
// The order matters and is the whole point of the two-phase read: SQL narrows
// (city / cuisine / price / text), Go decides open-now and bookability with the
// same code that publishes those fields, and only then does paging happen. Any
// other order gives a page-local answer — the exact defect the app worked around
// on the client, where "забронировать можно в 7 из 24" was true only while the
// whole catalog fitted in one page.
//
// The returned total is therefore the number of venues matching BOTH filters,
// which is what the client shows the guest.
func (f *facade) pageByVenueState(
	ctx context.Context,
	items []domain.RestaurantListItem, matched int,
	vs domain.VenueStateFilter, page, perPage int,
) ([]domain.RestaurantListItem, int, error) {
	if matched > len(items) {
		// The scan hit domain.CatalogScanLimit. Everything below is then
		// computed over a truncated set and the total under-reports. Loud in
		// the log rather than silently wrong-by-a-little; when this ever fires,
		// the filter has to move into SQL (materialized venue state), which is
		// a schema change and a separate decision.
		slog.Warn("catalog venue-state filter truncated by the scan limit",
			"matched", matched, "scanned", len(items), "limit", domain.CatalogScanLimit)
	}
	items, err := f.filterByAvailability(ctx, items, vs.Availability)
	if err != nil {
		return nil, 0, err
	}

	kept := make([]domain.RestaurantListItem, 0, len(items))
	for _, it := range items {
		if it.VenueState == nil {
			// The enrichment is optional and best-effort (a hours/tables read
			// that fails degrades the catalog to "hours unknown"). It cannot
			// degrade a REQUEST TO FILTER by it: serving the unfiltered list
			// under a filtered query is precisely the silent lie this task
			// removes. 503 + a code the client can act on instead.
			return nil, 0, errVenueStateUnavailable()
		}
		if vs.OpenNow != nil && it.VenueState.OpenNowUncomputed() {
			// Same refusal, one level finer. The venue's hours ARE known and
			// open-now still came back unanswered — the timezone would not load,
			// or the special-day read failed (VenueState.read degrades to
			// "no open_now" so a holiday closure is never ignored silently).
			// Counting such a venue as "not open" would drop it from
			// open_now=true, and when the failure hits the whole page the guest
			// would be shown an empty catalog as if nothing were open.
			return nil, 0, errVenueStateUnavailable()
		}
		if vs.Matches(it.VenueState) {
			kept = append(kept, it)
		}
	}
	total := len(kept)

	page, perPage = domain.NormalizePaging(page, perPage)
	start := (page - 1) * perPage
	if start >= total {
		return nil, total, nil
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return kept[start:end], total, nil
}

// filterByAvailability keeps only the venues that could really seat the party.
//
// It runs BEFORE the open-now / bookability filter and before paging, for the
// same reason those two do: a page-local answer is a wrong answer, and the
// total the guest is shown ("нашли 3 заведения") has to count what survived
// every filter.
//
// With no booking engine wired the request is REFUSED. Returning the unfiltered
// catalog under a "на двоих в пятницу" query is the same silent lie as
// publishing an uncomputed open_now: the guest would pick a venue from a list
// that promised free tables, and find out at the booking screen.
func (f *facade) filterByAvailability(
	ctx context.Context, items []domain.RestaurantListItem, q *domain.AvailabilitySearch,
) ([]domain.RestaurantListItem, error) {
	if q == nil {
		return items, nil
	}
	if f.availability == nil {
		return nil, domain.WithCode(domain.CodeCatalogAvailabilityUnavailable,
			fmt.Errorf("%w: availability filter is not configured", domain.ErrUnavailable))
	}
	if len(items) == 0 {
		return items, nil
	}
	venues := make([]domain.Restaurant, 0, len(items))
	for _, it := range items {
		venues = append(venues, it.Restaurant)
	}
	free, err := f.availability.Filter(ctx, venues, *q)
	if err != nil {
		// A validation error is the guest's (a broken date, zero guests) and
		// travels through as-is; anything else is ours, and it must not degrade
		// into an unfiltered list.
		if errors.Is(err, domain.ErrValidation) {
			return nil, err
		}
		slog.Error("catalog availability filter failed", "error", err)
		return nil, domain.WithCode(domain.CodeCatalogAvailabilityUnavailable,
			fmt.Errorf("%w: availability could not be computed", domain.ErrUnavailable))
	}
	kept := make([]domain.RestaurantListItem, 0, len(items))
	for _, it := range items {
		if free[it.ID] {
			kept = append(kept, it)
		}
	}
	return kept, nil
}

func (f *facade) Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	agg, err := f.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	f.venue.AttachOne(ctx, agg)
	return agg, nil
}

func (f *facade) Categories(ctx context.Context) ([]domain.RestaurantCategory, error) {
	return f.categories.List(ctx)
}

func (f *facade) Create(ctx context.Context, in SaveInput) (*domain.RestaurantAggregate, error) {
	if err := validateProvided(in); err != nil {
		return nil, err
	}
	rest := domain.Restaurant{ID: uuid.New(), IsActive: true}
	applyRestaurant(&rest, in)
	if err := validateRestaurant(rest); err != nil {
		return nil, err
	}
	if err := validatePriceRange(rest); err != nil {
		return nil, err
	}
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.repo.Create(ctx, &rest); err != nil {
			return err
		}
		return f.saveAllCollections(ctx, in, rest.ID)
	})
	if err != nil {
		return nil, err
	}
	return f.repo.GetByID(ctx, rest.ID)
}

func (f *facade) Update(ctx context.Context, id uuid.UUID, in SaveInput) (*domain.RestaurantAggregate, error) {
	if err := validateProvided(in); err != nil {
		return nil, err
	}
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		existing, err := f.repo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		// Read-modify-write: start from the stored row and overlay only the
		// fields the request actually provided. Untouched columns (including
		// is_active, kwaaka_restaurant_id and hidden_from_home) keep their
		// existing values instead of being reset to the zero value.
		rest := existing.Restaurant
		applyRestaurant(&rest, in)
		rest.ID = id
		if err := validatePriceRange(rest); err != nil {
			return err
		}
		if err := f.repo.Update(ctx, &rest); err != nil {
			return err
		}
		return f.saveProvidedCollections(ctx, in, id)
	})
	if err != nil {
		return nil, err
	}
	return f.repo.GetByID(ctx, id)
}

// saveAllCollections replaces all four inline collections, treating a nil
// pointer as an explicitly empty collection. Used by Create, where a
// brand-new restaurant has no prior rows to preserve.
func (f *facade) saveAllCollections(ctx context.Context, in SaveInput, rid uuid.UUID) error {
	if err := f.related.ReplaceImages(ctx, rid, deref(in.Images)); err != nil {
		return err
	}
	if err := f.related.ReplaceFeatures(ctx, rid, deref(in.Features)); err != nil {
		return err
	}
	if err := f.related.ReplaceTags(ctx, rid, deref(in.Tags)); err != nil {
		return err
	}
	return f.related.ReplaceSocialLinks(ctx, rid, deref(in.SocialLinks))
}

// saveProvidedCollections replaces only the collections explicitly present in
// in (non-nil pointer). Used by Update so that omitting a collection in a
// PATCH preserves its existing rows instead of wiping them.
func (f *facade) saveProvidedCollections(ctx context.Context, in SaveInput, rid uuid.UUID) error {
	if in.Images != nil {
		if err := f.related.ReplaceImages(ctx, rid, *in.Images); err != nil {
			return err
		}
	}
	if in.Features != nil {
		if err := f.related.ReplaceFeatures(ctx, rid, *in.Features); err != nil {
			return err
		}
	}
	if in.Tags != nil {
		if err := f.related.ReplaceTags(ctx, rid, *in.Tags); err != nil {
			return err
		}
	}
	if in.SocialLinks != nil {
		if err := f.related.ReplaceSocialLinks(ctx, rid, *in.SocialLinks); err != nil {
			return err
		}
	}
	return nil
}

// deref returns the empty/nil-slice value of *p, or nil if p is nil.
func deref[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

func (f *facade) SetActive(ctx context.Context, id uuid.UUID, active bool) error {
	return f.repo.SetActive(ctx, id, active)
}

func (f *facade) SubmitPartnership(ctx context.Context, in PartnershipInput) error {
	if strings.TrimSpace(in.RestaurantName) == "" || strings.TrimSpace(in.Email) == "" ||
		strings.TrimSpace(in.Phone) == "" || strings.TrimSpace(in.ContactName) == "" {
		return domain.ErrValidation
	}
	return f.partners.Create(ctx, &domain.PartnershipRequest{
		RestaurantName: in.RestaurantName, ContactName: in.ContactName, Email: in.Email,
		Phone: in.Phone, Address: in.Address, CuisineType: in.CuisineType,
		Description: in.Description, AdditionalInfo: in.AdditionalInfo, Status: "pending",
	})
}

// applyRestaurant overlays the fields present in in (non-nil) onto m, leaving
// everything else — including columns in isn't able to address — untouched.
func applyRestaurant(m *domain.Restaurant, in SaveInput) {
	if in.CategoryID != nil {
		m.CategoryID = in.CategoryID
	}
	if in.Name != nil {
		m.Name = *in.Name
	}
	if in.NameI18n != nil {
		m.NameI18n = in.NameI18n
	}
	if in.Description != nil {
		m.Description = *in.Description
	}
	if in.CuisineType != nil {
		m.CuisineType = *in.CuisineType
	}
	if in.Address != nil {
		m.Address = *in.Address
	}
	if in.OpeningHours != nil {
		m.OpeningHours = *in.OpeningHours
	}
	if in.City != nil {
		m.City = domain.City(*in.City)
	}
	if in.PriceCategory != nil {
		m.PriceCategory = domain.PriceCategory(*in.PriceCategory)
	}
	if in.Email != nil {
		m.Email = *in.Email
	}
	if in.Phone != nil {
		m.Phone = *in.Phone
	}
	if in.Latitude != nil {
		m.Latitude = in.Latitude
	}
	if in.Longitude != nil {
		m.Longitude = in.Longitude
	}
	if in.IsActive != nil {
		m.IsActive = *in.IsActive
	}
	if in.IsNew != nil {
		m.IsNew = in.IsNew
	}
	if in.IsPopular != nil {
		m.IsPopular = in.IsPopular
	}
	if in.IsPremium != nil {
		m.IsPremium = in.IsPremium
	}
	if in.DisplayOrder != nil {
		m.DisplayOrder = in.DisplayOrder
	}
	if in.PriceMin != nil {
		m.PriceMin = in.PriceMin
	}
	if in.PriceMax != nil {
		m.PriceMax = in.PriceMax
	}
}

// validateProvided rejects invalid values for the enumerated/required fields
// that are actually present in in. It runs before both Create and Update so a
// bad value fails fast (422) without a DB round-trip; on Update the fields the
// request omits keep the stored row's already-valid values.
func validateProvided(in SaveInput) error {
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return domain.ErrValidation
	}
	if in.City != nil && !domain.City(*in.City).Valid() {
		return domain.ErrValidation
	}
	if in.PriceCategory != nil && !domain.PriceCategory(*in.PriceCategory).Valid() {
		return domain.ErrValidation
	}
	return nil
}

// validatePriceRange enforces the average-check bounds on the FINAL restaurant
// row — i.e. AFTER the PATCH merge, so a request that sets only price_min while
// the stored row already has a price_max is judged against the merged pair, not
// the lone provided field. The rule mirrors migration 0068's CHECK exactly:
// both bounds unset (no range) is fine, otherwise both must be set with
// 0 <= price_min <= price_max. A half-set pair is rejected here as 422 rather
// than deferred to the DB constraint. Validating the merged row (not the raw
// input) is deliberate: it is the only place that knows the effective pair.
func validatePriceRange(m domain.Restaurant) error {
	if m.PriceMin == nil && m.PriceMax == nil {
		return nil
	}
	if m.PriceMin == nil || m.PriceMax == nil {
		return domain.ErrValidation
	}
	if *m.PriceMin < 0 || *m.PriceMax < *m.PriceMin {
		return domain.ErrValidation
	}
	return nil
}

// validateRestaurant enforces that a fully-built restaurant has the required
// enumerated fields set. Used by Create (where name/city/price must be present).
func validateRestaurant(r domain.Restaurant) error {
	if strings.TrimSpace(r.Name) == "" || !r.City.Valid() || !r.PriceCategory.Valid() {
		return domain.ErrValidation
	}
	return nil
}
