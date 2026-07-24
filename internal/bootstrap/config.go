package bootstrap

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"backend-core/internal/transport/rest/middleware"
)

// Config is the whole application configuration, built from environment
// variables. Grow it with new sections (Redis, external services, …) as the
// domain requires — one struct per concern, wired in NewConfig.
type Config struct {
	App      AppConfig
	DB       DBConfig
	Auth     AuthConfig
	Booking  BookingConfig
	Worker   WorkerConfig
	Payments PaymentsConfig
	// PaymentsReconciler configures the background payments reconciliation
	// worker (usecase/payments.Reconciler). KNOWN GAP, disclosed: this
	// config is read and validated here, but cmd/worker does not construct
	// the reconciler yet — that needs a real Postgres implementation of
	// domain.PaymentRepository / PaymentRefundRepository / PaymentLedgerRepository
	// / PaymentOutboxRepository, which does not exist in this branch (only
	// in-memory test fakes do, same KNOWN GAP as the rest of usecase/payments —
	// see team-memory's payments-usecase notes). Wiring RunWorker to start it
	// is the next step once that adapter lands.
	PaymentsReconciler PaymentsReconcilerConfig

	// Push configures the web-push notification channel (VAPID keys) and the
	// notification dispatcher worker. Absent VAPID keys make the channel a
	// clean no-op — the dispatcher still runs and drains the outbox, it just
	// sends nothing until the owner provisions keys.
	Push PushConfig

	// RateLimit configures middleware.RateLimit and the in-memory limiter
	// backing it (per-client-IP request budgets, one per route tier — see
	// that middleware's doc comment for which routes fall into which tier
	// and why webhooks get their own profile).
	RateLimit RateLimiterConfig
}

// RateLimiterConfig bundles middleware.RateLimit's own config (budgets, one
// per tier) with the memory-bound settings for the InMemoryLimiter backing
// it in bootstrap.NewApp. Kept as one section because both are read from the
// same RATE_LIMIT_* env prefix and always constructed together.
type RateLimiterConfig struct {
	middleware.RateLimitConfig

	// IdleTTL/SweepEvery bound the limiter's memory: a bucket untouched for
	// longer than IdleTTL is evicted, checked at most once per SweepEvery.
	// See middleware.NewInMemoryLimiter.
	IdleTTL    time.Duration // env: RATE_LIMIT_IDLE_TTL
	SweepEvery time.Duration // env: RATE_LIMIT_SWEEP_INTERVAL
}

type AppConfig struct {
	Name               string
	Environment        string
	URL                string
	LogLevel           string
	LogFormat          string // env: APP_LOG_FORMAT — "json" (default) or "text"
	CORSAllowedOrigins []string

	// TrustedProxies lists the IPs/CIDRs allowed to set X-Forwarded-For /
	// X-Real-IP and have gin's Context.ClientIP() believe them (env:
	// APP_TRUSTED_PROXIES, comma-separated). Empty (the default) means trust
	// nobody — ClientIP() falls back to the raw TCP peer address, which in
	// the deploy topology (Caddy is the only public listener, `app` has no
	// published port — see deploy/docker-compose.yml) is Caddy's own
	// container address, so nothing breaks, it is just not per-real-client.
	// Set this to Caddy's address on the compose network (see
	// deploy/.env.example) so ClientIP() — and therefore
	// middleware.RateLimit's per-IP buckets — resolve the actual caller
	// instead of Caddy. Defaulting to "trust nobody" rather than guessing a
	// docker-compose subnet is deliberate: an unverified guess here would be
	// worse than the safe-but-imprecise default (see NewApp's
	// SetTrustedProxies call for how this feeds gin).
	TrustedProxies []string
}

type DBConfig struct {
	Postgres PostgresConfig
}

