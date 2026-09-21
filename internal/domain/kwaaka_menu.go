package domain

import "context"

// KwaakaMenuSource fetches a venue's current menu (dishes + availability) from
// Kwaaka, already translated into domain terms. It is the only thing the
// domain knows about Kwaaka's HTTP contract — the request shape, the "Gourmet"
// JSON schema and auth live in infrastructure/kwaaka, exactly as
// PaymentGateway relates to infrastructure/payment (spec: Kwaaka POS
// integration phase 1, menu + stop-list only — see docs and
// bookeat-docs/kwaaka/table-booking-swagger-v1.1.0.yaml).
type KwaakaMenuSource interface {
	// FetchMenu pulls the CURRENT full menu for kwaakaRestaurantID
	// (Restaurant.KwaakaRestaurantID). Kwaaka's GET /restaurants/{id}/menu has
	// no delta or pagination — every call returns the whole menu — so a
	// caller must treat the result as authoritative for "what should be
	// visible right now", never as an increment to merge.
	FetchMenu(ctx context.Context, kwaakaRestaurantID string) (KwaakaMenu, error)
}

// KwaakaMenu is one venue's menu as Kwaaka reports it, translated into domain
// terms. Combos and modifiers exist in Kwaaka's schema but are out of scope
// for phase 1 (menu + stop-list only) and are dropped by the adapter, not
// carried here — see infrastructure/kwaaka's doc.go for what is deliberately
// not mapped yet.
type KwaakaMenu struct {
	Products []KwaakaMenuProduct
}

// KwaakaMenuProduct is one dish, already resolved into what BookEat needs to
// upsert a menu_items row: IsAvailable already folds in BOTH the product's own
// flag and its section's — a dish belonging to a hidden section reads as
// unavailable here even though Kwaaka's per-product flag alone says otherwise,
// because a stop-listed section is exactly as unavailable to a guest as a
// stop-listed dish.
type KwaakaMenuProduct struct {
	// ExternalID is Kwaaka's product id — MenuItem.KwaakaProductID.
	ExternalID string
	Name       string
	// Description is the resolved single-language text; localisation across
	// KZ/RU is deferred (LanguageDescription entries beyond the first are
	// dropped) — see infrastructure/kwaaka doc.go.
	Description string
	// Price is a decimal STRING in the same format the rest of the domain
	// uses (domain.ValidPrice), converted from Kwaaka's numeric price by the
	// adapter so no float ever crosses this boundary.
	Price string
	// ImageURL is the first image, empty when Kwaaka sent none.
	ImageURL string
	// Category is the section name, carried as free text — MenuItem.Category
	// is not an FK (see domain.MenuItem's doc comment).
	Category string
	// IsAvailable is the combined product+section availability described
	// above.
	IsAvailable bool
}
