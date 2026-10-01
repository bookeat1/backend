package admin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/restaurants"
)

// kwRest is a restaurantStore whose Kwaaka linkage the test controls.
type kwRest struct {
	link *string
	err  error
}

func (f *kwRest) Get(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	if f.err != nil {
		return nil, f.err
	}
	a := &domain.RestaurantAggregate{}
	a.ID = id
	a.KwaakaRestaurantID = f.link
	return a, nil
}
func (f *kwRest) Update(context.Context, uuid.UUID, restaurants.SaveInput) (*domain.RestaurantAggregate, error) {
	return nil, errors.New("not used")
}

// kwStore mimics the Postgres repository: Save replaces the pool and moves
// enabled_at only on false->true.
type kwStore struct {
	row    *domain.KwaakaOrderSettings
	saves  int
	locked int
}

func (s *kwStore) Get(_ context.Context, _ uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	if s.row == nil {
		return nil, domain.ErrNotFound
	}
	c := *s.row
	c.Pool = append([]domain.KwaakaPoolTable(nil), s.row.Pool...)
	return &c, nil
}
func (s *kwStore) LockForUpdate(ctx context.Context, id uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	s.locked++
	return s.Get(ctx, id)
}
func (s *kwStore) Save(_ context.Context, in *domain.KwaakaOrderSettings) error {
	s.saves++
	now := time.Now()
	var enabledAt *time.Time
	if s.row != nil {
		enabledAt = s.row.EnabledAt
	}
	if in.OrdersEnabled && (s.row == nil || !s.row.OrdersEnabled) {
		enabledAt = &now
	}
	in.EnabledAt, in.UpdatedAt = enabledAt, now
	c := *in
	c.Pool = append([]domain.KwaakaPoolTable(nil), in.Pool...)
	s.row = &c
	return nil
}

type kwPOS struct {
	mu     sync.Mutex // the DB concurrency test calls it from many goroutines
	tables []domain.PosTable
	err    error
	calls  int
	gotID  string
}

func (p *kwPOS) GetTables(_ context.Context, id string) ([]domain.PosTable, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.gotID = id
	return p.tables, p.err
}

type kwTx struct{}

func (kwTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (kwTx) Detach(ctx context.Context) context.Context                         { return ctx }

type kwHarness struct {
	uc    *UseCase
	rest  *kwRest
	store *kwStore
	pos   *kwPOS
	rid   uuid.UUID
	admin Actor
}

func newKwHarness(global bool) *kwHarness {
	link := "kw-1"
	h := &kwHarness{
		rest: &kwRest{link: &link}, store: &kwStore{},
		pos: &kwPOS{tables: []domain.PosTable{{ID: "T1", Name: "BookEat 1"}, {ID: "T2", Name: "BookEat 2"}, {ID: "T3", Name: "BookEat 3"}}},
		rid: uuid.New(), admin: Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
	}
	h.uc = NewUseCase(fakePerms{}, h.rest, &fakeMenu{}, &fakeWH{}, &fakeOverrides{}, &fakeGuests{},
		&fakeBookingList{}, &fakeBookingTx{}, &fakePaymentSettings{}, &fakeTelegramSettings{},
		WithKwaakaOrders(h.store, h.pos, kwTx{}, KwaakaOrdersGlobal{Enabled: global, DefaultLead: 60 * time.Minute}))
	return h
}

func pool(ids ...string) []KwaakaPoolTableInput {
	out := make([]KwaakaPoolTableInput, len(ids))
	for i, id := range ids {
		out[i] = KwaakaPoolTableInput{KwaakaTableID: id}
	}
	return out
}

func lead(n int) *int { return &n }

func wantCode(t *testing.T, err error, sentinel error, code domain.ErrorCode) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if code != "" {
		got, ok := domain.CodeOf(err)
		if !ok || got != code {
			t.Fatalf("code = %q (ok=%v), want %q", got, ok, code)
		}
	}
}