type PostgresConfig struct {
	Host            string
	Port            int
	Database        string
	Username        string
	Password        string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type AuthConfig struct {
	JWTPrivateKeyPEM    string        // RSA private key (PEM). env: AUTH_JWT_PRIVATE_KEY
	JWTKeyID            string        // kid advertised in JWKS. env: AUTH_JWT_KID
	AccessTokenTTL      time.Duration // env: AUTH_ACCESS_TOKEN_TTL
	RefreshTokenTTL     time.Duration // env: AUTH_REFRESH_TOKEN_TTL
	OTPCodeTTL          time.Duration // env: AUTH_OTP_TTL
	OTPRateLimitPerMin  int           // env: AUTH_OTP_RATE_PER_MIN
	OTPRateLimitPerHour int           // env: AUTH_OTP_RATE_PER_HOUR
	OTPDevExpose        bool          // env: AUTH_OTP_DEV_EXPOSE — echo code in response (dev only)
}

// BookingConfig holds the global (level-1) booking policy. A restaurant may
// override any of these per venue (restaurants.booking_* columns, all NULLABLE
// — NULL means "use the value from here"). Resolution: usecase/bookings.
type BookingConfig struct {
	DefaultDuration       time.Duration // env: BOOKING_DEFAULT_DURATION_MINUTES
	DefaultBuffer         time.Duration // env: BOOKING_DEFAULT_BUFFER_MINUTES — cleanup gap added on both sides of the occupied slot
	DefaultLead           time.Duration // env: BOOKING_DEFAULT_LEAD_MINUTES — minimum distance from now to starts_at
	DefaultHorizonDays    int           // env: BOOKING_DEFAULT_HORIZON_DAYS — furthest bookable day ahead
	DefaultCancelDeadline time.Duration // env: BOOKING_DEFAULT_CANCEL_DEADLINE_MINUTES — guest may cancel until starts_at minus this
	DefaultConfirmSLA     time.Duration // env: BOOKING_DEFAULT_CONFIRM_SLA_MINUTES — pending auto-confirm / escalation deadline
	DefaultMaxGuests      int           // env: BOOKING_DEFAULT_MAX_GUESTS
	DefaultAutoConfirm    bool          // env: BOOKING_DEFAULT_AUTO_CONFIRM
	TimezoneFallback      string        // env: BOOKING_TIMEZONE_FALLBACK — IANA name used when restaurants.timezone is NULL

	// Anti-fraud: at most RateLimit booking attempts per normalized phone
	// within RateWindow (booking_rate_log).
	RateLimit  int           // env: BOOKING_RATE_LIMIT
	RateWindow time.Duration // env: BOOKING_RATE_WINDOW

	// SlotStep is the granularity used to generate bookable start times for a
	// venue that publishes opening hours but no explicit time slots.
	SlotStep time.Duration // env: BOOKING_SLOT_STEP_MINUTES
}

// WorkerConfig configures the background booking worker (cmd/worker): how
// often it wakes up and how long a finished booking is left alone before it is
// closed as completed / no_show. The per-venue booking policy is NOT here — it
// is resolved from BookingConfig plus the restaurant's overrides.
type WorkerConfig struct {
	TickInterval time.Duration // env: WORKER_TICK_INTERVAL
	NoShowGrace  time.Duration // env: WORKER_NO_SHOW_GRACE
	BatchSize    int           // env: WORKER_BATCH_SIZE — bookings claimed per pass
}

// PaymentsConfig holds the global (level-1) payment settings. A restaurant may
// override most of them per venue (restaurants.payments_enabled /
// deposit_* / preorder_payment_required / service_fee_bps / payment_provider,
// all NULLABLE — NULL means "use the value from here"). Resolution:
// usecase/payments.
//
// Acquirer credentials are deliberately NOT part of this struct: each adapter
// reads its own keys from env, and they never reach the database (spec §8).
type PaymentsConfig struct {
	Enabled bool // env: PAYMENTS_ENABLED — master switch, off by default

	// DefaultProvider is the acquirer used when the venue has no preference or
	// its preferred one is disabled in the payment_providers registry.
	DefaultProvider string // env: PAYMENTS_DEFAULT_PROVIDER

	// ServiceFeeBps is the acquirer's fee rate, in basis points (350 = 3.5%
	// prod, 290 = 2.9% sandbox). The guest is charged a grossed-up total so the
	// venue nets the full base after the acquirer withholds this rate from the
	// total (see domain.GrossUpForAcquirer). BookEat earns from the venue's
	// subscription, not from this fee — its take on the payment is ~zero. Basis
	// points, not a float: 3.5% in a float is a rounding error in someone's wallet.
	ServiceFeeBps int // env: PAYMENTS_SERVICE_FEE_BPS

	// RefundAcquiringBps is what is withheld from a refund to cover the cost of
	// moving money back, in basis points of the total (100 = 1%). It is a cost
	// booked to the `acquirer` ledger account, not platform revenue.
	RefundAcquiringBps int // env: PAYMENTS_REFUND_ACQUIRING_BPS

	// DepositDefaultMinor is the deposit charged per booking, in tiyn, when the
	// venue requires one but sets no amount of its own.
	DepositDefaultMinor int64 // env: PAYMENTS_DEPOSIT_DEFAULT_MINOR

	// DepositRequired and PreorderPaymentRequired are the GLOBAL fallback for
	// restaurants.deposit_required / preorder_payment_required when a venue
	// sets neither override (payments review 2026-07-23, item #10): without
	// these, usecase/payments.resolveAmount always found "no payment
	// required" for any restaurant running on the global defaults, so
	// CreateForBooking rejected every checkout with ErrValidation. A venue's
	// own override (NULLABLE columns from migration 0007) still wins when set.
	DepositRequired         bool // env: PAYMENTS_DEPOSIT_REQUIRED
	PreorderPaymentRequired bool // env: PAYMENTS_PREORDER_PAYMENT_REQUIRED

	// HoldTTL is how long an authorization is expected to stay valid. The
	// acquirer has the final say; this drives payments.expires_at and the
	// reconciliation worker. Kept below FreedomPay's 5-day auto-clearing of an
	// uncleared two-stage payment: a hold left past that is charged to the guest
	// instead of expiring, which is the opposite of what an expiry should do.
	HoldTTL time.Duration // env: PAYMENTS_HOLD_TTL

	// FreeCancelWindow is the GLOBAL default free-cancellation window for the
	// money path, applied to any restaurant that has not overridden
	// free_cancel_window_minutes (migration 0034/0035). A deposit hold is
	// released to the guest only when the booking is cancelled earlier than
	// this before starts_at; a later cancellation or a no-show forfeits it to
	// the venue. Owner-confirmed default 120 minutes.
	FreeCancelWindow time.Duration // env: PAYMENTS_FREE_CANCEL_WINDOW_MINUTES

	// PublicBaseURL is this backend's own externally-reachable origin (e.g.
	// https://api.bookeat.kz), used ONLY to build the webhook CallbackURL
	// handed to an acquirer at Authorize time. It is never taken from the
	// client — a client-supplied callback URL would let an attacker redirect
	// our own webhook delivery. TipTopPay ignores CallbackURL entirely (its
	// notification endpoints are configured once in its own merchant
	// dashboard, see tiptoppay.Gateway.Authorize's doc comment), so in
	// practice this only ever has to be FreedomPay-shaped; it is still built
	// from a single base URL rather than hardcoding the route twice.
	PublicBaseURL string // env: PAYMENTS_PUBLIC_BASE_URL
}

// PaymentsReconcilerConfig configures the background payments reconciliation
// worker: how often it wakes up, how long a transient claim (capturing /
// voiding / a refund in_flight or pending) may sit before it counts as stuck,
// how long a created/authorized payment may go without a status change
// before its acquirer status is read directly (in case a webhook was lost),
// how many rows one pass claims per stage, how many consecutive unresolved
// attempts flag a row for manual review, and the minimum spacing between two
// acquirer calls (the avalanche guard).
type PaymentsReconcilerConfig struct {
	TickInterval     time.Duration // env: PAYMENTS_RECONCILE_TICK_INTERVAL
	StuckAfter       time.Duration // env: PAYMENTS_RECONCILE_STUCK_AFTER
	LostWebhookAfter time.Duration // env: PAYMENTS_RECONCILE_LOST_WEBHOOK_AFTER
	BatchSize        int           // env: PAYMENTS_RECONCILE_BATCH_SIZE
	MaxAttempts      int           // env: PAYMENTS_RECONCILE_MAX_ATTEMPTS
	ProviderMinGap   time.Duration // env: PAYMENTS_RECONCILE_PROVIDER_MIN_GAP
}

// PushConfig holds the web-push channel's VAPID keys and the notification
// dispatcher's scheduling. The VAPID keys come from env only and are never
// logged (same discipline as acquirer credentials). When the keys are absent
// the web-push notifier is built disabled and no-ops cleanly.
type PushConfig struct {
	VAPIDPublicKey  string        // env: PUSH_VAPID_PUBLIC_KEY
	VAPIDPrivateKey string        // env: PUSH_VAPID_PRIVATE_KEY
	VAPIDSubject    string        // env: PUSH_VAPID_SUBJECT (mailto:/https: URL)
	TTL             time.Duration // env: PUSH_TTL — push-service message retention
	DispatchTick    time.Duration // env: NOTIFY_DISPATCH_TICK_INTERVAL
	DispatchBatch   int           // env: NOTIFY_DISPATCH_BATCH_SIZE
}

func (p PostgresConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		p.Host, p.Port, p.Username, p.Password, p.Database, p.SSLMode,
	)
}

