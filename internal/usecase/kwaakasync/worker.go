package kwaakasync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// Config is the sync worker's scheduling, same env-driven convention as the
// other background workers (e.g. usecase/legacysync.Config).
type Config struct {
	// TickInterval is the pause between two full passes over every linked
	// restaurant. env: KWAAKA_SYNC_TICK_INTERVAL
	TickInterval time.Duration
}

const defaultTickInterval = 5 * time.Minute

func (c Config) withDefaults() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	return c
}

// Worker periodically pulls the CURRENT menu + availability from Kwaaka for
// every restaurant with a Kwaaka binding, and upserts it into menu_items. A
// restaurant without KwaakaRestaurantID is never listed by RestaurantSource
// and therefore never touched — a hand-entered menu is safe from this worker
// by construction, not by a runtime check here.
type Worker struct {
	restaurants RestaurantSource
	fetcher     domain.KwaakaMenuSource
	items       domain.MenuItemRepository
	tx          domain.TxManager
	cfg         Config
	log         *slog.Logger
}

// NewWorker builds the sync worker.
func NewWorker(restaurants RestaurantSource, fetcher domain.KwaakaMenuSource, items domain.MenuItemRepository, tx domain.TxManager, cfg Config, log *slog.Logger) *Worker {
	return &Worker{restaurants: restaurants, fetcher: fetcher, items: items, tx: tx, cfg: cfg.withDefaults(), log: log}
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on
// the next tick, never fatal — same contract as the other workers' Run.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.TickInterval)
	defer t.Stop()
	w.log.Info("kwaaka menu sync started", slog.Duration("tick", w.cfg.TickInterval))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("kwaaka menu sync stopped")
			return nil
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				w.log.Error("kwaaka menu sync tick failed", slog.String("error", err.Error()))
			}
		}
	}
}

// RestaurantResult counts what one restaurant's sync did.
type RestaurantResult struct {
	Fetched   int
	Upserted  int
	Stopped   int
	Skipped   int
	VenueName string
}

// Tick runs one full pass over every Kwaaka-linked restaurant. One
// restaurant's failure (a Kwaaka outage, a malformed response) is logged and
// the pass continues to the next restaurant — a single bad venue must never
// stall the rest of the platform's menus from refreshing.
func (w *Worker) Tick(ctx context.Context) error {
	linked, err := w.restaurants.ListKwaakaLinked(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, r := range linked {
		res, err := w.syncRestaurant(ctx, r)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			w.log.Error(logging.EventKwaakaSyncFailed,
				slog.String("restaurant_id", r.RestaurantID.String()),
				slog.String("kwaaka_restaurant_id", r.KwaakaRestaurantID),
				slog.String("error", err.Error()))
			continue
		}
		w.log.Info(logging.EventKwaakaSyncTick,
			slog.String("restaurant_id", r.RestaurantID.String()),
			slog.Int("fetched", res.Fetched),
			slog.Int("upserted", res.Upserted),
			slog.Int("stopped", res.Stopped),
			slog.Int("skipped", res.Skipped))
	}
	return firstErr
}

// syncRestaurant fetches ONE restaurant's full menu (a network call, kept
// outside any transaction) and then applies every write inside a single
// transaction: the venue's menu never sits half-updated between the upserts
// and the stop-list pass that follows them.
func (w *Worker) syncRestaurant(ctx context.Context, r domain.KwaakaLinkedRestaurant) (RestaurantResult, error) {
	menu, err := w.fetcher.FetchMenu(ctx, r.KwaakaRestaurantID)
	if err != nil {
		return RestaurantResult{}, err
	}
	res := RestaurantResult{Fetched: len(menu.Products)}
	keep := make([]string, 0, len(menu.Products))

	err = w.tx.WithinTx(ctx, func(ctx context.Context) error {
		for _, p := range menu.Products {
			if !domain.ValidPrice(p.Price) {
				res.Skipped++
				w.log.Warn("kwaaka product skipped (malformed price)",
					slog.String("restaurant_id", r.RestaurantID.String()),
					slog.String("kwaaka_product_id", p.ExternalID))
				continue
			}
			extID := p.ExternalID
			item := domain.MenuItem{
				ID:              uuid.New(),
				RestaurantID:    r.RestaurantID,
				Name:            p.Name,
				Description:     p.Description,
				Price:           p.Price,
				IsAvailable:     p.IsAvailable,
				Category:        categoryPtr(p.Category),
				ImageURL:        imagePtr(p.ImageURL),
				KwaakaProductID: &extID,
			}
			if err := w.items.UpsertFromKwaaka(ctx, &item); err != nil {
				return err
			}
			res.Upserted++
			keep = append(keep, extID)
		}
		stopped, err := w.items.MarkUnavailableExceptKwaakaIDs(ctx, r.RestaurantID, keep)
		if err != nil {
			return err
		}
		res.Stopped = stopped
		return nil
	})
	if err != nil {
		return RestaurantResult{}, err
	}
	return res, nil
}

func categoryPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func imagePtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
