package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// Kwaaka kitchen orders: per-venue settings (ADR-048/049)
//
// Whether a venue's confirmed pre-orders are sent to its POS as a table order,
// how long before the booking, and WHICH POS tables the orders are spread over.
// Stored in restaurant_kwaaka_order_settings + restaurant_kwaaka_pool_tables
// (migration 0120) and read by the sending loop in usecase/kwaakaorders.
//
// SUPERADMIN ONLY: the switch makes the platform write into a venue's till.
//
// Two switches, and the global one wins: KWAAKA_ORDERS_ENABLED=false stops ALL
// sending platform-wide whatever a venue's orders_enabled says (the worker's
// ClaimPass is a no-op), so a venue can be fully configured and still send
// nothing. The view reports both and the verdict (WillSend / Blockers).
// ---------------------------------------------------------------------------

// KwaakaOrdersGlobal is the platform-side of the Kwaaka order settings, wired
// once from config.
type KwaakaOrdersGlobal struct {
	// Enabled mirrors KWAAKA_ORDERS_ENABLED, the MASTER switch for sending.
	Enabled bool
	// DefaultLead mirrors KWAAKA_KITCHEN_LEAD: what a NULL lead_minutes means.
	DefaultLead time.Duration
}

type kwaakaOrdersDeps struct {
	store  kwaakaOrderSettingsStore
	pos    kwaakaTablesPOS // nil = Kwaaka adapter not configured in this process
	tx     domain.TxManager
	global KwaakaOrdersGlobal
}

// WithKwaakaOrders enables the Kwaaka kitchen-order settings endpoints. pos may
// be nil (adapter not configured): reads still work, and a write that has to
// prove a table in the POS answers ErrUnavailable; switching a venue OFF never
// needs the POS.
func WithKwaakaOrders(store kwaakaOrderSettingsStore, pos kwaakaTablesPOS, tx domain.TxManager, global KwaakaOrdersGlobal) Option {
	return func(u *UseCase) { u.kwaaka = kwaakaOrdersDeps{store: store, pos: pos, tx: tx, global: global} }
}

// Bounds, enforced here as well as by the DB CHECK on lead_minutes.
const (
	minKwaakaLeadMinutes = 0
	maxKwaakaLeadMinutes = 720
	maxKwaakaPoolTables  = 20
	maxKwaakaTableIDLen  = 128
	maxKwaakaLabelRunes  = 100
	// kwaakaPOSTimeout bounds the live GET /tables of ONE request. The client's
	// own retries could otherwise hold an admin HTTP request for ~45 s.
	kwaakaPOSTimeout = 8 * time.Second
)

// Blockers: why a configured venue still sends nothing. Stable strings, the
// panel may branch on them.
const (
	KwaakaBlockerGlobalOff = "global_switch_off"
	KwaakaBlockerDisabled  = "venue_disabled"
	KwaakaBlockerPoolEmpty = "pool_empty"
	KwaakaBlockerPoolStale = "pool_stale"
	KwaakaBlockerNotLinked = "venue_not_linked"
	KwaakaBlockerNotSetUp  = "not_configured"
)

// KwaakaPoolTableInput is one table of the pool as the superadmin sends it.
// Its priority is its index in the request list.
type KwaakaPoolTableInput struct {
	KwaakaTableID string
	Label         string
}

// KwaakaOrdersInput is the full replacement of a venue's settings.
type KwaakaOrdersInput struct {
	OrdersEnabled bool
	// LeadMinutes nil = the platform default (KWAAKA_KITCHEN_LEAD).
	LeadMinutes *int
	Pool        []KwaakaPoolTableInput
	// KwaakaRestaurantID is an optional CONFIRMATION of the linkage the caller
	// saw. The stored value is always taken from restaurants.kwaaka_restaurant_id,
	// never from the client; a different non-empty value here means the form is
	// stale and the write is refused.
	KwaakaRestaurantID string
}

// KwaakaPoolTableView is a stored pool table. InPOS is nil when the POS was not
// asked or could not answer.
type KwaakaPoolTableView struct {
	KwaakaTableID string
	Position      int
	Label         string
	InPOS         *bool
}

