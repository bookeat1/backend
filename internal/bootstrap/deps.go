package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/otpsender"
	paymentgw "backend-core/internal/infrastructure/payment"
	"backend-core/internal/infrastructure/payment/freedompay"
	"backend-core/internal/infrastructure/payment/tiptoppay"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	favoriterepo "backend-core/internal/infrastructure/postgres/favorite"
	idemrepo "backend-core/internal/infrastructure/postgres/idempotency"
	menurepo "backend-core/internal/infrastructure/postgres/menu"
	otprepo "backend-core/internal/infrastructure/postgres/otp"
	paymentrepo "backend-core/internal/infrastructure/postgres/payment"
	rtrepo "backend-core/internal/infrastructure/postgres/refreshtoken"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	reviewrepo "backend-core/internal/infrastructure/postgres/review"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	credrepo "backend-core/internal/infrastructure/postgres/usercredential"
	usercuisinerepo "backend-core/internal/infrastructure/postgres/usercuisine"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/usecase/auth"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/favorites"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/payments"
	"backend-core/internal/usecase/restaurants"
	"backend-core/internal/usecase/reviews"
	"backend-core/internal/usecase/users"
)

// Deps holds the constructed usecases and shared infrastructure.
type Deps struct {
	AuthFacade         auth.Facade
	AuthOTP            auth.OTPUseCase
	UsersFacade        users.Facade
	UsersRepo          domain.UserRepository
	RestaurantsFacade  restaurants.Facade
	RestaurantManagers restaurants.ManagerUseCase
	FavoritesFacade    favorites.Facade
	ReviewsFacade      reviews.Facade
	MenuFacade         menu.Facade
	BookingsFacade     bookings.Facade
	BookingCreate      bookings.CreateUseCase
	BookingIdempotent  bookings.IdempotentCreateUseCase
	BookingStatus      bookings.StatusUseCase
	BookingUpdate      bookings.UpdateUseCase
	BookingAvail       bookings.AvailabilityUseCase
	BookingBlacklist   bookings.BlacklistUseCase
	BookingPolicy      bookings.PolicyUseCase
	Issuer             *token.RSAIssuer

	// Payments repositories, exposed for anything that still wants direct
	// access (the reconciler in cmd/worker, ad-hoc tooling).
	PaymentsRepo         domain.PaymentRepository
	PaymentRefundsRepo   domain.PaymentRefundRepository
	PaymentEventsRepo    domain.PaymentEventRepository
	PaymentLedgerRepo    domain.PaymentLedgerRepository
	PaymentOutboxRepo    domain.PaymentOutboxRepository
	PaymentProvidersRepo domain.PaymentProviderRepository
	PaymentGateways      *paymentgw.Registry

	// Payments usecases — the guest/staff-facing HTTP surface (transport/rest/payments).
	PaymentCreate  payments.CreateUseCase
	PaymentCapture payments.CaptureUseCase
	PaymentVoid    payments.VoidUseCase
	PaymentRefund  payments.RefundUseCase
	PaymentWebhook payments.WebhookUseCase
	PaymentStatus  payments.StatusUseCase
	// PaymentsPublicBaseURL is threaded straight through from cfg.Payments so
	// the transport layer can build the FreedomPay CallbackURL without
	// importing bootstrap.Config.
	PaymentsPublicBaseURL string
}

