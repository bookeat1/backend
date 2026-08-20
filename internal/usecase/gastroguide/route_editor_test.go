package gastroguide

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeRouteRepo is a hand-written double (no mock framework, per the repo's
// convention). The SQL-level guarantees — atomic reorder, gap closing, slug
// uniqueness — belong to the integration tests; what is checked here is the
// publication rule, the validation and the superadmin gate.
type fakeRouteRepo struct {
	detail *domain.GastroRouteAdminDetail
	points int
	err    error

	gotStatus      domain.GuideRouteStatus
	gotPublishedAt *time.Time
	statusCalls    int
	gotPoint       domain.GuideRoutePointWrite
	writes         int
}

func (f *fakeRouteRepo) ListRoutesAdmin(context.Context, domain.GastroRouteAdminFilter) ([]domain.GastroRoute, int, error) {
	f.writes++
	return nil, 0, f.err
}

func (f *fakeRouteRepo) GetRouteAdmin(context.Context, uuid.UUID) (*domain.GastroRouteAdminDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.detail == nil {
		return nil, domain.ErrNotFound
	}
	return f.detail, nil
}

func (f *fakeRouteRepo) CreateRoute(context.Context, domain.GastroRouteWrite) (*domain.GastroRoute, error) {
	f.writes++
	return &domain.GastroRoute{ID: uuid.New()}, f.err
}

func (f *fakeRouteRepo) UpdateRoute(context.Context, uuid.UUID, domain.GastroRouteWrite) (*domain.GastroRoute, error) {
	f.writes++
	return &domain.GastroRoute{ID: uuid.New()}, f.err
}

func (f *fakeRouteRepo) SetRouteStatus(_ context.Context, _ uuid.UUID, status domain.GuideRouteStatus, at *time.Time) (*domain.GastroRoute, error) {
	f.writes++
	f.statusCalls++
	f.gotStatus, f.gotPublishedAt = status, at
	if f.err != nil {
		return nil, f.err
	}
	return &domain.GastroRoute{Status: status, PublishedAt: at}, nil
}

func (f *fakeRouteRepo) CountPoints(context.Context, uuid.UUID) (int, error) {
	return f.points, f.err
}

func (f *fakeRouteRepo) AddPoint(_ context.Context, _ uuid.UUID, in domain.GuideRoutePointWrite) (*domain.GuideRoutePoint, error) {
	f.writes++
	f.gotPoint = in
	return &domain.GuideRoutePoint{ID: uuid.New(), Kind: in.Kind, Title: in.Title}, f.err
}

func (f *fakeRouteRepo) UpdatePoint(_ context.Context, _, _ uuid.UUID, in domain.GuideRoutePointWrite) (*domain.GuideRoutePoint, error) {
	f.writes++
	f.gotPoint = in
	return &domain.GuideRoutePoint{ID: uuid.New(), Kind: in.Kind, Title: in.Title}, f.err
}

func (f *fakeRouteRepo) DeletePoint(context.Context, uuid.UUID, uuid.UUID) error {
	f.writes++
	return f.err
}

func (f *fakeRouteRepo) ReorderPoints(context.Context, uuid.UUID, []uuid.UUID) error {
	f.writes++
	return f.err
}

func (f *fakeRouteRepo) ListRoutePointIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, f.err
}

var _ domain.GastroRouteEditorRepository = (*fakeRouteRepo)(nil)

func routeWithPoints(n int) *fakeRouteRepo {
	return &fakeRouteRepo{
		detail: &domain.GastroRouteAdminDetail{
			GastroRoute: domain.GastroRoute{ID: uuid.New(), Slug: "classic-almaty", Title: "Классический тур"},
		},
		points: n,
	}
}

func admin() EditorActor {
	return EditorActor{UserID: uuid.New(), Role: domain.RoleAdmin}
}