func TestKwaakaOrders_SuperadminOnly(t *testing.T) {
	h := newKwHarness(true)
	in := KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}
	for name, a := range map[string]Actor{
		"owner":     {UserID: uuid.New(), Role: domain.RoleRestaurant},
		"guest":     {UserID: uuid.New(), Role: domain.RoleUser},
		"anonymous": {},
	} {
		_, err := h.uc.SetKwaakaOrders(context.Background(), a, h.rid, in)
		if name == "anonymous" {
			wantCode(t, err, domain.ErrUnauthorized, "")
		} else {
			wantCode(t, err, domain.ErrForbidden, "")
		}
		if _, err := h.uc.GetKwaakaOrders(context.Background(), a, h.rid); err == nil {
			t.Fatalf("%s: GET must be refused", name)
		}
	}
	if h.store.saves != 0 || h.pos.calls != 0 {
		t.Fatalf("refused callers touched the store/POS: saves=%d pos=%d", h.store.saves, h.pos.calls)
	}
}

func TestKwaakaOrders_Validation(t *testing.T) {
	cases := map[string]struct {
		in       KwaakaOrdersInput
		mutate   func(h *kwHarness)
		sentinel error
		code     domain.ErrorCode
	}{
		"lead below range":  {in: KwaakaOrdersInput{LeadMinutes: lead(-1), Pool: pool("T1")}, sentinel: domain.ErrValidation},
		"lead above range":  {in: KwaakaOrdersInput{LeadMinutes: lead(721), Pool: pool("T1")}, sentinel: domain.ErrValidation},
		"empty pool enable": {in: KwaakaOrdersInput{OrdersEnabled: true, Pool: nil}, sentinel: domain.ErrValidation, code: domain.CodeKwaakaTablePoolNeeded},
		"duplicate table":   {in: KwaakaOrdersInput{Pool: pool("T1", "T1")}, sentinel: domain.ErrValidation},
		"blank table id":    {in: KwaakaOrdersInput{Pool: pool("  ")}, sentinel: domain.ErrValidation},
		"too many tables": {in: KwaakaOrdersInput{Pool: func() []KwaakaPoolTableInput {
			var ids []string
			for i := 0; i < 21; i++ {
				ids = append(ids, fmt.Sprintf("T%d", i))
			}
			return pool(ids...)
		}()}, sentinel: domain.ErrValidation},
		"not linked": {in: KwaakaOrdersInput{Pool: pool("T1")}, mutate: func(h *kwHarness) { h.rest.link = nil },
			sentinel: domain.ErrValidation, code: domain.CodeKwaakaNotLinked},
		"blank link": {in: KwaakaOrdersInput{Pool: pool("T1")}, mutate: func(h *kwHarness) { b := "  "; h.rest.link = &b },
			sentinel: domain.ErrValidation, code: domain.CodeKwaakaNotLinked},
		"stale form": {in: KwaakaOrdersInput{Pool: pool("T1"), KwaakaRestaurantID: "kw-OLD"},
			sentinel: domain.ErrValidation, code: domain.CodeKwaakaLinkMismatch},
		"unknown table": {in: KwaakaOrdersInput{Pool: pool("T1", "GONE")},
			sentinel: domain.ErrValidation, code: domain.CodeKwaakaTableUnknown},
		"unknown venue": {in: KwaakaOrdersInput{Pool: pool("T1")}, mutate: func(h *kwHarness) { h.rest.err = domain.ErrNotFound },
			sentinel: domain.ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newKwHarness(true)
			if tc.mutate != nil {
				tc.mutate(h)
			}
			_, err := h.uc.SetKwaakaOrders(context.Background(), h.admin, h.rid, tc.in)
			wantCode(t, err, tc.sentinel, tc.code)
			if h.store.saves != 0 {
				t.Fatalf("a refused PUT wrote %d times", h.store.saves)
			}
		})
	}
}

