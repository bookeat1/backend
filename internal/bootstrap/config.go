package bootstrap

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"backend-core/internal/infrastructure/whatsapp"
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

	// TicketsSweep configures the pending-event-ticket sweep worker
	// (usecase/tickets.PendingSweeper): it releases seats held by pending
	// tickets whose payment never completed (or was never created).
	TicketsSweep TicketsSweepConfig

	// EventRecurrences configures the worker that materialises recurring-event
	// rules into real `events` rows for a rolling window ahead.
	EventRecurrences EventRecurrencesConfig

	// Payouts configures the scheduled daily payout pass (one payout per venue
	// per venue-local day) and the money policy of a payout: the acquirer's
	// fee, who bears it, and the minimum below which money rolls over.
	Payouts PayoutsConfig

	// Push configures the web-push notification channel (VAPID keys) and the
	// notification dispatcher worker. Absent VAPID keys make the channel a
	// clean no-op — the dispatcher still runs and drains the outbox, it just
	// sends nothing until the owner provisions keys.
	Push PushConfig

	// LegacySync configures the one-way data sync from the OLD BookEat system
	// (the live Supabase Postgres guests still book on during "Вариант Б") into
	// this backend. When LEGACY_DB_URL is empty the sync never starts — a clean
	// no-op, exactly like the other optional workers when their credentials are
	// absent.
	LegacySync LegacySyncConfig

	// RateLimit configures middleware.RateLimit and the in-memory limiter
	// backing it (per-client-IP request budgets, one per route tier — see
	// that middleware's doc comment for which routes fall into which tier
	// and why webhooks get their own profile).
	RateLimit RateLimiterConfig

	// Analytics configures the Amplitude analytics worker. Absent
	// AMPLITUDE_API_KEY makes the worker a clean no-op — it still ticks and
	// advances its cursor, it just ships nothing until the owner provisions a
	// key (same discipline as web push without VAPID keys).
	Analytics AnalyticsConfig

	// StaticMap configures the server-side restaurant map-preview proxy
	// (GET /api/v1/restaurants/:id/map). Absent provider key → the endpoint
	// still exists and answers a clean 503/map_not_configured, same discipline
	// as web push without VAPID keys: the service boots and works, one feature
	// says "not available yet" until the credential arrives.
	StaticMap StaticMapConfig

	// OTPDelivery configures the login-code delivery waterfall (Telegram
	// Gateway → WhatsApp → SMS). Each channel is enabled ONLY by the presence
	// of its credentials; absent credentials are not an error, they just mean
	// the channel does not exist. With none of them configured the application
	// keeps today's stub behaviour (dev logs the code, everything else warns),
	// so the tokens can arrive one at a time without a deploy in between.
	OTPDelivery OTPDeliveryConfig
}

// OTPDeliveryConfig holds the credentials and knobs of every OTP channel. All
// tokens come from env only and are never logged (same discipline as acquirer
// keys / VAPID keys); see infrastructure/otpsender for where each one goes.
type OTPDeliveryConfig struct {
	// ChannelOrder is the fallback order, cheapest-and-most-certain first.
	// Names are the domain.OTPChannel* values. A name whose channel is not
	// configured is skipped; an unknown name is ignored with a loud log rather
	// than crashing the server on a typo in one env var.
	// env: OTP_CHANNEL_ORDER (default "telegram_gateway,whatsapp,sms")
	ChannelOrder []string
	// SendTimeout caps ONE channel's attempt, so a hung provider cannot eat the
	// whole login request and starve the channels below it.
	// env: OTP_SEND_TIMEOUT
	SendTimeout time.Duration
	// DeliveryBudget caps the WHOLE waterfall (every fallback hop together). It
	// must stay under the HTTP server's WriteTimeout (15s, see bootstrap.app),
	// or a slow waterfall would keep spending money on a guest whose connection
	// has already been closed.
	// env: OTP_DELIVERY_BUDGET
	DeliveryBudget time.Duration

	// Telegram Gateway (https://gateway.telegram.org) — the paid product that
	// delivers a code to a PHONE NUMBER, not the notifications bot.
	TelegramGatewayToken  string // env: OTP_TELEGRAM_GATEWAY_TOKEN
	TelegramSenderUser    string // env: OTP_TELEGRAM_GATEWAY_SENDER_USERNAME (optional, verified channel)
	TelegramGatewayAPIURL string // env: OTP_TELEGRAM_GATEWAY_API_URL (tests/staging override)

	// WhatsApp Cloud API (Meta).
	WhatsAppAccessToken   string // env: OTP_WHATSAPP_ACCESS_TOKEN (System User token)
	WhatsAppPhoneNumberID string // env: OTP_WHATSAPP_PHONE_NUMBER_ID
	WhatsAppTemplateName  string // env: OTP_WHATSAPP_TEMPLATE_NAME (approved AUTHENTICATION template)
	WhatsAppTemplateLang  string // env: OTP_WHATSAPP_TEMPLATE_LANG
	WhatsAppAPIVersion    string // env: OTP_WHATSAPP_API_VERSION
	// WhatsAppCopyButton must match the approved template: an authentication
	// template WITH a "Copy code" button needs a button parameter, one without
	// it rejects the same request. Wrong value = every WhatsApp send fails.
	WhatsAppCopyButton bool   // env: OTP_WHATSAPP_COPY_CODE_BUTTON
	WhatsAppAPIURL     string // env: OTP_WHATSAPP_API_URL (tests/staging override)

	// SMSProvider selects the SMS backend: "" (none) | "twilio" | "mobizon".
	// It is an explicit switch rather than a guess from whichever credentials
	// happen to be present, so an old key left in the environment can never
	// silently take over the most expensive channel.
	SMSProvider string // env: OTP_SMS_PROVIDER

	TwilioAccountSID   string // env: OTP_SMS_TWILIO_ACCOUNT_SID
	TwilioAuthToken    string // env: OTP_SMS_TWILIO_AUTH_TOKEN
	TwilioFrom         string // env: OTP_SMS_TWILIO_FROM (E.164, or use the messaging service)
	TwilioMessagingSID string // env: OTP_SMS_TWILIO_MESSAGING_SERVICE_SID
	TwilioAPIURL       string // env: OTP_SMS_TWILIO_API_URL (tests override)

	MobizonAPIKey string // env: OTP_SMS_MOBIZON_API_KEY
	MobizonSender string // env: OTP_SMS_MOBIZON_SENDER (approved alphanumeric sender, optional)
	MobizonAPIURL string // env: OTP_SMS_MOBIZON_API_URL (tests override)
}

