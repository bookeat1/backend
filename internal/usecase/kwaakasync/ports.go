// Package kwaakasync periodically pulls menu + stop-list state from Kwaaka
// (domain.KwaakaMenuSource) for every restaurant that opted in
// (Restaurant.KwaakaRestaurantID set) and upserts it into menu_items. This is
// phase 1 of the Kwaaka POS integration ONLY — see
// infrastructure/kwaaka/doc.go for the verified contract and
// team-memory specs/bookeat-kwaaka-pos-integration.md for the phased plan.
// Orders, reserves, banquets and payment are explicitly out of scope.
package kwaakasync

import (
	"context"

	"backend-core/internal/domain"
)

// RestaurantSource lists the restaurants this worker is allowed to touch. It
// is a local, narrow port over domain.RestaurantRepository — wired in
// bootstrap with the SAME Postgres restaurant repository the rest of the app
// uses, so a restaurant that gets its kwaaka_restaurant_id cleared drops out
// of the next sync pass with no extra bookkeeping.
type RestaurantSource interface {
	ListKwaakaLinked(ctx context.Context) ([]domain.KwaakaLinkedRestaurant, error)
}