func TestKwaakaOrders_EnableHappyPath(t *testing.T) {
	h := newKwHarness(true)
	in := KwaakaOrdersInput{OrdersEnabled: true, LeadMinutes: lead(90), KwaakaRestaurantID: "kw-1",
		Pool: []KwaakaPoolTableInput{{KwaakaTableID: " T2 ", Label: "Bar"}, {KwaakaTableID: "T1"}}}
	v, err := h.uc.SetKwaakaOrders(context.Background(), h.admin, h.rid, in)
	if err != nil {
		t.Fatal(err)
	}
	if h.pos.calls != 1 || h.pos.gotID != "kw-1" {
		t.Fatalf("POS calls=%d id=%q, want one call for the venue's link", h.pos.calls, h.pos.gotID)
	}
	row := h.store.row
	if row == nil || !row.OrdersEnabled || row.KwaakaRestaurantID != "kw-1" || *row.LeadMinutes != 90 {
		t.Fatalf("stored row = %+v", row)
	}
	if row.UpdatedBy == nil || *row.UpdatedBy != h.admin.UserID {
		t.Fatalf("updated_by = %v, want the actor", row.UpdatedBy)
	}
	if row.EnabledAt == nil {
		t.Fatal("enabled_at not set on first enable")
	}
	if len(row.Pool) != 2 || row.Pool[0].KwaakaTableID != "T2" || row.Pool[0].Position != 0 || row.Pool[0].Label != "Bar" ||
		row.Pool[1].KwaakaTableID != "T1" || row.Pool[1].Position != 1 || row.Pool[1].Label != "BookEat 1" {
		t.Fatalf("pool (order = priority, empty label filled from POS) = %+v", row.Pool)
	}
	if !v.WillSend || len(v.Blockers) != 0 || !v.GlobalEnabled || v.DefaultLeadMinutes != 60 || !v.POSChecked {
		t.Fatalf("view = %+v", v)
	}
	if v.Pool[0].InPOS == nil || !*v.Pool[0].InPOS {
		t.Fatalf("pool[0].InPOS = %v, want true", v.Pool[0].InPOS)
	}
}

func TestKwaakaOrders_EnabledAtMovesOnlyOnFalseToTrue(t *testing.T) {
	h := newKwHarness(true)
	ctx := context.Background()
	on := KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, on); err != nil {
		t.Fatal(err)
	}
	first := *h.store.row.EnabledAt
	time.Sleep(2 * time.Millisecond)
	on.LeadMinutes = lead(30) // still on: enabled_at must stay
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, on); err != nil {
		t.Fatal(err)
	}
	if !h.store.row.EnabledAt.Equal(first) {
		t.Fatal("enabled_at moved while staying enabled")
	}
	off := on
	off.OrdersEnabled = false
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, off); err != nil {
		t.Fatal(err)
	}
	if h.store.row.OrdersEnabled || !h.store.row.EnabledAt.Equal(first) {
		t.Fatalf("disable must keep enabled_at: %+v", h.store.row)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, on); err != nil {
		t.Fatal(err)
	}
	if !h.store.row.EnabledAt.After(first) {
		t.Fatal("enabled_at must be re-stamped on a new false->true")
	}
}

// Switching OFF is the kill switch: it must work while Kwaaka is down, must not
// even ask the POS, and must not re-stamp a stale linkage as fresh.
func TestKwaakaOrders_DisableNeverNeedsPOSAndKeepsStaleSnapshot(t *testing.T) {
	h := newKwHarness(true)
	ctx := context.Background()
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1", "T2")}); err != nil {
		t.Fatal(err)
	}
	h.pos.calls, h.pos.err = 0, fmt.Errorf("%w: kwaaka down", domain.ErrUnavailable)
	relinked := "kw-2"
	h.rest.link = &relinked

	v, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: false, Pool: pool("T1", "T2")})
	if err != nil {
		t.Fatalf("disable while POS is down: %v", err)
	}
	if h.pos.calls != 0 {
		t.Fatalf("disable called the POS %d times", h.pos.calls)
	}
	if h.store.row.OrdersEnabled || h.store.row.KwaakaRestaurantID != "kw-1" {
		t.Fatalf("row = %+v, want off with the OLD snapshot kw-1 (stale stays visible)", h.store.row)
	}
	if !v.PoolStale {
		t.Fatal("pool must still be reported stale")
	}
}