// AnalyticsConfig holds the Amplitude project credentials and the analytics
// dispatcher's scheduling. The API key comes from env only and is never logged
// (same discipline as acquirer credentials / VAPID keys). When it is absent the
// worker is built with a no-op sender.
type AnalyticsConfig struct {
	APIKey        string        // env: AMPLITUDE_API_KEY
	Endpoint      string        // env: AMPLITUDE_ENDPOINT (defaults to Amplitude US /2/httpapi)
	DispatchTick  time.Duration // env: ANALYTICS_DISPATCH_TICK_INTERVAL
	DispatchBatch int           // env: ANALYTICS_DISPATCH_BATCH_SIZE
	HTTPTimeout   time.Duration // env: ANALYTICS_HTTP_TIMEOUT
}

// Configured reports whether an Amplitude API key is present.
func (a AnalyticsConfig) Configured() bool { return strings.TrimSpace(a.APIKey) != "" }

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
	Name        string
	Environment string
	// BootstrapAdminEmail is promoted to platform administrator on startup, but
	// ONLY while the platform has no administrator at all. It answers "where
	// does the first administrator come from" without leaving a way to quietly
	// re-grant rights somebody removed on purpose.
	BootstrapAdminEmail string // env: BOOTSTRAP_ADMIN_EMAIL
	URL                 string
	LogLevel            string
	LogFormat           string // env: APP_LOG_FORMAT — "json" (default) or "text"
	CORSAllowedOrigins  []string

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

	// The App Store review account: ONE phone number that logs in with a fixed
	// code and no message sent (an App Review reviewer can receive neither a
	// Telegram code nor an SMS). Both empty = the feature does not exist; a half
	// configured pair is a startup error, never a silent "off". Any human phone
	// format is accepted — it is normalized the same way a login request is.
	// Rationale and rules: internal/usecase/auth/test_account.go.
	TestAccountPhone string // env: AUTH_TEST_ACCOUNT_PHONE
	TestAccountCode  string // env: AUTH_TEST_ACCOUNT_CODE
}