// NewDeps constructs repositories, infrastructure clients, and usecases.
func NewDeps(cfg Config, db *pgxpool.Pool, log *slog.Logger) (*Deps, error) {
	issuer, err := token.NewRSAIssuer(cfg.Auth.JWTPrivateKeyPEM, cfg.Auth.JWTKeyID, cfg.Auth.AccessTokenTTL)
	if err != nil {
		return nil, fmt.Errorf("build token issuer: %w", err)
	}
	txm := sqltx.NewManager(db)

	usersRepo := userrepo.New(db)
	credsRepo := credrepo.New(db)
	refreshRepo := rtrepo.New(db)
	otpRepo := otprepo.New(db)
	userCuisineRepo := usercuisinerepo.New(db)

	authCfg := auth.Config{
		RefreshTTL:   cfg.Auth.RefreshTokenTTL,
		OTPTTL:       cfg.Auth.OTPCodeTTL,
		OTPPerMin:    cfg.Auth.OTPRateLimitPerMin,
		OTPPerHour:   cfg.Auth.OTPRateLimitPerHour,
		OTPDevExpose: cfg.Auth.OTPDevExpose,
	}
	authFacade := auth.NewFacade(usersRepo, credsRepo, refreshRepo, txm, issuer, authCfg)
	authOTP := auth.NewOTPUseCase(usersRepo, otpRepo, refreshRepo, txm, issuer, otpsender.NewStub(log, cfg.App.Environment), authCfg)

	restRepo := restrepo.New(db)
	restRelated := restrepo.NewRelated(db)
	restCategories := restrepo.NewCategories(db)
	restManagers := restrepo.NewManagers(db)
	restPartners := restrepo.NewPartnership(db)

	menuItems := menurepo.New(db)
	menuCategories := menurepo.NewCategories(db)

	restaurantManagers := restaurants.NewManagerUseCase(restManagers, usersRepo, txm)
	favoritesRepo := favoriterepo.New(db)
	favoritesFacade := favorites.NewFacade(favoritesRepo)

	bookingRepo := bookingrepo.New(db)
	reviewsFacade := reviews.NewFacade(reviewrepo.New(db), bookingRepo, restManagers)
	bookingLinks := bookingrepo.NewTables(db)
	bookingItems := bookingrepo.NewItems(db)
	bookingMessages := bookingrepo.NewMessages(db)
	bookingSurveys := bookingrepo.NewSurveys(db)
	bookingHistory := bookingrepo.NewHistory(db)
	bookingOutbox := bookingrepo.NewOutbox(db)
	bookingBlacklist := bookingrepo.NewBlacklist(db)
	bookingRateLog := bookingrepo.NewRateLog(db)
	idempotencyKeys := idemrepo.New(db)

	bookingCfg := newBookingConfig(cfg)

	bookingCreate := bookings.NewCreateUseCase(bookingRepo, bookingLinks, bookingItems,
		bookingHistory, bookingOutbox, bookingBlacklist, bookingRateLog, restRepo,
		restRelated, restaurantManagers, txm, bookingCfg)

	paymentsRepo := paymentrepo.New(db)
	paymentRefundsRepo := paymentrepo.NewRefunds(db)
	paymentEventsRepo := paymentrepo.NewEvents(db)
	paymentLedgerRepo := paymentrepo.NewLedger(db)
	paymentOutboxRepo := paymentrepo.NewOutbox(db)
	paymentProvidersRepo := paymentrepo.NewProviders(db)
	paymentGateways, err := newPaymentGateways(cfg.Payments, paymentProvidersRepo, log)
	if err != nil {
		return nil, fmt.Errorf("build payment gateway registry: %w", err)
	}
	paymentsCfg := newPaymentsConfig(cfg)
	paymentSettings := restRepo // *restaurant.Repository now also implements restaurantPaymentSettings (GetPaymentOverride)
	cancelDeadline := cancelDeadlineAdapter{restaurants: restRepo, cfg: bookingCfg}

	paymentCreate := payments.NewCreateUseCase(paymentsRepo, paymentOutboxRepo, bookingRepo, bookingItems,
		paymentSettings, paymentGateways, restaurantManagers, txm, paymentsCfg)
	paymentCapture := payments.NewCaptureUseCase(paymentsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, restaurantManagers, txm)
	paymentVoid := payments.NewVoidUseCase(paymentsRepo, paymentOutboxRepo, paymentGateways, restaurantManagers, txm)
	paymentRefund := payments.NewRefundUseCase(paymentsRepo, paymentRefundsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, restaurantManagers, bookingRepo, cancelDeadline, txm, paymentsCfg)
	paymentWebhook := payments.NewWebhookUseCase(paymentsRepo, paymentEventsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, txm)
	paymentStatus := payments.NewStatusUseCase(paymentsRepo, restaurantManagers)

	return &Deps{
		AuthFacade:         authFacade,
		AuthOTP:            authOTP,
		UsersFacade:        users.NewFacade(usersRepo, userCuisineRepo, refreshRepo, otpRepo, txm),
		UsersRepo:          usersRepo,
		RestaurantsFacade:  restaurants.NewFacade(restRepo, restRelated, restCategories, restPartners, txm),
		RestaurantManagers: restaurantManagers,
		FavoritesFacade:    favoritesFacade,
		ReviewsFacade:      reviewsFacade,
		MenuFacade:         menu.NewFacade(menuItems, menuCategories, txm),
		BookingsFacade: bookings.NewFacade(bookingRepo, bookingLinks, bookingItems,
			bookingMessages, bookingSurveys, bookingHistory, bookingOutbox, restaurantManagers, txm),
		BookingCreate:     bookingCreate,
		BookingIdempotent: bookings.NewIdempotentCreateUseCase(bookingCreate, idempotencyKeys, txm),
		BookingStatus: bookings.NewStatusUseCase(bookingRepo, bookingHistory, bookingOutbox,
			restRepo, restaurantManagers, txm, bookingCfg),
		BookingUpdate: bookings.NewUpdateUseCase(bookingRepo, bookingLinks, bookingOutbox,
			restRepo, restRelated, restaurantManagers, txm, bookingCfg),
		BookingAvail:     bookings.NewAvailabilityUseCase(bookingLinks, restRepo, restRelated, bookingCfg),
		BookingBlacklist: bookings.NewBlacklistUseCase(bookingBlacklist, restaurantManagers),
		BookingPolicy:    bookings.NewPolicyUseCase(restRepo, restRepo, restaurantManagers, bookingCfg),
		Issuer:           issuer,

		PaymentsRepo:         paymentsRepo,
		PaymentRefundsRepo:   paymentRefundsRepo,
		PaymentEventsRepo:    paymentEventsRepo,
		PaymentLedgerRepo:    paymentLedgerRepo,
		PaymentOutboxRepo:    paymentOutboxRepo,
		PaymentProvidersRepo: paymentProvidersRepo,
		PaymentGateways:      paymentGateways,

		PaymentCreate:         paymentCreate,
		PaymentCapture:        paymentCapture,
		PaymentVoid:           paymentVoid,
		PaymentRefund:         paymentRefund,
		PaymentWebhook:        paymentWebhook,
		PaymentStatus:         paymentStatus,
		PaymentsPublicBaseURL: cfg.Payments.PublicBaseURL,
	}, nil
}

