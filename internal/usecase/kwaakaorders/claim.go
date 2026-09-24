package kwaakaorders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// venueCache holds per-tick, per-venue lookups so a venue with many candidates
// costs one settings read and at most one POS load query.
type venueCache struct {
	settings *domain.KwaakaOrderSettings
	posLoad  map[string]int
	loadDone bool
}

// ClaimPass finds bookings that are due and creates their kitchen-order rows.
// It is a no-op while the global switch is off.
func (w *Worker) ClaimPass(ctx context.Context) error {
	if !w.cfg.Enabled {
		return nil
	}
	now := w.now()
	cands, err := w.orders.ListCandidates(ctx, now, w.cfg.CandidateCap)
	if err != nil {
		return err
	}
	cache := map[uuid.UUID]*venueCache{}
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		vc := cache[c.RestaurantID]
		if vc == nil {
			set, err := w.settings.Get(ctx, c.RestaurantID)
			if err != nil {
				w.log.Warn("kwaaka claim: settings", slog.String("restaurant_id", c.RestaurantID.String()), slog.String("error", err.Error()))
				continue
			}
			vc = &venueCache{settings: set}
			cache[c.RestaurantID] = vc
		}
		// Cheap unlocked pre-check: no HTTP for a booking that is not due yet.
		sub, err := w.orders.GetClaimSubject(ctx, c.BookingID)
		if err != nil {
			continue
		}
		if !dueAt(sub, w.leadFor(vc.settings), vc.settings.EnabledAt, now).Send {
			continue
		}
		if !vc.loadDone { // one GET per venue per tick, outside any transaction
			vc.posLoad = w.loadPosTables(ctx, vc.settings)
			vc.loadDone = true
		}
		if err := w.claimOne(ctx, c.BookingID, c.RestaurantID, vc); err != nil {
			w.log.Error("kwaaka claim failed", slog.String("booking_id", c.BookingID.String()), slog.String("error", err.Error()))
		}
	}
	return nil
}

func (w *Worker) leadFor(s *domain.KwaakaOrderSettings) time.Duration {
	if s.LeadMinutes != nil {
		return time.Duration(*s.LeadMinutes) * time.Minute
	}
	return w.cfg.DefaultLead
}

// loadPosTables counts open POS orders per pool table. nil = the POS did not
// answer usefully ("unknown", pickTable falls back to our own live count).
func (w *Worker) loadPosTables(ctx context.Context, s *domain.KwaakaOrderSettings) map[string]int {
	ids := make([]string, 0, len(s.Pool))
	inPool := map[string]bool{}
	for _, t := range s.Pool {
		ids = append(ids, t.KwaakaTableID)
		inPool[t.KwaakaTableID] = true
	}
	orders, err := w.pos.ListOrdersByTables(ctx, s.KwaakaRestaurantID, ids)
	if err != nil {
		w.log.Warn("kwaaka pool load unavailable", slog.String("restaurant_id", s.RestaurantID.String()), slog.String("error", err.Error()))
		return nil
	}
	load := map[string]int{}
	for _, o := range orders {
		if o.WhenClosed != nil || o.State.Terminal() {
			continue
		}
		for _, id := range o.TableIDs {
			if inPool[id] {
				load[id]++
			}
		}
	}
	return load
}

// claimOne is one claim transaction: lock booking, lock settings, re-check,
// pick the table, insert. No HTTP inside.
func (w *Worker) claimOne(ctx context.Context, bookingID, restaurantID uuid.UUID, vc *venueCache) error {
	return w.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := w.now()
		sub, err := w.orders.LockClaimSubject(ctx, bookingID) // 1st lock: booking
		if err != nil {
			return err
		}
		set, err := w.settings.LockForUpdate(ctx, restaurantID) // 2nd lock: settings
		if err != nil {
			return err
		}
		if !set.OrdersEnabled || len(set.Pool) == 0 || set.KwaakaRestaurantID != sub.KwaakaID {
			return nil
		}
		dec := dueAt(sub, w.leadFor(set), set.EnabledAt, now)
		if !dec.Send || len(sub.Items) == 0 {
			return nil
		}
		loc, err := domain.LoadVenueLocation(sub.Timezone)
		if err != nil {
			// Guest-facing text needs the venue clock; a venue without a valid
			// zone is a data problem, not a reason to send a wrong time.
			return fmt.Errorf("venue timezone: %w", err)
		}

		loads, err := w.orders.TableLoads(ctx, restaurantID, now)
		if err != nil {
			return err
		}
		table, shared := pickTable(set.Pool, vc.posLoad, loads.Inflight, loads.Live)
		if shared {
			w.log.Warn(logging.EventKwaakaOrderPoolExhausted,
				slog.String("restaurant_id", restaurantID.String()), slog.Int("pool_size", len(set.Pool)))
		}

		id := uuid.New()
		snap := buildSnapshot(sub, id.String(), table, loc)
		raw, err := json.Marshal(snap.Snapshot)
		if err != nil {
			return err
		}
		o := &domain.KitchenOrder{
			ID: id, BookingID: bookingID, RestaurantID: restaurantID, KwaakaRestaurantID: sub.KwaakaID,
			KwaakaTableID: table, TableShared: shared, Status: domain.KitchenOrderSending, Trigger: dec.Trigger,
			Paid: sub.Paid, TotalMinor: snap.TotalMinor, Partial: snap.Partial, RequestSnapshot: raw,
			BookingStartsAt: sub.StartsAt, DeadlineAt: dec.DeadlineAt, TableHoldUntil: tableHoldUntil(sub, now),
			NextAttemptAt: &now,
		}
		if sub.Paid {
			p := sub.PaidMinor
			o.PaidAmountMinor = &p
		}
		if snap.NoPosItems {
			// Nothing the POS knows: no HTTP, straight to failed + one alert.
			code, msg := domain.KitchenErrNoPosItems, "no dish of the pre-order is mapped to a POS product"
			o.Status, o.NextAttemptAt, o.ErrorCode, o.LastError = domain.KitchenOrderFailed, nil, &code, &msg
		}
		created, err := w.orders.Insert(ctx, o)
		if err != nil {
			return err
		}
		if created && snap.NoPosItems {
			return w.alert(ctx, o, "failed", *o.LastError)
		}
		return nil
	})
}

var errLostCAS = errors.New("kitchen order changed concurrently")
