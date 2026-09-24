package kwaakaorders

import (
	"context"
	"log/slog"
	"time"

	"backend-core/internal/domain"
)

// Outbox is the slice of domain.BookingOutboxRepository this package writes to.
type Outbox interface {
	Create(ctx context.Context, e *domain.BookingOutboxEvent) error
}

// Config is the loop's tuning, env-driven in bootstrap.
type Config struct {
	// Enabled is the global switch for SENDING new orders (KWAAKA_ORDERS_ENABLED).
	// Cancels of already sent orders always run.
	Enabled      bool
	DefaultLead  time.Duration // KWAAKA_KITCHEN_LEAD, used when a venue has none
	MaxAttempts  int           // KWAAKA_ORDER_MAX_ATTEMPTS
	LeaseFor     time.Duration
	CandidateCap int
}

func (c Config) withDefaults() Config {
	if c.DefaultLead <= 0 {
		c.DefaultLead = 60 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.LeaseFor <= 0 {
		c.LeaseFor = 2 * time.Minute
	}
	if c.CandidateCap <= 0 {
		c.CandidateCap = 200
	}
	return c
}

// Worker runs the kitchen-order loop. Dependencies are positional, as in the
// other usecases.
type Worker struct {
	orders   domain.KitchenOrderRepository
	settings domain.KwaakaOrderSettingsRepository
	pos      domain.KwaakaOrderPOS
	outbox   Outbox
	tx       domain.TxManager
	cfg      Config
	log      *slog.Logger
	now      func() time.Time
}

// NewWorker builds the worker.
func NewWorker(orders domain.KitchenOrderRepository, settings domain.KwaakaOrderSettingsRepository,
	pos domain.KwaakaOrderPOS, outbox Outbox, tx domain.TxManager, cfg Config, log *slog.Logger) *Worker {
	return &Worker{orders: orders, settings: settings, pos: pos, outbox: outbox, tx: tx,
		cfg: cfg.withDefaults(), log: log, now: time.Now}
}