// newPaymentsConfig mirrors PaymentsConfig field-for-field into the usecase
// layer's own Config, same arrangement as newBookingConfig.
func newPaymentsConfig(cfg Config) payments.Config {
	return payments.Config{
		Enabled:                 cfg.Payments.Enabled,
		DefaultProvider:         domain.PaymentProvider(cfg.Payments.DefaultProvider),
		ServiceFeeBps:           cfg.Payments.ServiceFeeBps,
		RefundAcquiringBps:      cfg.Payments.RefundAcquiringBps,
		DepositDefaultMinor:     cfg.Payments.DepositDefaultMinor,
		DepositRequired:         cfg.Payments.DepositRequired,
		PreorderPaymentRequired: cfg.Payments.PreorderPaymentRequired,
		HoldTTL:                 cfg.Payments.HoldTTL,
	}
}

// cancelDeadlineAdapter implements usecase/payments' cancelDeadlineResolver
// port over the same restaurant-policy resolution usecase/bookings already
// owns (bookings.CancelDeadlineFor) — see the KNOWN GAP note this closes in
// team-memory's payments-usecase entry: the deadline must come from the
// venue's real, server-resolved policy, never be recomputed or trusted from a
// caller.
type cancelDeadlineAdapter struct {
	restaurants domain.RestaurantRepository
	cfg         bookings.Config
}

func (a cancelDeadlineAdapter) CancelDeadlineFor(ctx context.Context, booking domain.Booking) (time.Time, error) {
	rest, err := a.restaurants.GetByID(ctx, booking.RestaurantID)
	if err != nil {
		return time.Time{}, err
	}
	return bookings.CancelDeadlineFor(rest.Restaurant, a.cfg, booking.StartsAt), nil
}

// newBookingConfig mirrors BookingConfig field-for-field into the usecase
// layer's own Config so that layer never imports bootstrap (same arrangement as
// auth.Config).
func newBookingConfig(cfg Config) bookings.Config {
	return bookings.Config{
		DefaultDuration:       cfg.Booking.DefaultDuration,
		DefaultBuffer:         cfg.Booking.DefaultBuffer,
		DefaultLead:           cfg.Booking.DefaultLead,
		DefaultHorizonDays:    cfg.Booking.DefaultHorizonDays,
		DefaultCancelDeadline: cfg.Booking.DefaultCancelDeadline,
		DefaultConfirmSLA:     cfg.Booking.DefaultConfirmSLA,
		DefaultMaxGuests:      cfg.Booking.DefaultMaxGuests,
		DefaultAutoConfirm:    cfg.Booking.DefaultAutoConfirm,
		TimezoneFallback:      cfg.Booking.TimezoneFallback,
		RateWindow:            cfg.Booking.RateWindow,
		RateLimit:             cfg.Booking.RateLimit,
		SlotStep:              cfg.Booking.SlotStep,
	}
}

