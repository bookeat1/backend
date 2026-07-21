package bootstrap

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
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
}

type AppConfig struct {
	Name               string
	Environment        string
	URL                string
	LogLevel           string
	CORSAllowedOrigins []string
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

	// ServiceFeeBps is the BookEat service fee charged to the guest, in basis
	// points (350 = 3.5%). Basis points, not a float percentage: 3.5% in a
	// float is a rounding error in somebody else's wallet.
	ServiceFeeBps int // env: PAYMENTS_SERVICE_FEE_BPS

	// RefundAcquiringBps is what is withheld from a refund to cover the cost of
	// moving money back, in basis points of the total (100 = 1%). It is a cost
	// booked to the `acquirer` ledger account, not platform revenue.
	RefundAcquiringBps int // env: PAYMENTS_REFUND_ACQUIRING_BPS

	// DepositDefaultMinor is the deposit charged per booking, in tiyn, when the
	// venue requires one but sets no amount of its own.
	DepositDefaultMinor int64 // env: PAYMENTS_DEPOSIT_DEFAULT_MINOR

	// HoldTTL is how long an authorization is expected to stay valid. The
	// acquirer has the final say; this drives payments.expires_at and the
	// reconciliation worker.
	HoldTTL time.Duration // env: PAYMENTS_HOLD_TTL
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
			CORSAllowedOrigins: getEnvList("APP_CORS_ORIGINS", "*"),
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
			Enabled:             getEnvBool("PAYMENTS_ENABLED", false),
			DefaultProvider:     getEnv("PAYMENTS_DEFAULT_PROVIDER", "freedompay"),
			ServiceFeeBps:       getEnvInt("PAYMENTS_SERVICE_FEE_BPS", 350),
			RefundAcquiringBps:  getEnvInt("PAYMENTS_REFUND_ACQUIRING_BPS", 100),
			DepositDefaultMinor: getEnvInt64("PAYMENTS_DEPOSIT_DEFAULT_MINOR", 0),
			HoldTTL:             getEnvDuration("PAYMENTS_HOLD_TTL", 168*time.Hour),
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
