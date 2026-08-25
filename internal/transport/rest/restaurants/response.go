package restaurants

import (
	"time"

	"backend-core/internal/domain"
)

type restaurantResponse struct {
	ID          string            `json:"id"`
	CategoryID  *string           `json:"category_id"`
	Name        string            `json:"name"`
	NameI18n    map[string]string `json:"name_i18n,omitempty"`
	Description string            `json:"description"`
	// CuisineType is the LEGACY single-string cuisine field: the venue's
	// cuisines joined with ", ". It is NOT removed and never will be silently:
	// the store builds (1.4 live, 1.5 in review) read exactly this string and
	// send it back as a filter value. `cuisines` below is the structured form
	// new clients should read.
	CuisineType string `json:"cuisine_type"`
	// Cuisines is the venue's cuisine set from the platform dictionary, in the
	// venue's own order (first = main). Omitted entirely when the venue has no
	// dictionary links yet — a venue whose historical value is still awaiting a
	// manual split has cuisine_type and nothing here, and an empty array would
	// read as "this venue declared no cuisine", which is a different statement.
	Cuisines      []cuisineResponse `json:"cuisines,omitempty"`
	Address       string            `json:"address"`
	OpeningHours  string            `json:"opening_hours"`
	City          string            `json:"city"`
	PriceCategory string            `json:"price_category"`
	// PriceRange is the numeric average-check range in whole tenge, shown next
	// to the categorical PriceCategory. A pointer with omitempty so it is absent
	// entirely when the venue has not declared a range (both bounds NULL) —
	// never serialized as a 0–0.
	PriceRange   *priceRangeResponse `json:"price_range,omitempty"`
	Email        string              `json:"email"`
	Phone        string              `json:"phone"`
	Latitude     *float64            `json:"latitude"`
	Longitude    *float64            `json:"longitude"`
	IsActive     bool                `json:"is_active"`
	IsNew        *bool               `json:"is_new"`
	IsPopular    *bool               `json:"is_popular"`
	IsPremium    *bool               `json:"is_premium"`
	DisplayOrder *int                `json:"display_order"`
	PrimaryImage *string             `json:"primary_image,omitempty"`
	Images       []imageResponse     `json:"images,omitempty"`
	Features     []featureResponse   `json:"features,omitempty"`
	Tags         []tagResponse       `json:"tags,omitempty"`
	SocialLinks  []socialResponse    `json:"social_links,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	// IsFavorite is nil for an anonymous caller (omitted from the JSON
	// entirely) and an explicit true/false for an authenticated one — a
	// pointer so "not favorited" and "we don't know because you're not
	// logged in" are never confused with each other.
	IsFavorite *bool `json:"is_favorite,omitempty"`
	// Schedule is the STRUCTURED weekly schedule plus a server-computed
	// "open now", replacing the client's habit of parsing the free-text
	// `opening_hours` string (which stays, untouched, for older clients).
	// Absent when the venue has no usable working-hours rows: the client must
	// then show "hours unknown", never "closed".
	Schedule *scheduleResponse `json:"schedule,omitempty"`
	// AcceptsOnlineBookings tells the guest, BEFORE they pick a date, whether
	// this venue can be booked through the app at all. Absent only when the
	// server did not compute the venue state (never guessed).
	AcceptsOnlineBookings *bool `json:"accepts_online_bookings,omitempty"`
}

// scheduleResponse is the venue's regular weekly hours in a shape a client
// renders directly. Days holds ONLY the weekdays the venue has a usable row
// for — a missing weekday is unknown, not closed.
type scheduleResponse struct {
	// Timezone is the IANA zone `open_now` was computed in — the venue's own.
	Timezone string `json:"timezone"`
	// OpenNow is absent when the timezone could not be resolved on the server,
	// or when the venue's special days could not be read — "open now" computed
	// without the exceptions is exactly the lie a holiday closure produces.
	OpenNow *bool                 `json:"open_now,omitempty"`
	Days    []scheduleDayResponse `json:"days"`
	// Exceptions are the venue's SPECIAL DAYS for the covered window: dates
	// that override the weekday row in Days — a holiday closure, one-off hours,
	// or an opening on a weekday the venue is normally shut. A date listed here
	// wins over Days for that date, and the booking engine resolves it through
	// the same rule, so a date shown as closed sells no slots.
	//
	// THE WINDOW IS THE PRESENCE SIGNAL, not the array: exceptions_from /
	// exceptions_until are set whenever the server looked, and the list is then
	// simply omitted when there is nothing in it (an empty JSON array carries
	// no information the window does not already give). No window at all means
	// the server does NOT know the venue's special days — never "there are
	// none", the same rule `days` already follows (absent ≠ closed).
	Exceptions      []scheduleExceptionResponse `json:"exceptions,omitempty"`
	ExceptionsFrom  string                      `json:"exceptions_from,omitempty"`
	ExceptionsUntil string                      `json:"exceptions_until,omitempty"`
}

// scheduleExceptionResponse is one dated departure from the weekly schedule. It
// mirrors scheduleDayResponse field for field (minus day_of_week, plus the
// date and the admin's note) so a client renders it with the same code.
type scheduleExceptionResponse struct {
	// Date is "YYYY-MM-DD" in the venue's own timezone.
	Date   string `json:"date"`
	IsOpen bool   `json:"is_open"`
	// OpensAt/ClosesAt are "HH:MM" venue-local wall clock, empty when closed.
	OpensAt  string `json:"opens_at"`
	ClosesAt string `json:"closes_at"`
	// ClosesNextDay marks a special-day window that runs past midnight.
	ClosesNextDay bool `json:"closes_next_day"`
	// Note is what the venue wrote about the day ("Новогодние каникулы"),
	// empty when it wrote nothing.
	Note string `json:"note,omitempty"`
}

type scheduleDayResponse struct {
	// DayOfWeek is 0 = Sunday .. 6 = Saturday.
	DayOfWeek int  `json:"day_of_week"`
	IsOpen    bool `json:"is_open"`
	// OpensAt/ClosesAt are "HH:MM" venue-local wall clock, empty when closed.
	OpensAt  string `json:"opens_at"`
	ClosesAt string `json:"closes_at"`
	// ClosesNextDay marks a window that runs past midnight (11:00 → 01:00):
	// ClosesAt belongs to the FOLLOWING calendar day.
	ClosesNextDay bool `json:"closes_next_day"`
}

// applyVenueState maps the server-computed venue state onto the public payload.
// A nil state leaves both fields absent.
func applyVenueState(resp *restaurantResponse, st *domain.PublicVenueState) {
	if st == nil {
		return
	}
	accepts := st.AcceptsOnlineBookings
	resp.AcceptsOnlineBookings = &accepts
	if st.Schedule == nil {
		return
	}
	days := make([]scheduleDayResponse, 0, len(st.Schedule.Days))
	for _, d := range st.Schedule.Days {
		days = append(days, scheduleDayResponse{
			DayOfWeek: d.DayOfWeek, IsOpen: d.IsOpen,
			OpensAt: d.OpenTime, ClosesAt: d.CloseTime, ClosesNextDay: d.ClosesNextDay,
		})
	}
	exceptions := make([]scheduleExceptionResponse, 0, len(st.Schedule.Exceptions))
	for _, e := range st.Schedule.Exceptions {
		exceptions = append(exceptions, scheduleExceptionResponse{
			Date: e.Date, IsOpen: e.IsOpen,
			OpensAt: e.OpenTime, ClosesAt: e.CloseTime, ClosesNextDay: e.ClosesNextDay,
			Note: e.Note,
		})
	}
	resp.Schedule = &scheduleResponse{
		Timezone:        st.Schedule.Timezone,
		OpenNow:         st.Schedule.OpenNow,
		Days:            days,
		Exceptions:      exceptions,
		ExceptionsFrom:  st.Schedule.ExceptionsFrom,
		ExceptionsUntil: st.Schedule.ExceptionsUntil,
	}
}

// priceRangeResponse is the venue's average-check range in whole tenge. Both
// bounds are always present together (the domain guarantees both-or-neither),
// so they are plain ints, not pointers — the whole object is omitted upstream
// when there is no range.
type priceRangeResponse struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type imageResponse struct {
	ID        string `json:"id"`
	ImageURL  string `json:"image_url"`
	IsPrimary bool   `json:"is_primary"`
}

// featureResponse is one of the venue's features. The shape is unchanged from
// when these rows were free text (migration 0082 dropped that table): same
// three fields, so a client mapper written against the old payload still
// parses. `code` is additive — it is the stable, language-independent key the
// filter travels by, and the app keys its icon off it.
type featureResponse struct {
	ID       string            `json:"id"`
	Code     string            `json:"code,omitempty"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
}