// KwaakaOrdersView is the panel's view of a venue's Kwaaka order settings.
type KwaakaOrdersView struct {
	// GlobalEnabled is KWAAKA_ORDERS_ENABLED, the MASTER switch: false = nothing
	// is sent for ANY venue regardless of OrdersEnabled.
	GlobalEnabled      bool
	DefaultLeadMinutes int
	// VenueKwaakaID is restaurants.kwaaka_restaurant_id NOW ("" = not linked);
	// SettingsKwaakaID is the linkage the stored pool was chosen for.
	VenueKwaakaID    string
	SettingsKwaakaID string
	// Configured is false until a settings row exists.
	Configured    bool
	OrdersEnabled bool
	LeadMinutes   *int
	EnabledAt     *time.Time
	UpdatedAt     *time.Time
	UpdatedBy     *uuid.UUID
	Pool          []KwaakaPoolTableView
	// PoolStale: the venue was re-linked after the pool was chosen; the sender
	// stops for it until the pool is saved again (ADR-049).
	PoolStale bool
	// WillSend is the verdict: global switch, venue switch, pool and linkage all
	// allow sending. Blockers lists what does not.
	WillSend bool
	Blockers []string
	// POSChecked is true when a live GET /tables answered; POSTables then lists
	// the tables the superadmin may put in the pool.
	POSChecked bool
	POSTables  []domain.PosTable
}

func requireSuperadmin(actor Actor) error {
	return requirePlatformActor(actor, "Kwaaka order settings")
}

func errKwaakaNotLinked() error {
	return domain.WithCode(domain.CodeKwaakaNotLinked,
		fmt.Errorf("%w: the venue is not linked to Kwaaka (restaurants.kwaaka_restaurant_id is empty)", domain.ErrValidation))
}

// KwaakaUnknownTablesError: pool tables that do not exist in the venue's POS.
// It is a 422 kwaaka_table_unknown (ErrValidation + code) and the transport
// layer additionally returns IDs to the panel.
type KwaakaUnknownTablesError struct{ IDs []string }

func (e *KwaakaUnknownTablesError) Error() string {
	return "pool tables not found in the venue's POS: " + strings.Join(e.IDs, ", ")
}
func (e *KwaakaUnknownTablesError) Unwrap() error               { return domain.ErrValidation }
func (e *KwaakaUnknownTablesError) ErrorCode() domain.ErrorCode { return domain.CodeKwaakaTableUnknown }

func (u *UseCase) kwaakaReady() error {
	if u.kwaaka.store == nil || u.kwaaka.tx == nil {
		return fmt.Errorf("%w: kwaaka order settings are not configured in this deployment", domain.ErrUnavailable)
	}
	return nil
}

// venueKwaakaID returns the venue's current Kwaaka linkage ("" = none).
func (u *UseCase) venueKwaakaID(ctx context.Context, restaurantID uuid.UUID) (string, error) {
	agg, err := u.restaurants.Get(ctx, restaurantID)
	if err != nil {
		return "", err
	}
	if agg == nil || agg.Restaurant.KwaakaRestaurantID == nil {
		return "", nil
	}
	return strings.TrimSpace(*agg.Restaurant.KwaakaRestaurantID), nil
}

// GetKwaakaOrders returns the venue's Kwaaka order settings together with the
// master switch, the verdict and, best effort, the live POS table list.
// SUPERADMIN ONLY. An unreachable POS does not fail the read: POSChecked is
// false and the pool tables carry InPOS=nil.
func (u *UseCase) GetKwaakaOrders(ctx context.Context, actor Actor, restaurantID uuid.UUID) (KwaakaOrdersView, error) {
	if err := requireSuperadmin(actor); err != nil {
		return KwaakaOrdersView{}, err
	}
	if err := u.kwaakaReady(); err != nil {
		return KwaakaOrdersView{}, err
	}
	link, err := u.venueKwaakaID(ctx, restaurantID)
	if err != nil {
		return KwaakaOrdersView{}, err
	}
	set, err := u.kwaaka.store.Get(ctx, restaurantID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return KwaakaOrdersView{}, err
		}
		set = nil
	}
	var tables []domain.PosTable
	checked := false
	if link != "" && u.kwaaka.pos != nil {
		pctx, cancel := context.WithTimeout(ctx, kwaakaPOSTimeout)
		ts, perr := u.kwaaka.pos.GetTables(pctx, link)
		cancel()
		if perr != nil {
			slog.WarnContext(ctx, "admin: kwaaka tables lookup failed (settings read goes on)",
				"restaurant_id", restaurantID, "error", perr.Error())
		} else {
			tables, checked = ts, true
		}
	}
	return u.kwaakaView(link, set, tables, checked), nil
}