// NewBookingWorker wires the background booking worker. It is deliberately
// separate from NewDeps: the worker needs neither the HTTP stack nor a signing
// key, and requiring AUTH_JWT_PRIVATE_KEY to start a janitor process would be
// an operational trap.
func NewBookingWorker(cfg Config, db *pgxpool.Pool, log *slog.Logger) *bookings.Worker {
	return bookings.NewWorker(
		bookingrepo.New(db), bookingrepo.NewHistory(db), bookingrepo.NewOutbox(db),
		restrepo.New(db), sqltx.NewManager(db), newBookingConfig(cfg),
		bookings.WorkerConfig{
			TickInterval: cfg.Worker.TickInterval,
			NoShowGrace:  cfg.Worker.NoShowGrace,
			BatchSize:    cfg.Worker.BatchSize,
		}, log)
}

// newPaymentGateways builds the acquirer registry from whatever adapters this
// process actually has valid credentials for (spec §8: credentials come from
// env only). A provider with missing/incomplete env vars is logged and
// skipped, never a fatal error — this is the exact property that lets the app
// start with PAYMENTS_ENABLED=false and no acquirer keys configured at all:
// the registry ends up with zero gateways, and every call through it
// (Resolve / ForRefund) reports payment.ErrProviderNotConfigured /
// ErrNoEnabledProvider, an ordinary and already-handled outcome, never a
// panic or a boot failure.
func newPaymentGateways(cfg PaymentsConfig, providers domain.PaymentProviderRepository, log *slog.Logger) (*paymentgw.Registry, error) {
	client := paymentgw.NewClient(nil, paymentgw.DefaultConfig(), log)
	var gateways []domain.PaymentGateway

	fpCfg := freedompay.ConfigFromEnv()
	if err := fpCfg.Validate(); err != nil {
		log.Warn("freedompay adapter not configured, skipping", slog.String("reason", err.Error()))
	} else {
		gw, err := freedompay.New(fpCfg, client, log)
		if err != nil {
			return nil, fmt.Errorf("build freedompay gateway: %w", err)
		}
		gateways = append(gateways, gw)
	}

	ttpCfg := tiptoppay.ConfigFromEnv()
	if err := ttpCfg.Validate(); err != nil {
		log.Warn("tiptoppay adapter not configured, skipping", slog.String("reason", err.Error()))
	} else {
		gw, err := tiptoppay.New(ttpCfg, client, log)
		if err != nil {
			return nil, fmt.Errorf("build tiptoppay gateway: %w", err)
		}
		gateways = append(gateways, gw)
	}

	fallback := domain.PaymentProvider(cfg.DefaultProvider)
	registry, err := paymentgw.NewRegistry(providers, fallback, gateways...)
	if err != nil {
		return nil, fmt.Errorf("build payment registry: %w", err)
	}
	return registry, nil
}

// NewPaymentsReconciler wires the background payments reconciliation worker
// (usecase/payments.Reconciler). Deliberately separate from NewDeps for the
// same reason NewBookingWorker is: no HTTP stack, no signing key required to
// start a janitor process.
//
// It only needs the five Postgres repositories and the gateway registry —
// unlike CreateUseCase/CaptureUseCase/RefundUseCase, the reconciler never
// needs restaurantPaymentSettings or cancelDeadlineResolver (see the KNOWN
// GAP note on Deps above), so this can run correctly even before those two
// adapters exist.
func NewPaymentsReconciler(cfg Config, db *pgxpool.Pool, log *slog.Logger) (*payments.Reconciler, error) {
	providersRepo := paymentrepo.NewProviders(db)
	gateways, err := newPaymentGateways(cfg.Payments, providersRepo, log)
	if err != nil {
		return nil, err
	}
	return payments.NewReconciler(
		paymentrepo.New(db), paymentrepo.NewRefunds(db), paymentrepo.NewLedger(db),
		paymentrepo.NewOutbox(db), gateways, sqltx.NewManager(db),
		payments.ReconcilerConfig{
			TickInterval:     cfg.PaymentsReconciler.TickInterval,
			StuckAfter:       cfg.PaymentsReconciler.StuckAfter,
			LostWebhookAfter: cfg.PaymentsReconciler.LostWebhookAfter,
			BatchSize:        cfg.PaymentsReconciler.BatchSize,
			MaxAttempts:      cfg.PaymentsReconciler.MaxAttempts,
			ProviderMinGap:   cfg.PaymentsReconciler.ProviderMinGap,
		}, log), nil
}
