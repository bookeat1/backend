package gastroguide

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// The editor's guarantees here are properties of SQL — an ordering written in
// one transaction under a deferred unique constraint, a gap closed on delete, a
// slug refused by a unique index — so they run against a real Postgres.

func routeEditorSetup(t *testing.T) (*pgxpool.Pool, *RouteEditorRepository, context.Context) {
	t.Helper()
	pool, _, ctx := routeSetup(t)
	return pool, NewRouteEditor(pool, sqltx.NewManager(pool)), ctx
}

func placePoint(title string) domain.GuideRoutePointWrite {
	return domain.GuideRoutePointWrite{Kind: domain.GuideRoutePointPlace, Title: title}
}

func routePositions(t *testing.T, pool *pgxpool.Pool, ctx context.Context, routeID uuid.UUID) map[uuid.UUID]int {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT id, position FROM gastro_route_points WHERE route_id = $1`, routeID)
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		out[id] = pos
	}
	return out
}

// A new stop lands at the end, with a number nobody else holds.
func TestAddPoint_AppendsAtTheEnd(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})

	var ids []uuid.UUID
	for _, title := range []string{"Первая", "Вторая", "Третья"} {
		p, err := repo.AddPoint(ctx, route, placePoint(title))
		if err != nil {
			t.Fatalf("add point %q: %v", title, err)
		}
		ids = append(ids, p.ID)
	}
	positions := routePositions(t, pool, ctx, route)
	for i, id := range ids {
		if positions[id] != i+1 {
			t.Fatalf("position of stop #%d = %d, want %d", i, positions[id], i+1)
		}
	}
	n, err := repo.CountPoints(ctx, route)
	if err != nil {
		t.Fatalf("count points: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

// A venue stop that names a venue nobody has is ErrNotFound, not a 500 shaped
// like a foreign-key error.
func TestAddPoint_UnknownRestaurantIsNotFound(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})
	stranger := uuid.New()

	_, err := repo.AddPoint(ctx, route, domain.GuideRoutePointWrite{
		Kind: domain.GuideRoutePointRestaurant, RestaurantID: &stranger, Title: "Утро",
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Writing a stop against a route that does not exist is ErrNotFound too — the
// lock doubles as the existence check.
func TestAddPoint_UnknownRouteIsNotFound(t *testing.T) {
	_, repo, ctx := routeEditorSetup(t)
	if _, err := repo.AddPoint(ctx, uuid.New(), placePoint("Точка")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Deleting a stop closes the gap: positions stay 1..N so the next append does
// not drift away from the visible order.
func TestDeletePoint_ClosesTheGap(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})

	var ids []uuid.UUID
	for _, title := range []string{"Первая", "Вторая", "Третья"} {
		p, err := repo.AddPoint(ctx, route, placePoint(title))
		if err != nil {
			t.Fatalf("add point: %v", err)
		}
		ids = append(ids, p.ID)
	}
	if err := repo.DeletePoint(ctx, route, ids[0]); err != nil {
		t.Fatalf("delete point: %v", err)
	}
	positions := routePositions(t, pool, ctx, route)
	if len(positions) != 2 {
		t.Fatalf("stops = %d, want 2", len(positions))
	}
	if positions[ids[1]] != 1 || positions[ids[2]] != 2 {
		t.Fatalf("positions after delete = %v, want 1 and 2", positions)
	}
	if err := repo.DeletePoint(ctx, route, ids[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting the same stop twice: err = %v, want ErrNotFound", err)
	}
}

// A rotation passes through states where two stops share a number. It lands
// whole because the unique (route_id, position) is DEFERRABLE and the whole new
// ordering is one statement in one transaction.
func TestReorderPoints_RotationLandsWhole(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})

	var ids []uuid.UUID
	for _, title := range []string{"A", "B", "C", "D"} {
		p, err := repo.AddPoint(ctx, route, placePoint(title))
		if err != nil {
			t.Fatalf("add point: %v", err)
		}
		ids = append(ids, p.ID)
	}
	reversed := []uuid.UUID{ids[3], ids[2], ids[1], ids[0]}
	if err := repo.ReorderPoints(ctx, route, reversed); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	got, err := repo.ListRoutePointIDs(ctx, route)
	if err != nil {
		t.Fatalf("list ids: %v", err)
	}
	for i := range reversed {
		if got[i] != reversed[i] {
			t.Fatalf("order = %v, want %v", got, reversed)
		}
	}
	positions := routePositions(t, pool, ctx, route)
	seen := map[int]bool{}
	for id, pos := range positions {
		if seen[pos] {
			t.Fatalf("position %d is held twice (stop %s)", pos, id)
		}
		seen[pos] = true
	}
}

// A payload that does not describe exactly the current stops is refused with
// guide_order_mismatch and NOTHING is written: the editor's screen is stale, and
// guessing what they meant would silently rewrite an itinerary.
func TestReorderPoints_StalePayloadWritesNothing(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})

	var ids []uuid.UUID
	for _, title := range []string{"A", "B"} {
		p, err := repo.AddPoint(ctx, route, placePoint(title))
		if err != nil {
			t.Fatalf("add point: %v", err)
		}
		ids = append(ids, p.ID)
	}
	before := routePositions(t, pool, ctx, route)

	err := repo.ReorderPoints(ctx, route, []uuid.UUID{ids[1], ids[0], uuid.New()})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideOrderMismatch {
		t.Errorf("code = %q, want %q", code, domain.CodeGuideOrderMismatch)
	}
	after := routePositions(t, pool, ctx, route)
	for id, pos := range before {
		if after[id] != pos {
			t.Fatalf("stop %s moved from %d to %d on a refused reorder", id, pos, after[id])
		}
	}
}

// The cabinet sees the stop of a DEACTIVATED venue with its card and an
// is_active flag, where a guest sees the stop with no card at all. Without it
// the editor cannot explain why the app shows their route differently.
func TestGetRouteAdmin_ShowsDarkVenuesFlagged(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	dark := seedVenue(t, pool, ctx, "Погашенное", false)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})
	if _, err := repo.AddPoint(ctx, route, domain.GuideRoutePointWrite{
		Kind: domain.GuideRoutePointRestaurant, RestaurantID: &dark, Title: "Ужин",
	}); err != nil {
		t.Fatalf("add point: %v", err)
	}

	detail, err := repo.GetRouteAdmin(ctx, route)
	if err != nil {
		t.Fatalf("get admin route: %v", err)
	}
	if len(detail.Points) != 1 {
		t.Fatalf("points = %d, want 1", len(detail.Points))
	}
	card := detail.Points[0].Venue
	if card == nil {
		t.Fatalf("editor sees no venue card for a deactivated venue")
	}
	if card.IsActive {
		t.Errorf("is_active = true, want false — the editor must see the slot is dark")
	}
}

// The slug is the client-facing stable name and is unique platform-wide; a
// duplicate is a refusal an editor can act on, not a 500.
func TestCreateRoute_DuplicateSlugIsTagged(t *testing.T) {
	_, repo, ctx := routeEditorSetup(t)
	in := domain.GastroRouteWrite{Slug: "classic-almaty", Title: "Классический тур"}
	if _, err := repo.CreateRoute(ctx, in); err != nil {
		t.Fatalf("create route: %v", err)
	}
	_, err := repo.CreateRoute(ctx, in)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideSlugTaken {
		t.Errorf("code = %q, want %q", code, domain.CodeGuideSlugTaken)
	}
}

// Updating a stop keeps its place in the walk: moving stops is the reorder
// operation, and a saved typo fix must not rearrange the day.
func TestUpdatePoint_KeepsThePosition(t *testing.T) {
	pool, repo, ctx := routeEditorSetup(t)
	route := seedRoute(t, pool, ctx, routeSeed{slug: "walk", status: domain.GuideRouteDraft})
	first, err := repo.AddPoint(ctx, route, placePoint("Первая"))
	if err != nil {
		t.Fatalf("add point: %v", err)
	}
	second, err := repo.AddPoint(ctx, route, placePoint("Вторая"))
	if err != nil {
		t.Fatalf("add point: %v", err)
	}

	updated, err := repo.UpdatePoint(ctx, route, second.ID, placePoint("Вторая, переписанная"))
	if err != nil {
		t.Fatalf("update point: %v", err)
	}
	if updated.Position != second.Position {
		t.Errorf("position = %d, want %d", updated.Position, second.Position)
	}
	if updated.Title != "Вторая, переписанная" {
		t.Errorf("title = %q, want the new one", updated.Title)
	}
	// A stop of ANOTHER route is not reachable through this route's id.
	other := seedRoute(t, pool, ctx, routeSeed{slug: "other", status: domain.GuideRouteDraft})
	if _, err := repo.UpdatePoint(ctx, other, first.ID, placePoint("Чужая")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-route update: err = %v, want ErrNotFound", err)
	}
}