// SetKwaakaOrders replaces a venue's Kwaaka order settings: the switch, the lead
// and the pool (full replace; the request's list order is the pool's priority).
// SUPERADMIN ONLY.
//
// Rules, all checked before anything is written:
//   - lead_minutes is nil or within 0..720;
//   - the venue must be linked to Kwaaka (restaurants.kwaaka_restaurant_id); the
//     linkage stored with the pool is taken from there, and a client-supplied
//     kwaaka_restaurant_id that differs is refused as a stale form. The ONE
//     exception is switching OFF a venue that already has a settings row while
//     it is unlinked (with the stored pool or an empty one): that always
//     succeeds, keeps the stored snapshot and needs no POS, because the kill
//     switch must not depend on the link (a re-link to the same id would
//     otherwise resume sending). Unlinked + no row + disabled + empty pool is
//     a no-op 200 (nothing is created);
//   - enabling needs a non-empty pool;
//   - a pool table must exist in the venue's POS (GET /tables) whenever the pool
//     changes, the venue is switched on, or the stored pool was chosen for an
//     older linkage. Switching OFF with an unchanged pool never calls the POS
//     (the kill switch must work while Kwaaka is down) and keeps the stored
//     linkage, so a stale pool stays visibly stale;
//   - enabled_at moves only on false->true (repository), updated_by is the actor.
//
// Whether the stored snapshot is kept is decided twice: on the unlocked read (to
// know if the POS is needed) and again on the row under the lock; if the locked
// row now needs a POS proof that was not collected, the call answers 503 and the
// caller retries.
//
// The POS call happens OUTSIDE the transaction; inside it the settings row is
// locked FOR UPDATE (the same row the claim step locks, after its booking lock,
// so no new lock order appears) and replaced. Two concurrent PUTs both succeed
// and the later commit wins, as for any full-replace PUT.
//
// Every write is audited with an slog line (who, old and new values) emitted
// after the commit.
func (u *UseCase) SetKwaakaOrders(ctx context.Context, actor Actor, restaurantID uuid.UUID, in KwaakaOrdersInput) (KwaakaOrdersView, error) {
	if err := requireSuperadmin(actor); err != nil {
		return KwaakaOrdersView{}, err
	}
	if err := u.kwaakaReady(); err != nil {
		return KwaakaOrdersView{}, err
	}
	if in.LeadMinutes != nil && (*in.LeadMinutes < minKwaakaLeadMinutes || *in.LeadMinutes > maxKwaakaLeadMinutes) {
		return KwaakaOrdersView{}, fmt.Errorf("%w: lead_minutes must be between %d and %d",
			domain.ErrValidation, minKwaakaLeadMinutes, maxKwaakaLeadMinutes)
	}
	pool, err := normalizeKwaakaPool(in.Pool)
	if err != nil {
		return KwaakaOrdersView{}, err
	}
	if in.OrdersEnabled && len(pool) == 0 {
		return KwaakaOrdersView{}, domain.WithCode(domain.CodeKwaakaTablePoolNeeded,
			fmt.Errorf("%w: enabling kitchen orders needs at least one pool table", domain.ErrValidation))
	}

	link, err := u.venueKwaakaID(ctx, restaurantID)
	if err != nil {
		return KwaakaOrdersView{}, err
	}
	old, err := u.kwaaka.store.Get(ctx, restaurantID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return KwaakaOrdersView{}, err
		}
		old = nil
	}
	linked := link != ""
	if !linked {
		// Unlinked venue: switching OFF must still work (the kill switch cannot
		// depend on the link, or a re-link to the same id would silently resume
		// sending). Enabling and any real pool change need the POS, so they need
		// the link.
		if in.OrdersEnabled {
			return KwaakaOrdersView{}, errKwaakaNotLinked()
		}
		if len(pool) > 0 && (old == nil || !samePoolIDs(old.Pool, pool)) {
			return KwaakaOrdersView{}, errKwaakaNotLinked() // cannot prove new tables without the POS
		}
		if old == nil {
			// Nothing stored, nothing to switch off: a no-op 200, no row created.
			return u.kwaakaView(link, nil, nil, false), nil
		}
	} else if c := strings.TrimSpace(in.KwaakaRestaurantID); c != "" && c != link {
		return KwaakaOrdersView{}, domain.WithCode(domain.CodeKwaakaLinkMismatch,
			fmt.Errorf("%w: kwaaka_restaurant_id does not match the venue's current linkage", domain.ErrValidation))
	}

	// plan decides, for the stored state cur, whether the stored linkage snapshot
	// is kept and whether the pool must be proven in the POS. It runs twice: on
	// the unlocked read to decide whether to call the POS at all, and again under
	// the row lock on the row actually being replaced.
	plan := func(cur *domain.KwaakaOrderSettings) (keepSnapshot, needPOS bool) {
		poolChanged := (cur == nil && len(pool) > 0) || (cur != nil && !samePoolIDs(cur.Pool, pool))
		enabling := in.OrdersEnabled && (cur == nil || !cur.OrdersEnabled)
		snapshotChanged := cur == nil || cur.KwaakaRestaurantID != link
		keepSnapshot = cur != nil && !in.OrdersEnabled && (!poolChanged || !linked)
		needPOS = linked && !keepSnapshot && len(pool) > 0 && (poolChanged || enabling || snapshotChanged)
		return keepSnapshot, needPOS
	}
	_, needPOS := plan(old)

	var tables []domain.PosTable
	checked := false
	if needPOS {
		if u.kwaaka.pos == nil {
			return KwaakaOrdersView{}, fmt.Errorf("%w: the Kwaaka POS is not configured in this process, cannot verify pool tables",
				domain.ErrUnavailable)
		}
		pctx, cancel := context.WithTimeout(ctx, kwaakaPOSTimeout)
		tables, err = u.kwaaka.pos.GetTables(pctx, link)
		cancel()
		if err != nil {
			return KwaakaOrdersView{}, err // already wraps ErrUnavailable: 503, nothing written
		}
		checked = true
		byID := make(map[string]domain.PosTable, len(tables))
		for _, t := range tables {
			byID[t.ID] = t
		}
		var unknown []string
		for i := range pool {
			t, ok := byID[pool[i].KwaakaTableID]
			if !ok {
				unknown = append(unknown, pool[i].KwaakaTableID)
				continue
			}
			if pool[i].Label == "" {
				pool[i].Label = truncateRunes(strings.TrimSpace(t.Name), maxKwaakaLabelRunes)
			}
		}
		if len(unknown) > 0 {
			return KwaakaOrdersView{}, &KwaakaUnknownTablesError{IDs: unknown}
		}
	}

	actorID := actor.UserID
	saved := &domain.KwaakaOrderSettings{
		RestaurantID: restaurantID, OrdersEnabled: in.OrdersEnabled, LeadMinutes: in.LeadMinutes,
		KwaakaRestaurantID: link, UpdatedBy: &actorID, Pool: pool,
	}
	var before *domain.KwaakaOrderSettings
	noop := false
	err = u.kwaaka.tx.WithinTx(ctx, func(ctx context.Context) error {
		cur, err := u.kwaaka.store.LockForUpdate(ctx, restaurantID)
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			cur = nil
		}
		before = cur
		keepSnapshot, needNow := plan(cur)
		if needNow && !checked {
			// The row changed between the unlocked read and the lock so that the
			// pool now needs a POS proof we did not collect. Refuse, retry works.
			return fmt.Errorf("%w: the settings changed concurrently, retry", domain.ErrUnavailable)
		}
		if !linked && cur == nil {
			noop = true
			return nil
		}
		if keepSnapshot {
			saved.KwaakaRestaurantID = cur.KwaakaRestaurantID
		}
		return u.kwaaka.store.Save(ctx, saved)
	})
	if err != nil {
		return KwaakaOrdersView{}, err
	}
	if noop {
		return u.kwaakaView(link, nil, nil, false), nil
	}

	slog.InfoContext(ctx, "admin: kwaaka order settings updated",
		"actor_id", actor.UserID, "restaurant_id", restaurantID,
		"old_orders_enabled", before != nil && before.OrdersEnabled, "new_orders_enabled", saved.OrdersEnabled,
		"old_lead_minutes", fmtIntPtr(leadOf(before)), "new_lead_minutes", fmtIntPtr(saved.LeadMinutes),
		"old_pool", poolIDs(poolOf(before)), "new_pool", poolIDs(saved.Pool),
		"old_kwaaka_restaurant_id", kwaakaIDOf(before), "new_kwaaka_restaurant_id", saved.KwaakaRestaurantID,
		"global_orders_enabled", u.kwaaka.global.Enabled)

	return u.kwaakaView(link, saved, tables, checked), nil
}