// NewConfig builds the application configuration from environment variables,
// falling back to sane defaults. A `.env` file in the working directory is
// loaded automatically when present (real environment variables take
// precedence over it).
func NewConfig() (Config, error) {
	// Load .env if it exists; absence is not an error (env may be provided
	// directly by the shell, Docker, or the orchestrator).
	_ = godotenv.Load()

	cfg := Config{
		App: AppConfig{
			Name:               getEnv("APP_NAME", "backend-core"),
			Environment:        getEnv("APP_ENV", "development"),
			URL:                getEnv("APP_URL", "0.0.0.0:8080"),
			LogLevel:           getEnv("APP_LOG_LEVEL", "info"),
			LogFormat:          getEnv("APP_LOG_FORMAT", "json"),
			CORSAllowedOrigins: getEnvList("APP_CORS_ORIGINS", "*"),
			TrustedProxies:     getEnvList("APP_TRUSTED_PROXIES", ""),
		},
		DB: DBConfig{
			Postgres: PostgresConfig{
				Host:            getEnv("DB_HOST", "localhost"),
				Port:            getEnvInt("DB_PORT", 5432),
				Database:        getEnv("DB_DATABASE", "bookeat"),
				Username:        getEnv("DB_USERNAME", "postgres"),
				Password:        getEnv("DB_PASSWORD", "postgres"),
				SSLMode:         getEnv("DB_SSLMODE", "disable"),
				MaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 25),
				MaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 25),
				ConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute),
				ConnMaxIdleTime: getEnvDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
			},
		},
		Auth: AuthConfig{
			JWTPrivateKeyPEM:    getEnv("AUTH_JWT_PRIVATE_KEY", ""),
			JWTKeyID:            getEnv("AUTH_JWT_KID", "bookeat-dev"),
			AccessTokenTTL:      getEnvDuration("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL:     getEnvDuration("AUTH_REFRESH_TOKEN_TTL", 720*time.Hour),
			OTPCodeTTL:          getEnvDuration("AUTH_OTP_TTL", 5*time.Minute),
			OTPRateLimitPerMin:  getEnvInt("AUTH_OTP_RATE_PER_MIN", 1),
			OTPRateLimitPerHour: getEnvInt("AUTH_OTP_RATE_PER_HOUR", 5),
			OTPDevExpose:        getEnvBool("AUTH_OTP_DEV_EXPOSE", false),
		},
		Booking: BookingConfig{
			DefaultDuration:       getEnvMinutes("BOOKING_DEFAULT_DURATION_MINUTES", 90),
			DefaultBuffer:         getEnvMinutes("BOOKING_DEFAULT_BUFFER_MINUTES", 0),
			DefaultLead:           getEnvMinutes("BOOKING_DEFAULT_LEAD_MINUTES", 60),
			DefaultHorizonDays:    getEnvInt("BOOKING_DEFAULT_HORIZON_DAYS", 60),
			DefaultCancelDeadline: getEnvMinutes("BOOKING_DEFAULT_CANCEL_DEADLINE_MINUTES", 180),
			DefaultConfirmSLA:     getEnvMinutes("BOOKING_DEFAULT_CONFIRM_SLA_MINUTES", 120),
			DefaultMaxGuests:      getEnvInt("BOOKING_DEFAULT_MAX_GUESTS", 20),
			DefaultAutoConfirm:    getEnvBool("BOOKING_DEFAULT_AUTO_CONFIRM", true),
			TimezoneFallback:      getEnv("BOOKING_TIMEZONE_FALLBACK", "Asia/Almaty"),
			RateLimit:             getEnvInt("BOOKING_RATE_LIMIT", 10),
			RateWindow:            getEnvDuration("BOOKING_RATE_WINDOW", time.Hour),
			SlotStep:              getEnvMinutes("BOOKING_SLOT_STEP_MINUTES", 30),
		},
		Worker: WorkerConfig{
			TickInterval: getEnvDuration("WORKER_TICK_INTERVAL", time.Minute),
			NoShowGrace:  getEnvDuration("WORKER_NO_SHOW_GRACE", 30*time.Minute),
			BatchSize:    getEnvInt("WORKER_BATCH_SIZE", 100),
		},
		Payments: PaymentsConfig{
			Enabled:                 getEnvBool("PAYMENTS_ENABLED", false),
			DefaultProvider:         getEnv("PAYMENTS_DEFAULT_PROVIDER", "freedompay"),
			ServiceFeeBps:           getEnvInt("PAYMENTS_SERVICE_FEE_BPS", 350),
			RefundAcquiringBps:      getEnvInt("PAYMENTS_REFUND_ACQUIRING_BPS", 100),
			DepositDefaultMinor:     getEnvInt64("PAYMENTS_DEPOSIT_DEFAULT_MINOR", 0),
			DepositRequired:         getEnvBool("PAYMENTS_DEPOSIT_REQUIRED", false),
			PreorderPaymentRequired: getEnvBool("PAYMENTS_PREORDER_PAYMENT_REQUIRED", false),
			HoldTTL:                 getEnvDuration("PAYMENTS_HOLD_TTL", 96*time.Hour),
			FreeCancelWindow:        getEnvMinutes("PAYMENTS_FREE_CANCEL_WINDOW_MINUTES", 120),
			PublicBaseURL:           strings.TrimRight(getEnv("PAYMENTS_PUBLIC_BASE_URL", ""), "/"),
		},
		PaymentsReconciler: PaymentsReconcilerConfig{
			TickInterval:     getEnvDuration("PAYMENTS_RECONCILE_TICK_INTERVAL", 2*time.Minute),
			StuckAfter:       getEnvDuration("PAYMENTS_RECONCILE_STUCK_AFTER", 10*time.Minute),
			LostWebhookAfter: getEnvDuration("PAYMENTS_RECONCILE_LOST_WEBHOOK_AFTER", time.Hour),
			BatchSize:        getEnvInt("PAYMENTS_RECONCILE_BATCH_SIZE", 50),
			MaxAttempts:      getEnvInt("PAYMENTS_RECONCILE_MAX_ATTEMPTS", 5),
			ProviderMinGap:   getEnvDuration("PAYMENTS_RECONCILE_PROVIDER_MIN_GAP", 200*time.Millisecond),
		},
		Push: PushConfig{
			VAPIDPublicKey:  getEnv("PUSH_VAPID_PUBLIC_KEY", ""),
			VAPIDPrivateKey: getEnv("PUSH_VAPID_PRIVATE_KEY", ""),
			VAPIDSubject:    getEnv("PUSH_VAPID_SUBJECT", ""),
			TTL:             getEnvDuration("PUSH_TTL", 24*time.Hour),
			DispatchTick:    getEnvDuration("NOTIFY_DISPATCH_TICK_INTERVAL", 15*time.Second),
			DispatchBatch:   getEnvInt("NOTIFY_DISPATCH_BATCH_SIZE", 100),
		},
		RateLimit: RateLimiterConfig{
			RateLimitConfig: middleware.RateLimitConfig{
				Enabled: getEnvBool("RATE_LIMIT_ENABLED", true),
				// Strict: OTP send, booking/payment creation, guest checkout
				// settle. 5 requests/minute per IP per route is deliberately
				// tight — a real user retries a couple of times at most; a
				// script left running overnight is exactly what this exists
				// to stop (see the task that motivated this middleware).
				// This is IN ADDITION TO, not instead of, usecase/auth's own
				// per-phone OTP limiter (1/min, 5/hour) — that one guards
				// the SMS bill per phone number, this one guards the
				// endpoint per source IP regardless of which phone numbers
				// it cycles through.
				Strict: middleware.RateLimitBudget{
					Limit:  getEnvInt("RATE_LIMIT_STRICT_LIMIT", 5),
					Window: getEnvDuration("RATE_LIMIT_STRICT_WINDOW", time.Minute),
				},
				// Soft: public listings/menus/availability. Generous —
				// legitimate browsing easily bursts above 5/min.
				Soft: middleware.RateLimitBudget{
					Limit:  getEnvInt("RATE_LIMIT_SOFT_LIMIT", 60),
					Window: getEnvDuration("RATE_LIMIT_SOFT_WINDOW", time.Minute),
				},
				// Webhook: acquirer callbacks. Wide on purpose — see
				// middleware.RateLimit's doc for why this is not the strict
				// profile; this budget only exists to bound an unrelated
				// flood at this public route, not to throttle the
				// acquirer's own retry behaviour.
				Webhook: middleware.RateLimitBudget{
					Limit:  getEnvInt("RATE_LIMIT_WEBHOOK_LIMIT", 120),
					Window: getEnvDuration("RATE_LIMIT_WEBHOOK_WINDOW", time.Minute),
				},
				// Default: every authenticated route not explicitly
				// classified (staff capture/void, admin CRUD, /me, booking
				// messages, …) — moderate floor, not a wall.
				Default: middleware.RateLimitBudget{
					Limit:  getEnvInt("RATE_LIMIT_DEFAULT_LIMIT", 30),
					Window: getEnvDuration("RATE_LIMIT_DEFAULT_WINDOW", time.Minute),
				},
			},
			IdleTTL:    getEnvDuration("RATE_LIMIT_IDLE_TTL", 10*time.Minute),
			SweepEvery: getEnvDuration("RATE_LIMIT_SWEEP_INTERVAL", time.Minute),
		},
	}

	return cfg, nil
}