// THE DECISION: an empty route cannot be published.
//
// For collections we deliberately REMOVED the equivalent guard (PR #81): a
// collection is an article, and one that links no openable venue still reads.
// A route is not an article — it IS the sequence of stops. Published empty, it
// renders as a title, a cover and a duration label («1 день · 4 точки») that
// contradicts itself, with nothing to walk.
func TestRoutePublish_RefusesAnEmptyRoute(t *testing.T) {
	repo := routeWithPoints(0)
	e := NewRouteEditor(repo)

	_, err := e.Publish(context.Background(), admin(), repo.detail.ID, nil)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodeGuideRouteEmpty {
		t.Errorf("code = %q, want %q", code, domain.CodeGuideRouteEmpty)
	}
	if repo.statusCalls != 0 {
		t.Errorf("status was written %d times on a refused publish", repo.statusCalls)
	}
}

// ONE stop is enough — the check counts the ITINERARY, not how much of it is
// bookable today. A route of one park is thin editorial content, but it is
// content, and refusing it would repeat the mistake the collection guard made.
func TestRoutePublish_OneStopIsEnough(t *testing.T) {
	repo := routeWithPoints(1)
	e := NewRouteEditor(repo)
	frozen := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	e.(*routeEditor).clock = func() time.Time { return frozen }

	rt, err := e.Publish(context.Background(), admin(), repo.detail.ID, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if repo.gotStatus != domain.GuideRoutePublished {
		t.Errorf("status = %q, want published", repo.gotStatus)
	}
	if repo.gotPublishedAt == nil || !repo.gotPublishedAt.Equal(frozen) {
		t.Errorf("published_at = %v, want the clock's %v", repo.gotPublishedAt, frozen)
	}
	if rt.PublishedAt == nil {
		t.Errorf("returned route has no published_at")
	}
}

// A published_at in the future is a SCHEDULED publication, passed through as
// given: the guest predicate (published_at <= now) already implements it.
func TestRoutePublish_ScheduledTimePassesThrough(t *testing.T) {
	repo := routeWithPoints(3)
	e := NewRouteEditor(repo)
	at := time.Now().Add(48 * time.Hour)

	if _, err := e.Publish(context.Background(), admin(), repo.detail.ID, &at); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if repo.gotPublishedAt == nil || !repo.gotPublishedAt.Equal(at) {
		t.Errorf("published_at = %v, want %v", repo.gotPublishedAt, at)
	}
}

// Unpublish clears published_at (so a later re-publish gets a fresh date);
// archive KEEPS it, because an archived route is one that WAS live.
func TestRouteStatus_UnpublishClearsAndArchiveKeepsTheDate(t *testing.T) {
	repo := routeWithPoints(2)
	was := time.Now().Add(-72 * time.Hour)
	repo.detail.PublishedAt = &was
	e := NewRouteEditor(repo)

	if _, err := e.Unpublish(context.Background(), admin(), repo.detail.ID); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if repo.gotStatus != domain.GuideRouteDraft || repo.gotPublishedAt != nil {
		t.Errorf("unpublish wrote status %q / published_at %v, want draft / nil", repo.gotStatus, repo.gotPublishedAt)
	}

	if _, err := e.Archive(context.Background(), admin(), repo.detail.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if repo.gotStatus != domain.GuideRouteArchived {
		t.Errorf("archive wrote status %q, want archived", repo.gotStatus)
	}
	if repo.gotPublishedAt == nil || !repo.gotPublishedAt.Equal(was) {
		t.Errorf("archive wrote published_at %v, want the original %v", repo.gotPublishedAt, was)
	}
}

// EVERY write refuses a caller who is not a superadmin, and refuses BEFORE
// touching the repository. The router already gates these routes; this is the
// second lock, for the day somebody mounts them on the wrong group.
func TestRouteEditor_EveryOperationIsSuperadminOnly(t *testing.T) {
	for _, role := range []domain.Role{domain.RoleUser, domain.RoleRestaurant} {
		repo := routeWithPoints(3)
		e := NewRouteEditor(repo)
		actor := EditorActor{UserID: uuid.New(), Role: role}
		id, pointID := uuid.New(), uuid.New()

		calls := map[string]error{}
		_, calls["list"] = func() (int, error) {
			_, n, err := e.ListRoutes(context.Background(), actor, RouteAdminListInput{})
			return n, err
		}()
		_, calls["get"] = e.GetRoute(context.Background(), actor, id)
		_, calls["create"] = e.CreateRoute(context.Background(), actor, RouteInput{Slug: "walk", Title: "Прогулка"})
		_, calls["update"] = e.UpdateRoute(context.Background(), actor, id, RouteInput{Slug: "walk", Title: "Прогулка"})
		_, calls["publish"] = e.Publish(context.Background(), actor, id, nil)
		_, calls["unpublish"] = e.Unpublish(context.Background(), actor, id)
		_, calls["archive"] = e.Archive(context.Background(), actor, id)
		_, calls["addPoint"] = e.AddPoint(context.Background(), actor, id, PointInput{Kind: domain.GuideRoutePointPlace, Title: "Точка"})
		_, calls["updatePoint"] = e.UpdatePoint(context.Background(), actor, id, pointID, PointInput{Kind: domain.GuideRoutePointPlace, Title: "Точка"})
		calls["deletePoint"] = e.DeletePoint(context.Background(), actor, id, pointID)
		calls["reorder"] = e.ReorderPoints(context.Background(), actor, id, []uuid.UUID{pointID})

		for name, err := range calls {
			if !errors.Is(err, domain.ErrForbidden) {
				t.Errorf("role %s, %s: err = %v, want ErrForbidden", role, name, err)
			}
		}
		if repo.writes != 0 {
			t.Errorf("role %s: repository was written %d times", role, repo.writes)
		}
	}
}

// The kind ↔ restaurant_id pairing is the invariant the schema cannot fully
// hold: restaurant_id stays nullable so that DELETING a venue clears the link
// instead of deleting the stop, which means "a venue stop names a venue" can
// only be enforced at the write.
func TestRoutePoint_ValidatesKindAgainstTheVenue(t *testing.T) {
	venue := uuid.New()
	cases := []struct {
		name    string
		in      PointInput
		wantErr bool
	}{
		{"venue stop with a venue", PointInput{Kind: domain.GuideRoutePointRestaurant, RestaurantID: &venue, Title: "Утро: Daily Coffee"}, false},
		{"venue stop without a venue", PointInput{Kind: domain.GuideRoutePointRestaurant, Title: "Утро"}, true},
		{"place stop", PointInput{Kind: domain.GuideRoutePointPlace, Title: "Парк 28 панфиловцев"}, false},
		{"place stop carrying a venue", PointInput{Kind: domain.GuideRoutePointPlace, RestaurantID: &venue, Title: "Парк"}, true},
		{"unknown kind", PointInput{Kind: "museum", Title: "Музей"}, true},
		{"no title", PointInput{Kind: domain.GuideRoutePointPlace}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := routeWithPoints(1)
			e := NewRouteEditor(repo)
			_, err := e.AddPoint(context.Background(), admin(), uuid.New(), tc.in)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("err = %v, want ErrValidation", err)
				}
				if repo.writes != 0 {
					t.Errorf("the repository was written on a refused point")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// Half a coordinate pair is refused: a stop with a latitude and no longitude
// would be pinned on the prime meridian, off the coast of Africa.
func TestRoutePoint_CoordinatesAreBothOrNeither(t *testing.T) {
	lat, lng := 43.238949, 76.889709
	cases := []struct {
		name    string
		lat     *float64
		lng     *float64
		wantErr bool
	}{
		{"both", &lat, &lng, false},
		{"neither", nil, nil, false},
		{"only latitude", &lat, nil, true},
		{"only longitude", nil, &lng, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := routeWithPoints(1)
			e := NewRouteEditor(repo)
			_, err := e.AddPoint(context.Background(), admin(), uuid.New(), PointInput{
				Kind: domain.GuideRoutePointPlace, Title: "Кок-Тобе",
				Latitude: tc.lat, Longitude: tc.lng,
			})
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, domain.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// A blank cover URL means "there is no cover", not a cover whose URL is "" —
// the guest response omits nil and would otherwise make the app render a broken
// image.
func TestRouteInput_BlankCoverBecomesNoCover(t *testing.T) {
	blank := "   "
	w, err := validateRoute(RouteInput{Slug: "walk", Title: "Прогулка", CoverImageURL: &blank})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if w.CoverImageURL != nil {
		t.Errorf("cover = %q, want nil", *w.CoverImageURL)
	}
}