func TestKwaakaOrders_POSDown_503AndNothingWritten(t *testing.T) {
	h := newKwHarness(true)
	h.pos.err = fmt.Errorf("%w: kwaaka down", domain.ErrUnavailable)
	_, err := h.uc.SetKwaakaOrders(context.Background(), h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")})
	wantCode(t, err, domain.ErrUnavailable, "")
	if h.store.saves != 0 {
		t.Fatal("wrote although the pool could not be verified")
	}
	h.uc.kwaaka.pos = nil // adapter not configured in this process
	_, err = h.uc.SetKwaakaOrders(context.Background(), h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")})
	wantCode(t, err, domain.ErrUnavailable, "")
}

// Re-confirming an unchanged enabled pool (only the lead changes) or relabelling
// needs no POS proof; enabling, changing the pool or a changed linkage does.
func TestKwaakaOrders_POSOnlyWhenNeeded(t *testing.T) {
	h := newKwHarness(true)
	ctx := context.Background()
	base := KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1", "T2")}
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, base); err != nil {
		t.Fatal(err)
	}
	check := func(name string, in KwaakaOrdersInput, wantCalls int) {
		t.Helper()
		h.pos.calls = 0
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if h.pos.calls != wantCalls {
			t.Fatalf("%s: POS calls = %d, want %d", name, h.pos.calls, wantCalls)
		}
	}
	lead30 := base
	lead30.LeadMinutes = lead(30)
	check("lead only", lead30, 0)
	relabel := lead30
	relabel.Pool = []KwaakaPoolTableInput{{KwaakaTableID: "T1", Label: "A"}, {KwaakaTableID: "T2", Label: "B"}}
	check("relabel", relabel, 0)
	reordered := relabel
	reordered.Pool = pool("T2", "T1")
	check("reorder = new pool", reordered, 1)
	grown := reordered
	grown.Pool = pool("T2", "T1", "T3")
	check("pool grown", grown, 1)
	relinked := "kw-2"
	h.rest.link = &relinked
	check("relinked venue, same pool, stays on -> re-prove against new POS", grown, 1)
	if h.store.row.KwaakaRestaurantID != "kw-2" {
		t.Fatalf("snapshot = %q, want kw-2 after re-save", h.store.row.KwaakaRestaurantID)
	}
}

func TestKwaakaOrders_GetViewAndBlockers(t *testing.T) {
	ctx := context.Background()

	t.Run("never configured, global off", func(t *testing.T) {
		h := newKwHarness(false)
		v, err := h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
		if err != nil {
			t.Fatal(err)
		}
		if v.Configured || v.WillSend || v.GlobalEnabled || v.OrdersEnabled {
			t.Fatalf("view = %+v", v)
		}
		wantBlockers(t, v, KwaakaBlockerGlobalOff, KwaakaBlockerNotSetUp)
		if !v.POSChecked || len(v.POSTables) != 3 {
			t.Fatalf("pos tables should be offered for picking: %+v", v)
		}
	})
	t.Run("configured but global off: venue switch alone sends nothing", func(t *testing.T) {
		h := newKwHarness(false)
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}); err != nil {
			t.Fatal(err)
		}
		v, _ := h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
		if !v.OrdersEnabled || v.WillSend {
			t.Fatalf("view = %+v", v)
		}
		wantBlockers(t, v, KwaakaBlockerGlobalOff)
	})
	t.Run("stale pool and table gone from POS", func(t *testing.T) {
		h := newKwHarness(true)
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1", "T2")}); err != nil {
			t.Fatal(err)
		}
		relinked := "kw-2"
		h.rest.link = &relinked
		h.pos.tables = []domain.PosTable{{ID: "T1"}}
		v, _ := h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
		if !v.PoolStale || v.WillSend || v.SettingsKwaakaID != "kw-1" || v.VenueKwaakaID != "kw-2" {
			t.Fatalf("view = %+v", v)
		}
		wantBlockers(t, v, KwaakaBlockerPoolStale)
		if *v.Pool[0].InPOS != true || *v.Pool[1].InPOS != false {
			t.Fatalf("in_pos flags = %v %v, want true false", *v.Pool[0].InPOS, *v.Pool[1].InPOS)
		}
	})
	t.Run("POS down: read still works", func(t *testing.T) {
		h := newKwHarness(true)
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}); err != nil {
			t.Fatal(err)
		}
		h.pos.err = fmt.Errorf("%w: down", domain.ErrUnavailable)
		v, err := h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
		if err != nil {
			t.Fatal(err)
		}
		if v.POSChecked || v.Pool[0].InPOS != nil || !v.WillSend {
			t.Fatalf("view = %+v", v)
		}
	})
	t.Run("venue not linked", func(t *testing.T) {
		h := newKwHarness(true)
		h.rest.link = nil
		v, err := h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
		if err != nil {
			t.Fatal(err)
		}
		wantBlockers(t, v, KwaakaBlockerNotLinked)
		if h.pos.calls != 0 {
			t.Fatal("asked the POS for a venue without a linkage")
		}
	})
}