func (u *UseCase) kwaakaView(link string, set *domain.KwaakaOrderSettings, tables []domain.PosTable, checked bool) KwaakaOrdersView {
	g := u.kwaaka.global
	v := KwaakaOrdersView{
		GlobalEnabled:      g.Enabled,
		DefaultLeadMinutes: int(g.DefaultLead / time.Minute),
		VenueKwaakaID:      link,
		Pool:               []KwaakaPoolTableView{},
		Blockers:           []string{},
		POSChecked:         checked,
		POSTables:          []domain.PosTable{},
	}
	if checked {
		v.POSTables = tables
	}
	var inPOS map[string]bool
	if checked {
		inPOS = make(map[string]bool, len(tables))
		for _, t := range tables {
			inPOS[t.ID] = true
		}
	}
	if set != nil {
		v.Configured = true
		v.SettingsKwaakaID = set.KwaakaRestaurantID
		v.OrdersEnabled = set.OrdersEnabled
		v.LeadMinutes = set.LeadMinutes
		v.EnabledAt = set.EnabledAt
		ua, ub := set.UpdatedAt, set.UpdatedBy
		v.UpdatedAt, v.UpdatedBy = &ua, ub
		v.PoolStale = link != "" && set.KwaakaRestaurantID != link
		for _, t := range set.Pool {
			pt := KwaakaPoolTableView{KwaakaTableID: t.KwaakaTableID, Position: t.Position, Label: t.Label}
			if checked {
				ok := inPOS[t.KwaakaTableID]
				pt.InPOS = &ok
			}
			v.Pool = append(v.Pool, pt)
		}
	}
	if !g.Enabled {
		v.Blockers = append(v.Blockers, KwaakaBlockerGlobalOff)
	}
	switch {
	case link == "":
		v.Blockers = append(v.Blockers, KwaakaBlockerNotLinked)
	case set == nil:
		v.Blockers = append(v.Blockers, KwaakaBlockerNotSetUp)
	default:
		if !set.OrdersEnabled {
			v.Blockers = append(v.Blockers, KwaakaBlockerDisabled)
		}
		if len(set.Pool) == 0 {
			v.Blockers = append(v.Blockers, KwaakaBlockerPoolEmpty)
		}
		if v.PoolStale {
			v.Blockers = append(v.Blockers, KwaakaBlockerPoolStale)
		}
	}
	v.WillSend = len(v.Blockers) == 0
	return v
}