// featuresToResponse renders a venue's feature set, resolving each name into
// the requested locale (falling back to the Russian base, never to a blank).
func featuresToResponse(items []domain.VenueFeature, lang string) []featureResponse {
	if len(items) == 0 {
		return nil
	}
	out := make([]featureResponse, 0, len(items))
	for _, f := range items {
		out = append(out, featureResponse{
			ID:       f.ID.String(),
			Code:     f.Code,
			Name:     f.NameI18n.Resolve(lang, f.Name),
			NameI18n: f.NameI18n,
		})
	}
	return out
}

type tagResponse struct {
	ID          string            `json:"id"`
	TagName     string            `json:"tag_name"`
	TagNameI18n map[string]string `json:"tag_name_i18n,omitempty"`
}
type socialResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

// cuisineResponse is one dictionary entry as every client reads it — the same
// shape in the venue payload and in GET /cuisines, so a client has one mapper.
type cuisineResponse struct {
	ID       string            `json:"id"`
	Code     string            `json:"code"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
	ImageURL *string           `json:"image_url,omitempty"`
}

func cuisineToResponse(c domain.Cuisine, lang string) cuisineResponse {
	return cuisineResponse{
		ID: c.ID.String(), Code: c.Code,
		Name:     c.NameI18n.Resolve(lang, c.Name),
		NameI18n: c.NameI18n, ImageURL: c.ImageURL,
	}
}

func cuisinesToResponse(cs []domain.Cuisine, lang string) []cuisineResponse {
	if len(cs) == 0 {
		return nil
	}
	out := make([]cuisineResponse, 0, len(cs))
	for _, c := range cs {
		out = append(out, cuisineToResponse(c, lang))
	}
	return out
}

type categoryResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
}
type managerResponse struct {
	ID            string  `json:"id"`
	RestaurantID  string  `json:"restaurant_id"`
	UserID        string  `json:"user_id"`
	Role          string  `json:"role"`
	WhatsappOptIn bool    `json:"whatsapp_opt_in"`
	WhatsappPhone *string `json:"whatsapp_phone"`
}

// baseFromDomain builds the response shape shared by the list/detail routes.
// lang is the resolved caller locale ("" when the caller asked for nothing —
// see resolveLocale). When lang is non-empty, the scalar fields fall back to
// the i18n column's translation for lang, or to the base value when that
// translation is missing (domain.I18n.Resolve); when lang is empty, the
// scalar fields are the base value exactly as before, so a caller that never
// asks for a language sees byte-identical output to before this feature.
func baseFromDomain(r domain.Restaurant, lang string) restaurantResponse {
	var cat *string
	if r.CategoryID != nil {
		s := r.CategoryID.String()
		cat = &s
	}
	var priceRange *priceRangeResponse
	if r.PriceMin != nil && r.PriceMax != nil {
		priceRange = &priceRangeResponse{Min: *r.PriceMin, Max: *r.PriceMax}
	}
	return restaurantResponse{
		ID: r.ID.String(), CategoryID: cat,
		Name:         r.NameI18n.Resolve(lang, r.Name),
		NameI18n:     r.NameI18n,
		Description:  r.DescriptionI18n.Resolve(lang, r.Description),
		CuisineType:  r.CuisineTypeI18n.Resolve(lang, r.CuisineType),
		Address:      r.AddressI18n.Resolve(lang, r.Address),
		OpeningHours: r.OpeningHoursI18n.Resolve(lang, r.OpeningHours),
		City:         string(r.City), PriceCategory: string(r.PriceCategory),
		PriceRange: priceRange,
		Email:      r.Email, Phone: r.Phone, Latitude: r.Latitude, Longitude: r.Longitude,
		IsActive: r.IsActive, IsNew: r.IsNew, IsPopular: r.IsPopular, IsPremium: r.IsPremium,
		DisplayOrder: r.DisplayOrder, CreatedAt: r.CreatedAt,
	}
}

func listItemToResponse(it domain.RestaurantListItem, lang string) restaurantResponse {
	resp := baseFromDomain(it.Restaurant, lang)
	resp.PrimaryImage = it.PrimaryImage
	resp.Cuisines = cuisinesToResponse(it.Cuisines, lang)
	applyDerivedCuisineType(&resp, it.Cuisines, lang)
	// Features travel with the LISTING too, not just the detail read: a card
	// shown under a «Wi-Fi» filter has to be able to say why it matched.
	resp.Features = featuresToResponse(it.Features, lang)
	applyVenueState(&resp, it.VenueState)
	return resp
}

// PublicListItem builds the same public JSON shape the catalog listing uses,
// for a domain.RestaurantListItem read from elsewhere (the favorites
// transport package reads a user's bookmarked restaurants through
// favorites.Facade.List and must serialize them identically to the main
// catalog listing). Returned as the concrete response value (not a pointer)
// so json.Marshal/gin's c.JSON serialize it exactly like every other route in
// this package.
func PublicListItem(it domain.RestaurantListItem, lang string) any {
	return listItemToResponse(it, lang)
}

func aggregateToResponse(a *domain.RestaurantAggregate, lang string) restaurantResponse {
	resp := baseFromDomain(a.Restaurant, lang)
	resp.Cuisines = cuisinesToResponse(a.Cuisines, lang)
	applyDerivedCuisineType(&resp, a.Cuisines, lang)
	for _, i := range a.Images {
		resp.Images = append(resp.Images, imageResponse{ID: i.ID.String(), ImageURL: i.ImageURL, IsPrimary: i.IsPrimary})
		if i.IsPrimary && resp.PrimaryImage == nil {
			u := i.ImageURL
			resp.PrimaryImage = &u
		}
	}
	resp.Features = featuresToResponse(a.Features, lang)
	for _, t := range a.Tags {
		resp.Tags = append(resp.Tags, tagResponse{ID: t.ID.String(), TagName: t.TagName, TagNameI18n: t.TagNameI18n})
	}
	for _, s := range a.SocialLinks {
		resp.SocialLinks = append(resp.SocialLinks, socialResponse{ID: s.ID.String(), Type: s.Type, URL: s.URL})
	}
	applyVenueState(&resp, a.VenueState)
	return resp
}

// applyDerivedCuisineType keeps the legacy string in sync with the set INSIDE
// the response, not only in the database.
//
// The stored cuisine_type is rewritten whenever a venue changes its set, but it
// can still drift: a cuisine renamed in the dictionary does not touch 45 venue
// rows. Rendering it from the set right here means an old store build always
// sees names that actually exist, and — unlike the stored column, which is
// Russian — sees them in the language it asked for. A venue with no dictionary
// links keeps its stored string untouched, which is the whole reason the
// unresolved composite values are safe to leave alone.
func applyDerivedCuisineType(resp *restaurantResponse, cs []domain.Cuisine, lang string) {
	if len(cs) == 0 {
		return
	}
	resp.CuisineType = domain.JoinCuisineNames(cs, lang)
}

func categoryToResponse(c domain.RestaurantCategory) categoryResponse {
	return categoryResponse{ID: c.ID.String(), Name: c.Name, NameI18n: c.NameI18n}
}

func managerToResponse(m domain.RestaurantManager) managerResponse {
	return managerResponse{
		ID: m.ID.String(), RestaurantID: m.RestaurantID.String(), UserID: m.UserID.String(),
		Role:          string(m.Role),
		WhatsappOptIn: m.WhatsappOptIn, WhatsappPhone: m.WhatsappPhone,
	}
}