func wantBlockers(t *testing.T, v KwaakaOrdersView, want ...string) {
	t.Helper()
	if fmt.Sprint(v.Blockers) != fmt.Sprint(want) {
		t.Fatalf("blockers = %v, want %v", v.Blockers, want)
	}
}

func TestKwaakaOrders_NotWiredIsUnavailable(t *testing.T) {
	h := newHarness(nil)
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}
	if _, err := h.uc.GetKwaakaOrders(context.Background(), admin, uuid.New()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// Disabling an existing row must not depend on the venue's link (PR #168 gate
// finding 1): no 422, no POS call, the stored snapshot (and so the stale mark)
// is kept.
func TestKwaakaOrders_DisableWorksOnUnlinkedVenue(t *testing.T) {
	h := newKwHarness(true)
	ctx := context.Background()
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}); err != nil {
		t.Fatal(err)
	}
	h.rest.link = nil // unlinked through the #149 path
	h.pos.calls = 0

	v, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: false, Pool: pool("T1")})
	if err != nil {
		t.Fatalf("disable on an unlinked venue: %v", err)
	}
	if v.OrdersEnabled || h.store.row.OrdersEnabled {
		t.Fatalf("orders_enabled still true: view=%+v row=%+v", v, h.store.row)
	}
	if h.store.row.KwaakaRestaurantID != "kw-1" {
		t.Fatalf("snapshot not kept: %q", h.store.row.KwaakaRestaurantID)
	}
	if h.pos.calls != 0 {
		t.Fatal("disable called the POS")
	}
	// Re-link to the SAME id: it must not resume sending.
	l := "kw-1"
	h.rest.link = &l
	g, err := h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
	if err != nil || g.WillSend {
		t.Fatalf("re-link resumed sending: %+v err=%v", g, err)
	}
	// Re-link to ANOTHER id after a disable: still stale, still blocked.
	h.rest.link = nil
	if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: false, Pool: pool("T1")}); err != nil {
		t.Fatal(err)
	}
	l2 := "kw-2"
	h.rest.link = &l2
	g, _ = h.uc.GetKwaakaOrders(ctx, h.admin, h.rid)
	if !g.PoolStale || g.WillSend {
		t.Fatalf("stale snapshot lost: %+v", g)
	}
}