// BookingConfig holds the global (level-1) booking policy. A restaurant may
// override any of these per venue (restaurants.booking_* columns, all NULLABLE
// — NULL means "use the value from here"). Resolution: usecase/bookings.
type BookingConfig struct {
	DefaultDuration        time.Duration // env: BOOKING_DEFAULT_DURATION_MINUTES
	DefaultBuffer          time.Duration // env: BOOKING_DEFAULT_BUFFER_MINUTES — cleanup gap added on both sides of the occupied slot
	DefaultLead            time.Duration // env: BOOKING_DEFAULT_LEAD_MINUTES — minimum distance from now to starts_at
	DefaultHorizonDays     int           // env: BOOKING_DEFAULT_HORIZON_DAYS — furthest bookable day ahead
	DefaultCancelDeadline  time.Duration // env: BOOKING_DEFAULT_CANCEL_DEADLINE_MINUTES — guest may cancel until starts_at minus this
	DefaultConfirmSLA      time.Duration // env: BOOKING_DEFAULT_CONFIRM_SLA_MINUTES — pending auto-confirm / escalation deadline
	DefaultMaxGuests       int           // env: BOOKING_DEFAULT_MAX_GUESTS
	DefaultAutoConfirm     bool          // env: BOOKING_DEFAULT_AUTO_CONFIRM — confirm a pending booking when the SLA elapses
	DefaultConfirmOnCreate bool          // env: BOOKING_DEFAULT_CONFIRM_ON_CREATE — confirm a NEW booking without asking the venue
	TimezoneFallback       string        // env: BOOKING_TIMEZONE_FALLBACK — IANA name used when restaurants.timezone is NULL

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
	// ReminderLead is how long before starts_at the guest gets their pre-visit
	// reminder (one per booking). env: WORKER_GUEST_REMINDER_LEAD
	ReminderLead time.Duration

	// GuestRemindersEnabled switches the pre-visit reminder pass on. OFF by
	// default: while the old Supabase system still reminds the same guests, ours
	// would be the second (or third) message about one visit. Turning this on is
	// the owner's deliberate act once the old reminder path is dead.
	// env: WORKER_GUEST_REMINDERS_ENABLED
	GuestRemindersEnabled bool
	BatchSize             int // env: WORKER_BATCH_SIZE — bookings claimed per pass
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
	// booked to the `acquirer` ledger account, not platform revenue. Owner
	// decision (2026-07-25): 0 by default — nothing is taken off the guest's
	// refund unless an acquirer genuinely charges for the reversal.
	RefundAcquiringBps int // env: PAYMENTS_REFUND_ACQUIRING_BPS

	// AcquirerMinFeeMinor is the acquirer's per-operation FLOOR in tiyn, the
	// "минимум 25 ₸" half of FreedomPay's "3,5% мин 25 ₸" tariff. Without it a
	// small deposit leaves the venue short: the percentage markup would be less
	// than the fee the acquirer actually takes. env: PAYMENTS_ACQUIRER_MIN_FEE_MINOR
	AcquirerMinFeeMinor int64

	// RefundAcquiringBpsByProvider overrides RefundAcquiringBps per acquirer,
	// because the rate is a property of the acquirer, not of the platform: one
	// returns its fee on a reversal (0 bps), another keeps it (some non-zero
	// rate). A provider absent from this map falls back to RefundAcquiringBps.
	// env: PAYMENTS_REFUND_ACQUIRING_BPS_<PROVIDER>, e.g.
	// PAYMENTS_REFUND_ACQUIRING_BPS_FREEDOMPAY=0.
	RefundAcquiringBpsByProvider map[string]int

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

	// SplitEnabled turns SPLIT payments on: one guest charge divided at the
	// acquirer between the venue's own sub-merchant account and the platform's,
	// instead of landing whole on ours and being settled later by a payout.
	//
	// OFF by default and it must stay off until venues actually have
	// sub-merchant identifiers (restaurant_split_accounts, migration 0077):
	// with it on, a venue that has none cannot take a payment at all, which is
	// deliberate — the alternative is paying the venue's share to the platform.
	// env: PAYMENTS_SPLIT_ENABLED
	SplitEnabled bool

	// PlatformSplitAccountRef is the platform's own sub-merchant identifier at
	// the acquirer — where our commission share of a split payment is credited.
	// Required whenever SplitEnabled is on. It identifies, it does not
	// authorise, so unlike an acquirer key it is ordinary configuration; it is
	// still never written to a log.
	// env: PAYMENTS_PLATFORM_SPLIT_ACCOUNT
	PlatformSplitAccountRef string
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

// LegacySyncConfig configures cmd/worker's legacy-sync loop. DatabaseURL is a
// read-only Postgres connection string to the OLD system; empty disables the
// whole sync. The connection string is a credential — read from env only, never
// logged. The end-time each booking needs (the old system stores only a single
// time) is derived with Booking.DefaultDuration, so there is no separate knob
// for it here.
type LegacySyncConfig struct {
	DatabaseURL  string        // env: LEGACY_DB_URL
	TickInterval time.Duration // env: LEGACY_SYNC_TICK_INTERVAL
	BatchSize    int           // env: LEGACY_SYNC_BATCH_SIZE
}

// TicketsSweepConfig configures the pending-event-ticket sweep worker. The
// StaleAfter default (100h) deliberately exceeds the payments HoldTTL default
// (96h) so a ticket whose payment hold is still legitimately in flight is never
// swept — see usecase/tickets.PendingSweeper.
type TicketsSweepConfig struct {
	TickInterval time.Duration // env: TICKETS_SWEEP_TICK_INTERVAL
	StaleAfter   time.Duration // env: TICKETS_SWEEP_STALE_AFTER
	BatchSize    int           // env: TICKETS_SWEEP_BATCH_SIZE
}

// EventRecurrencesConfig configures the recurring-event generator
// (usecase/eventrecurrence.Generator). Every pass is safe-idle: with no active
// rules it reads one empty page, and with the window already materialised its
// insert writes nothing (ON CONFLICT DO NOTHING), so a short tick is cheap.
type EventRecurrencesConfig struct {
	TickInterval time.Duration // env: EVENT_RECURRENCE_TICK_INTERVAL
	// Window is how far ahead occurrences are created. 8 weeks by default: far
	// enough that the Афиша never looks empty, short enough that editing a rule
	// still reaches most of what guests will actually see.
	Window    time.Duration // env: EVENT_RECURRENCE_WINDOW
	BatchSize int           // env: EVENT_RECURRENCE_BATCH_SIZE
}

// PayoutsConfig configures the scheduled daily payout pass and the money policy
// of a payout: what moving it costs, who pays that cost, and how little is too
// little to be worth moving.
//
// The fee defaults are FreedomPay's tariff for a payout to a KZ bank card
// (merchant questionnaire, 14.07.2026): 1.9% with a minimum of 300 ₸ per
// payout. They are env-configurable because a tariff is a contract term, not a
// constant — but the DEFAULTS are the real numbers, so a deployment that
// configures nothing still models the true cost instead of pretending payouts
// are free.
type PayoutsConfig struct {
	// DailyTickInterval is how often the pass checks whether a venue's local
	// day has ended. NOT the payout cadence — that is one per venue per local
	// day, enforced by a UNIQUE index (migration 0052).
	DailyTickInterval time.Duration // env: PAYOUTS_DAILY_TICK_INTERVAL
	// DailySendEnabled additionally DISPATCHES what the pass generates. Off by
	// default: generating moves no money, sending does, and sending also
	// requires FREEDOMPAY_PAYOUT_ENABLED and a verified payout product.
	DailySendEnabled bool // env: PAYOUTS_DAILY_SEND_ENABLED
	// MinPayoutMinor is the roll-over threshold: below it a venue's money waits
	// for the next day instead of paying a 300 ₸ floor on a small amount.
	// PLATFORM DEFAULT only — a venue may override it
	// (restaurant_payout_settings.min_payout_minor, migration 0053).
	MinPayoutMinor int64 // env: PAYOUTS_MIN_AMOUNT_MINOR
	// MaxHoldDays forces a payout after this many days even when the venue
	// never reaches its threshold, so the platform never sits on a venue's
	// money indefinitely. NOTE: 0 here does NOT mean "never force" — an unset
	// or non-positive env value falls back to 7 days (see payouts.Config
	// withDefaults). "Never force" is expressed PER VENUE, as an explicit 0 in
	// that venue's payout settings.
	MaxHoldDays int // env: PAYOUTS_MAX_HOLD_DAYS
	// FeeBps / FeeMinimumMinor are the acquirer's payout tariff.
	FeeBps          int   // env: PAYOUTS_FEE_BPS
	FeeMinimumMinor int64 // env: PAYOUTS_FEE_MIN_MINOR
	// FeeBearer is WHO absorbs the fee: "platform" (default) or "venue".
	// OPEN OWNER DECISION — the default is the one that cannot surprise a venue
	// with a payout smaller than the statement it already read.
	FeeBearer string // env: PAYOUTS_FEE_BEARER
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
	// TelegramBotToken is the BookEat notifications bot token. Read from env
	// only and never logged (a bot credential). Absent → the telegram channel
	// no-ops cleanly, exactly like absent VAPID keys for web push.
	TelegramBotToken string // env: TELEGRAM_NOTIFY_BOT_TOKEN
	// TelegramWebhookSecret is the secret_token given to Telegram's setWebhook
	// and required back on every inbound update. EMPTY DISABLES the inbound
	// webhook entirely: the buttons under an alert are only as safe as this
	// header, so an unset secret must mean "no endpoint", never "no check".
	TelegramWebhookSecret string // env: TELEGRAM_NOTIFY_WEBHOOK_SECRET

	// GuestPushProvider selects the GUEST mobile-push provider. Empty (the
	// default) means no provider is configured and the guest channel is a clean
	// no-op — the dispatcher still drains, it just sends nothing. The only value
	// implemented today is "expo"; it is an explicit switch rather than a
	// derived flag so swapping in a direct FCM/APNs sender later is a config
	// change, not a code change at the call site.
	GuestPushProvider string // env: GUEST_PUSH_PROVIDER ("" | "expo")
	// ExpoAccessToken is Expo's OPTIONAL push-security token. A credential: env
	// only, never logged. Expo accepts unauthenticated sends unless push
	// security is enabled on the project, so an empty value is legitimate and
	// does NOT disable the channel — GuestPushProvider does.
	ExpoAccessToken string // env: EXPO_ACCESS_TOKEN
	// ExpoEndpoint overrides Expo's push URL (tests / a future relay).
	ExpoEndpoint string // env: EXPO_PUSH_ENDPOINT

	// --- venue WhatsApp booking alerts (Meta Cloud API) ---

	// WhatsAppNotifyEnabled is the KILL SWITCH. It is a separate variable from
	// the credentials on purpose: the credentials are shared with the login-code
	// channel, so "stop the venue alerts right now" must be possible without
	// taking WhatsApp logins down with them. Default true — a channel nobody
	// asked to silence is on.
	WhatsAppNotifyEnabled bool // env: WHATSAPP_NOTIFY_ENABLED
	// WhatsAppNotifyAccessToken / WhatsAppNotifyPhoneNumberID are the sending
	// credentials. Empty falls back to the OTP channel's OTP_WHATSAPP_* pair,
	// because today it is the SAME business number — the split exists so a
	// second number (or a rotated token) is a config change, not a deploy.
	// Credentials: env only, NEVER logged.
	WhatsAppNotifyAccessToken   string // env: WHATSAPP_NOTIFY_ACCESS_TOKEN
	WhatsAppNotifyPhoneNumberID string // env: WHATSAPP_NOTIFY_PHONE_NUMBER_ID
	// WhatsAppNotifyTemplateName / …Lang name the APPROVED template. Meta
	// requires a NEW template for any wording change, so being able to point at
	// the successor without a deploy is the point of these two.
	WhatsAppNotifyTemplateName string // env: WHATSAPP_NOTIFY_TEMPLATE_NAME
	WhatsAppNotifyTemplateLang string // env: WHATSAPP_NOTIFY_TEMPLATE_LANG
	// WhatsAppNotifyAPIVersion pins the Graph version; empty falls back to the
	// OTP channel's value, then to the adapter default.
	WhatsAppNotifyAPIVersion string // env: WHATSAPP_NOTIFY_API_VERSION
	// WhatsAppNotifyAPIURL overrides the Graph host (tests / staging).
	WhatsAppNotifyAPIURL string // env: WHATSAPP_NOTIFY_API_URL
	// WhatsAppNotifyTimeout caps ONE send. The dispatcher fans out to several
	// recipients per event, so an unbounded call would stall the whole tick.
	WhatsAppNotifyTimeout time.Duration // env: WHATSAPP_NOTIFY_TIMEOUT
}

// StaticMapConfig configures the restaurant map-preview proxy.
//
// Provider is an explicit switch, not a flag derived from "is some key set", so
// adding Google Static Maps later is a config change at the call site, not a
// guess about which credential wins — same shape as PushConfig.GuestPushProvider.
type StaticMapConfig struct {
	// Provider selects the map vendor. Empty (the default) means no provider:
	// the endpoint answers map_not_configured. The only value implemented today
	// is "2gis".
	Provider string // env: STATIC_MAP_PROVIDER ("" | "2gis")
	// TwoGISAPIKey is the 2GIS Platform Manager access key. A credential: env
	// only, NEVER logged and never echoed in a response. Empty while Provider
	// is "2gis" is a misconfiguration, reported once at startup — the endpoint
	// then behaves as unconfigured rather than failing every request loudly.
	TwoGISAPIKey string // env: STATIC_MAP_2GIS_API_KEY
	// TwoGISBaseURL overrides 2GIS's Static API entry point (tests, staging).
	TwoGISBaseURL string // env: STATIC_MAP_2GIS_BASE_URL
	// Timeout caps one provider render call. Kept well inside the HTTP server's
	// write timeout: a map preview must never be the reason a response is cut off.
	Timeout time.Duration // env: STATIC_MAP_TIMEOUT
	// CacheTTL is how long a rendered image is reused (and the HTTP max-age the
	// client is told). Restaurant coordinates change roughly never.
	CacheTTL time.Duration // env: STATIC_MAP_CACHE_TTL
	// CacheMaxBytes bounds the in-process image cache.
	CacheMaxBytes int64 // env: STATIC_MAP_CACHE_MAX_BYTES
}

// Configured reports whether a map provider is selected AND has its credential.
func (s StaticMapConfig) Configured() bool {
	switch strings.ToLower(strings.TrimSpace(s.Provider)) {
	case "2gis":
		return strings.TrimSpace(s.TwoGISAPIKey) != ""
	default:
		return false
	}
}

// GuestPushConfigured reports whether a guest mobile-push provider is selected.
// When false the guest channel is built disabled and no-ops cleanly.
func (p PushConfig) GuestPushConfigured() bool {
	return strings.TrimSpace(p.GuestPushProvider) != ""
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

	// Parsed before the struct literal because, unlike every other knob here, a
	// value that is SET BUT INVALID is refused rather than defaulted: this one
	// decides how much of a guest's money is kept on a refund (see
	// refundAcquiringByProvider).
	refundAcquiring, err := refundAcquiringByProvider()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		App: AppConfig{
			Name:                getEnv("APP_NAME", "backend-core"),
			Environment:         getEnv("APP_ENV", "development"),
			BootstrapAdminEmail: getEnv("BOOTSTRAP_ADMIN_EMAIL", ""),
			URL:                 getEnv("APP_URL", "0.0.0.0:8080"),
			LogLevel:            getEnv("APP_LOG_LEVEL", "info"),
			LogFormat:           getEnv("APP_LOG_FORMAT", "json"),
			CORSAllowedOrigins:  getEnvList("APP_CORS_ORIGINS", "*"),
			TrustedProxies:      getEnvList("APP_TRUSTED_PROXIES", ""),
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
			TestAccountPhone:    strings.TrimSpace(getEnv("AUTH_TEST_ACCOUNT_PHONE", "")),
			TestAccountCode:     strings.TrimSpace(getEnv("AUTH_TEST_ACCOUNT_CODE", "")),
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
			// OFF by default (owner decision, 29.07.2026): the venue answers
			// its own bookings, and the SLA worker above is what keeps a guest
			// from waiting on a venue that never looks at the panel.
			DefaultConfirmOnCreate: getEnvBool("BOOKING_DEFAULT_CONFIRM_ON_CREATE", false),
			TimezoneFallback:       getEnv("BOOKING_TIMEZONE_FALLBACK", "Asia/Almaty"),
			RateLimit:              getEnvInt("BOOKING_RATE_LIMIT", 10),
			RateWindow:             getEnvDuration("BOOKING_RATE_WINDOW", time.Hour),
			SlotStep:               getEnvMinutes("BOOKING_SLOT_STEP_MINUTES", 30),
		},
		Worker: WorkerConfig{
			TickInterval:          getEnvDuration("WORKER_TICK_INTERVAL", time.Minute),
			NoShowGrace:           getEnvDuration("WORKER_NO_SHOW_GRACE", 30*time.Minute),
			GuestRemindersEnabled: getEnvBool("WORKER_GUEST_REMINDERS_ENABLED", false),
			ReminderLead:          getEnvDuration("WORKER_GUEST_REMINDER_LEAD", time.Hour),
			BatchSize:             getEnvInt("WORKER_BATCH_SIZE", 100),
		},
		Payments: PaymentsConfig{
			Enabled:                      getEnvBool("PAYMENTS_ENABLED", false),
			DefaultProvider:              getEnv("PAYMENTS_DEFAULT_PROVIDER", "freedompay"),
			ServiceFeeBps:                getEnvInt("PAYMENTS_SERVICE_FEE_BPS", 350),
			RefundAcquiringBps:           getEnvInt("PAYMENTS_REFUND_ACQUIRING_BPS", 0),
			AcquirerMinFeeMinor:          getEnvInt64("PAYMENTS_ACQUIRER_MIN_FEE_MINOR", 2500),
			RefundAcquiringBpsByProvider: refundAcquiring,
			DepositDefaultMinor:          getEnvInt64("PAYMENTS_DEPOSIT_DEFAULT_MINOR", 0),
			DepositRequired:              getEnvBool("PAYMENTS_DEPOSIT_REQUIRED", false),
			PreorderPaymentRequired:      getEnvBool("PAYMENTS_PREORDER_PAYMENT_REQUIRED", false),
			HoldTTL:                      getEnvDuration("PAYMENTS_HOLD_TTL", 96*time.Hour),
			FreeCancelWindow:             getEnvMinutes("PAYMENTS_FREE_CANCEL_WINDOW_MINUTES", 120),
			PublicBaseURL:                strings.TrimRight(getEnv("PAYMENTS_PUBLIC_BASE_URL", ""), "/"),
			SplitEnabled:                 getEnvBool("PAYMENTS_SPLIT_ENABLED", false),
			PlatformSplitAccountRef:      strings.TrimSpace(getEnv("PAYMENTS_PLATFORM_SPLIT_ACCOUNT", "")),
		},
		PaymentsReconciler: PaymentsReconcilerConfig{
			TickInterval:     getEnvDuration("PAYMENTS_RECONCILE_TICK_INTERVAL", 2*time.Minute),
			StuckAfter:       getEnvDuration("PAYMENTS_RECONCILE_STUCK_AFTER", 10*time.Minute),
			LostWebhookAfter: getEnvDuration("PAYMENTS_RECONCILE_LOST_WEBHOOK_AFTER", time.Hour),
			BatchSize:        getEnvInt("PAYMENTS_RECONCILE_BATCH_SIZE", 50),
			MaxAttempts:      getEnvInt("PAYMENTS_RECONCILE_MAX_ATTEMPTS", 5),
			ProviderMinGap:   getEnvDuration("PAYMENTS_RECONCILE_PROVIDER_MIN_GAP", 200*time.Millisecond),
		},
		LegacySync: LegacySyncConfig{
			DatabaseURL:  getEnv("LEGACY_DB_URL", ""),
			TickInterval: getEnvDuration("LEGACY_SYNC_TICK_INTERVAL", time.Minute),
			BatchSize:    getEnvInt("LEGACY_SYNC_BATCH_SIZE", 500),
		},
		TicketsSweep: TicketsSweepConfig{
			TickInterval: getEnvDuration("TICKETS_SWEEP_TICK_INTERVAL", 5*time.Minute),
			StaleAfter:   getEnvDuration("TICKETS_SWEEP_STALE_AFTER", 100*time.Hour),
			BatchSize:    getEnvInt("TICKETS_SWEEP_BATCH_SIZE", 100),
		},
		EventRecurrences: EventRecurrencesConfig{
			TickInterval: getEnvDuration("EVENT_RECURRENCE_TICK_INTERVAL", 5*time.Minute),
			Window:       getEnvDuration("EVENT_RECURRENCE_WINDOW", 8*7*24*time.Hour),
			BatchSize:    getEnvInt("EVENT_RECURRENCE_BATCH_SIZE", 100),
		},
		Payouts: PayoutsConfig{
			DailyTickInterval: getEnvDuration("PAYOUTS_DAILY_TICK_INTERVAL", 15*time.Minute),
			DailySendEnabled:  getEnvBool("PAYOUTS_DAILY_SEND_ENABLED", false),
			// 10 000 ₸: the amount at which the 300 ₸ floor is 3% — the worst
			// case the owner already accepted by choosing daily batching.
			MinPayoutMinor: getEnvInt64("PAYOUTS_MIN_AMOUNT_MINOR", 1_000_000),
			// 7 days (owner decision, 25.07.2026): a venue that never reaches
			// the threshold is still paid weekly rather than indefinitely held.
			MaxHoldDays: getEnvInt("PAYOUTS_MAX_HOLD_DAYS", 7),
			// FreedomPay, KZ bank card, questionnaire of 14.07.2026.
			FeeBps:          getEnvInt("PAYOUTS_FEE_BPS", 190),
			FeeMinimumMinor: getEnvInt64("PAYOUTS_FEE_MIN_MINOR", 30_000),
			FeeBearer:       getEnv("PAYOUTS_FEE_BEARER", "platform"),
		},
		Push: PushConfig{
			VAPIDPublicKey:        getEnv("PUSH_VAPID_PUBLIC_KEY", ""),
			VAPIDPrivateKey:       getEnv("PUSH_VAPID_PRIVATE_KEY", ""),
			VAPIDSubject:          getEnv("PUSH_VAPID_SUBJECT", ""),
			TTL:                   getEnvDuration("PUSH_TTL", 24*time.Hour),
			DispatchTick:          getEnvDuration("NOTIFY_DISPATCH_TICK_INTERVAL", 15*time.Second),
			DispatchBatch:         getEnvInt("NOTIFY_DISPATCH_BATCH_SIZE", 100),
			TelegramBotToken:      getEnv("TELEGRAM_NOTIFY_BOT_TOKEN", ""),
			TelegramWebhookSecret: getEnv("TELEGRAM_NOTIFY_WEBHOOK_SECRET", ""),

			GuestPushProvider: getEnv("GUEST_PUSH_PROVIDER", ""),
			ExpoAccessToken:   getEnv("EXPO_ACCESS_TOKEN", ""),
			ExpoEndpoint:      getEnv("EXPO_PUSH_ENDPOINT", ""),

			WhatsAppNotifyEnabled: getEnvBool("WHATSAPP_NOTIFY_ENABLED", true),
			// Fall back to the login-code channel's credentials: same business
			// number today, and requiring the operator to paste the same token
			// under a second name is how a channel ends up silently unconfigured.
			WhatsAppNotifyAccessToken:   getEnv("WHATSAPP_NOTIFY_ACCESS_TOKEN", getEnv("OTP_WHATSAPP_ACCESS_TOKEN", "")),
			WhatsAppNotifyPhoneNumberID: getEnv("WHATSAPP_NOTIFY_PHONE_NUMBER_ID", getEnv("OTP_WHATSAPP_PHONE_NUMBER_ID", "")),
			// The APPROVED venue template (Meta id 1046784127953752, ru, UTILITY).
			// NOT the OTP template: that one is an AUTHENTICATION template with a
			// single parameter and would be rejected with four.
			WhatsAppNotifyTemplateName: getEnv("WHATSAPP_NOTIFY_TEMPLATE_NAME", whatsapp.DefaultBookingTemplateName),
			WhatsAppNotifyTemplateLang: getEnv("WHATSAPP_NOTIFY_TEMPLATE_LANG", whatsapp.DefaultBookingTemplateLang),
			WhatsAppNotifyAPIVersion:   getEnv("WHATSAPP_NOTIFY_API_VERSION", getEnv("OTP_WHATSAPP_API_VERSION", whatsapp.DefaultAPIVersion)),
			WhatsAppNotifyAPIURL:       getEnv("WHATSAPP_NOTIFY_API_URL", ""),
			WhatsAppNotifyTimeout:      getEnvDuration("WHATSAPP_NOTIFY_TIMEOUT", 10*time.Second),
		},
		StaticMap: StaticMapConfig{
			Provider:      getEnv("STATIC_MAP_PROVIDER", ""),
			TwoGISAPIKey:  getEnv("STATIC_MAP_2GIS_API_KEY", ""),
			TwoGISBaseURL: getEnv("STATIC_MAP_2GIS_BASE_URL", ""),
			Timeout:       getEnvDuration("STATIC_MAP_TIMEOUT", 5*time.Second),
			CacheTTL:      getEnvDuration("STATIC_MAP_CACHE_TTL", 24*time.Hour),
			CacheMaxBytes: getEnvInt64("STATIC_MAP_CACHE_MAX_BYTES", 64<<20),
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
		Analytics: AnalyticsConfig{
			APIKey:        getEnv("AMPLITUDE_API_KEY", ""),
			Endpoint:      getEnv("AMPLITUDE_ENDPOINT", ""),
			DispatchTick:  getEnvDuration("ANALYTICS_DISPATCH_TICK_INTERVAL", 30*time.Second),
			DispatchBatch: getEnvInt("ANALYTICS_DISPATCH_BATCH_SIZE", 100),
			HTTPTimeout:   getEnvDuration("ANALYTICS_HTTP_TIMEOUT", 10*time.Second),
		},

		OTPDelivery: OTPDeliveryConfig{
			// Default order: the pre-checkable channel first (a miss costs
			// nothing), the optimistic one second, the expensive certain one
			// last.
			ChannelOrder:   getEnvList("OTP_CHANNEL_ORDER", "telegram_gateway,whatsapp,sms"),
			SendTimeout:    getEnvDuration("OTP_SEND_TIMEOUT", 5*time.Second),
			DeliveryBudget: getEnvDuration("OTP_DELIVERY_BUDGET", 12*time.Second),

			TelegramGatewayToken:  getEnv("OTP_TELEGRAM_GATEWAY_TOKEN", ""),
			TelegramSenderUser:    getEnv("OTP_TELEGRAM_GATEWAY_SENDER_USERNAME", ""),
			TelegramGatewayAPIURL: getEnv("OTP_TELEGRAM_GATEWAY_API_URL", ""),

			WhatsAppAccessToken:   getEnv("OTP_WHATSAPP_ACCESS_TOKEN", ""),
			WhatsAppPhoneNumberID: getEnv("OTP_WHATSAPP_PHONE_NUMBER_ID", ""),
			// The template that was verified against the live number on
			// 2026-07-25, kept as the default so the owner only has to supply
			// the two credentials. The language code is "en" (not "en_US"):
			// confirmed by a real send that Meta accepted AND actually
			// delivered to the owner's phone, not by reading the dashboard.
			WhatsAppTemplateName: getEnv("OTP_WHATSAPP_TEMPLATE_NAME", "bookeat_otp_en"),
			WhatsAppTemplateLang: getEnv("OTP_WHATSAPP_TEMPLATE_LANG", "en"),
			// v22.0 is the version the live send was verified on.
			WhatsAppAPIVersion: getEnv("OTP_WHATSAPP_API_VERSION", "v22.0"),
			// Meta's authentication templates carry the "Copy code" button by
			// default, and the old system's working payload sends its parameter.
			WhatsAppCopyButton: getEnvBool("OTP_WHATSAPP_COPY_CODE_BUTTON", true),
			WhatsAppAPIURL:     getEnv("OTP_WHATSAPP_API_URL", ""),

			SMSProvider:        getEnv("OTP_SMS_PROVIDER", ""),
			TwilioAccountSID:   getEnv("OTP_SMS_TWILIO_ACCOUNT_SID", ""),
			TwilioAuthToken:    getEnv("OTP_SMS_TWILIO_AUTH_TOKEN", ""),
			TwilioFrom:         getEnv("OTP_SMS_TWILIO_FROM", ""),
			TwilioMessagingSID: getEnv("OTP_SMS_TWILIO_MESSAGING_SERVICE_SID", ""),
			TwilioAPIURL:       getEnv("OTP_SMS_TWILIO_API_URL", ""),

			MobizonAPIKey: getEnv("OTP_SMS_MOBIZON_API_KEY", ""),
			MobizonSender: getEnv("OTP_SMS_MOBIZON_SENDER", ""),
			MobizonAPIURL: getEnv("OTP_SMS_MOBIZON_API_URL", ""),
		},
	}

	// A half-configured review account is refused at startup rather than
	// silently ignored. Both empty is the normal, intended state (the feature
	// does not exist); one of the two set means somebody meant to enable it and
	// mistyped a variable name — and the failure mode of "silently off" is a
	// rejected App Store submission nobody can explain.
	if (cfg.Auth.TestAccountPhone == "") != (cfg.Auth.TestAccountCode == "") {
		return Config{}, fmt.Errorf(
			"AUTH_TEST_ACCOUNT_PHONE and AUTH_TEST_ACCOUNT_CODE must be set together (or both left empty)")
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

// knownPaymentProviders mirrors domain.PaymentProvider's constants. It lives
// here as plain strings so the config layer keeps its "env in, struct out"
// shape without importing the domain; a provider added to the domain and
// forgotten here simply has no per-provider env knob and falls back to the
// global rate, which is the safe direction.
var knownPaymentProviders = []string{"freedompay", "tiptoppay", "partnerspay"}

// maxBasisPoints is 100% — the domain's ApplyBasisPoints refuses anything
// above it, so a larger configured rate could only ever fail at settle time.
const maxBasisPoints = 10000

// refundAcquiringByProvider reads PAYMENTS_REFUND_ACQUIRING_BPS_<PROVIDER> for
// every known acquirer. Only variables that are actually SET land in the map —
// an unset provider must fall back to the global rate, so a missing key and an
// explicit 0 have to stay distinguishable.
//
// Unlike the getEnv* helpers, a value that is present but unusable is an ERROR,
// not a silent fallback. This knob decides how much of a guest's refund is kept:
// "2.9" typed instead of "290" would quietly hand the acquirer's cost to the
// platform, and 29000 would make every refund for that provider fail at settle
// time (ApplyBasisPoints caps at 100%). Both are ops mistakes that must surface
// at boot, where they are a non-event, rather than in the money path.
func refundAcquiringByProvider() (map[string]int, error) {
	var out map[string]int
	for _, p := range knownPaymentProviders {
		key := "PAYMENTS_REFUND_ACQUIRING_BPS_" + strings.ToUpper(p)
		v, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%s=%q: want whole basis points (290 = 2.9%%)", key, v)
		}
		if n < 0 || n > maxBasisPoints {
			return nil, fmt.Errorf("%s=%d: basis points must be within 0..%d", key, n, maxBasisPoints)
		}
		if out == nil {
			out = make(map[string]int, len(knownPaymentProviders))
		}
		out[p] = n
	}
	return out, nil
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