// normalizeKwaakaPool trims and validates the requested pool and assigns
// positions from the list order.
func normalizeKwaakaPool(in []KwaakaPoolTableInput) ([]domain.KwaakaPoolTable, error) {
	if len(in) > maxKwaakaPoolTables {
		return nil, fmt.Errorf("%w: pool has more than %d tables", domain.ErrValidation, maxKwaakaPoolTables)
	}
	out := make([]domain.KwaakaPoolTable, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, t := range in {
		id := strings.TrimSpace(t.KwaakaTableID)
		if id == "" || len(id) > maxKwaakaTableIDLen {
			return nil, fmt.Errorf("%w: pool[%d].kwaaka_table_id must be 1..%d characters", domain.ErrValidation, i, maxKwaakaTableIDLen)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: pool has table %q twice", domain.ErrValidation, id)
		}
		seen[id] = struct{}{}
		label := strings.TrimSpace(t.Label)
		if utf8.RuneCountInString(label) > maxKwaakaLabelRunes {
			return nil, fmt.Errorf("%w: pool[%d].label is longer than %d characters", domain.ErrValidation, i, maxKwaakaLabelRunes)
		}
		out = append(out, domain.KwaakaPoolTable{KwaakaTableID: id, Position: i, Label: label})
	}
	return out, nil
}

// samePoolIDs reports whether two pools hold the same tables in the same order
// (labels do not matter: relabelling needs no POS proof).
func samePoolIDs(a, b []domain.KwaakaPoolTable) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].KwaakaTableID != b[i].KwaakaTableID {
			return false
		}
	}
	return true
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func poolIDs(p []domain.KwaakaPoolTable) string {
	ids := make([]string, len(p))
	for i, t := range p {
		ids[i] = t.KwaakaTableID
	}
	return strings.Join(ids, ",")
}

func poolOf(s *domain.KwaakaOrderSettings) []domain.KwaakaPoolTable {
	if s == nil {
		return nil
	}
	return s.Pool
}

func leadOf(s *domain.KwaakaOrderSettings) *int {
	if s == nil {
		return nil
	}
	return s.LeadMinutes
}

func kwaakaIDOf(s *domain.KwaakaOrderSettings) string {
	if s == nil {
		return ""
	}
	return s.KwaakaRestaurantID
}

func fmtIntPtr(n *int) string {
	if n == nil {
		return "null"
	}
	return fmt.Sprint(*n)
}