// getEnv returns the value of the environment variable named by key, or def
// when the variable is unset.
func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// getEnvInt returns the integer value of the environment variable named by
// key, or def when the variable is unset or not a valid integer.
func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// getEnvMinutes returns the environment variable named by key interpreted as a
// whole number of minutes, or defMinutes when unset or unparseable. Negative
// values fall back to the default (a negative buffer or lead is meaningless).
func getEnvMinutes(key string, defMinutes int) time.Duration {
	n := getEnvInt(key, defMinutes)
	if n < 0 {
		n = defMinutes
	}
	return time.Duration(n) * time.Minute
}

// getEnvInt64 returns the 64-bit integer value of the environment variable
// named by key, or def when the variable is unset or not a valid integer. Money
// amounts are int64 (tiyn) everywhere, so they need their own reader.
func getEnvInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

// getEnvDuration returns the duration value of the environment variable named
// by key, or def when the variable is unset or not a valid Go duration.
func getEnvDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

// getEnvList returns the comma-separated value of the environment variable named
// by key (each element trimmed, empties dropped), or def parsed the same way
// when the variable is unset.
func getEnvList(key, def string) []string {
	raw := getEnv(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// getEnvBool returns the boolean value of the environment variable named by
// key, or def when unset or unparseable. Accepts 1/t/true/0/f/false.
func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}