func TestKwaakaOrders_UnlinkedVenueRules(t *testing.T) {
	ctx := context.Background()
	t.Run("enable still needs the link", func(t *testing.T) {
		h := newKwHarness(true)
		h.rest.link = nil
		_, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")})
		wantCode(t, err, domain.ErrValidation, domain.CodeKwaakaNotLinked)
	})
	t.Run("a changed pool needs the link", func(t *testing.T) {
		h := newKwHarness(true)
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}); err != nil {
			t.Fatal(err)
		}
		h.rest.link = nil
		_, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{Pool: pool("T2")})
		wantCode(t, err, domain.ErrValidation, domain.CodeKwaakaNotLinked)
		if !h.store.row.OrdersEnabled || h.store.row.Pool[0].KwaakaTableID != "T1" {
			t.Fatalf("a refused PUT changed the row: %+v", h.store.row)
		}
	})
	t.Run("disable with an emptied pool works and keeps the snapshot", func(t *testing.T) {
		h := newKwHarness(true)
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1")}); err != nil {
			t.Fatal(err)
		}
		h.rest.link = nil
		// A stale confirmation of the old link must not block the kill switch.
		if _, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{Pool: pool(), KwaakaRestaurantID: "kw-1"}); err != nil {
			t.Fatal(err)
		}
		if h.store.row.OrdersEnabled || len(h.store.row.Pool) != 0 || h.store.row.KwaakaRestaurantID != "kw-1" {
			t.Fatalf("row = %+v", h.store.row)
		}
	})
	t.Run("no row, disable, empty pool is a no-op 200", func(t *testing.T) {
		h := newKwHarness(true)
		h.rest.link = nil
		v, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{Pool: pool()})
		if err != nil || v.Configured {
			t.Fatalf("view=%+v err=%v", v, err)
		}
		if h.store.row != nil || h.store.saves != 0 {
			t.Fatalf("a no-op created a row: %+v saves=%d", h.store.row, h.store.saves)
		}
	})
	t.Run("unknown venue is still 404", func(t *testing.T) {
		h := newKwHarness(true)
		h.rest.err = domain.ErrNotFound
		_, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{Pool: pool()})
		wantCode(t, err, domain.ErrNotFound, "")
	})
}

// raceStore returns a different row from LockForUpdate than from the earlier
// unlocked Get, as if a concurrent PUT committed in between (finding 2).
type raceStore struct {
	kwStore
	locked *domain.KwaakaOrderSettings
}

func (s *raceStore) LockForUpdate(context.Context, uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	c := *s.locked
	return &c, nil
}

func TestKwaakaOrders_KeepSnapshotDecidedUnderLock(t *testing.T) {
	ctx := context.Background()
	h := newKwHarness(true)
	// Unlocked read: enabled, pool P=[T1], snapshot kw-1. Under the lock: another
	// PUT already moved it to snapshot kw-9 with pool Q=[T2].
	rs := &raceStore{locked: &domain.KwaakaOrderSettings{
		RestaurantID: h.rid, OrdersEnabled: true, KwaakaRestaurantID: "kw-9",
		Pool: []domain.KwaakaPoolTable{{KwaakaTableID: "T2"}},
	}}
	rs.row = &domain.KwaakaOrderSettings{
		RestaurantID: h.rid, OrdersEnabled: true, KwaakaRestaurantID: "kw-1",
		Pool: []domain.KwaakaPoolTable{{KwaakaTableID: "T1"}},
	}
	h.uc = NewUseCase(fakePerms{}, h.rest, &fakeMenu{}, &fakeWH{}, &fakeOverrides{}, &fakeGuests{},
		&fakeBookingList{}, &fakeBookingTx{}, &fakePaymentSettings{}, &fakeTelegramSettings{},
		WithKwaakaOrders(rs, h.pos, kwTx{}, KwaakaOrdersGlobal{Enabled: true, DefaultLead: time.Hour}))

	// Disable with pool P: on the stale read that is "unchanged pool, keep
	// snapshot, no POS"; on the locked row P is NEW and unproven. Must not be
	// written as a verified pool.
	_, err := h.uc.SetKwaakaOrders(ctx, h.admin, h.rid, KwaakaOrdersInput{Pool: pool("T1")})
	wantCode(t, err, domain.ErrUnavailable, "")
	if rs.saves != 0 {
		t.Fatalf("wrote a pool that was never proven for the locked row (saves=%d)", rs.saves)
	}
}

func TestKwaakaOrders_UnknownTablesListed(t *testing.T) {
	h := newKwHarness(true)
	_, err := h.uc.SetKwaakaOrders(context.Background(), h.admin, h.rid,
		KwaakaOrdersInput{OrdersEnabled: true, Pool: pool("T1", "GONE", "ALSO")})
	wantCode(t, err, domain.ErrValidation, domain.CodeKwaakaTableUnknown)
	var ute *KwaakaUnknownTablesError
	if !errors.As(err, &ute) || fmt.Sprint(ute.IDs) != "[GONE ALSO]" {
		t.Fatalf("unknown ids = %+v (err=%v)", ute, err)
	}
	if h.store.row != nil {
		t.Fatal("a refused PUT wrote")
	}
}
