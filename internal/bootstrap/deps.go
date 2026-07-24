package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/legacysource"
	"backend-core/internal/infrastructure/otpsender"
	paymentgw "backend-core/internal/infrastructure/payment"
	"backend-core/internal/infrastructure/payment/freedompay"
	"backend-core/internal/infrastructure/payment/tiptoppay"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	consentrepo "backend-core/internal/infrastructure/postgres/consent"
	contentdraftrepo "backend-core/internal/infrastructure/postgres/contentdraft"
	dashboardrepo "backend-core/internal/infrastructure/postgres/dashboard"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	favoriterepo "backend-core/internal/infrastructure/postgres/favorite"
	guestrepo "backend-core/internal/infrastructure/postgres/guest"
	idemrepo "backend-core/internal/infrastructure/postgres/idempotency"
	legacysink "backend-core/internal/infrastructure/postgres/legacysync"
	menurepo "backend-core/internal/infrastructure/postgres/menu"
	notificationrepo "backend-core/internal/infrastructure/postgres/notification"
	otprepo "backend-core/internal/infrastructure/postgres/otp"
	paymentrepo "backend-core/internal/infrastructure/postgres/payment"
	payoutrepo "backend-core/internal/infrastructure/postgres/payout"
	promorepo "backend-core/internal/infrastructure/postgres/promo"
	rtrepo "backend-core/internal/infrastructure/postgres/refreshtoken"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	reviewrepo "backend-core/internal/infrastructure/postgres/review"
	schedulerepo "backend-core/internal/infrastructure/postgres/schedule"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	credrepo "backend-core/internal/infrastructure/postgres/usercredential"
	usercuisinerepo "backend-core/internal/infrastructure/postgres/usercuisine"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/telegramnotify"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/webpush"
	"backend-core/internal/usecase/admin"
	"backend-core/internal/usecase/auth"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/consent"
	"backend-core/internal/usecase/content"
	"backend-core/internal/usecase/dashboard"
	"backend-core/internal/usecase/events"
	"backend-core/internal/usecase/favorites"
	"backend-core/internal/usecase/legacysync"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/notifications"
	"backend-core/internal/usecase/payments"
	"backend-core/internal/usecase/payouts"
	"backend-core/internal/usecase/promos"
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
	MyRestaurants      *restaurants.MyRestaurantsUseCase
	PushSubscriptions  *notifications.SubscriptionUseCase
	FavoritesFacade    favorites.Facade
	ConsentFacade      consent.Facade
	ReviewsFacade      reviews.Facade
	EventsFacade       events.Facade
	PromosFacade       promos.Facade
	ContentFacade      content.Facade
	MenuFacade         menu.Facade
	BookingsFacade     bookings.Facade
	BookingCreate      bookings.CreateUseCase
	BookingIdempotent  bookings.IdempotentCreateUseCase
	BookingStatus      bookings.StatusUseCase
	BookingUpdate      bookings.UpdateUseCase
	BookingAvail       bookings.AvailabilityUseCase
	BookingBlacklist   bookings.BlacklistUseCase
	BookingPolicy      bookings.PolicyUseCase
	AdminPanel         *admin.UseCase
	Dashboard          *dashboard.UseCase
	BookingExternal    bookings.ExternalReservationUseCase
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
	PaymentCreate        payments.CreateUseCase
	PaymentCapture       payments.CaptureUseCase
	PaymentVoid          payments.VoidUseCase
	PaymentRefund        payments.RefundUseCase
	PaymentWebhook       payments.WebhookUseCase
	PaymentStatus        payments.StatusUseCase
	PaymentDepositCancel payments.DepositCancellationUseCase

	// Payouts — restaurant settlement (выплаты заведениям). Destinations +
	// generation + send + statement, all behind usecase RBAC.
	Payouts *payouts.UseCase

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
	myRestaurants := restaurants.NewMyRestaurantsUseCase(restManagers, restRepo)
	pushSubscriptions := notifications.NewSubscriptionUseCase(notificationrepo.NewSubscriptions(db), restManagers)
	favoritesRepo := favoriterepo.New(db)
	favoritesFacade := favorites.NewFacade(favoritesRepo)
	consentFacade := consent.NewFacade(
		consentrepo.NewConsentRepository(db),
		consentrepo.NewPreferenceRepository(db),
	)

	bookingRepo := bookingrepo.New(db)
	reviewsFacade := reviews.NewFacade(reviewrepo.New(db), bookingRepo, restManagers)

	// Events & promos (Ф2): admin CRUD + public listings, both gated by the
	// shared RBAC matrix (PermRestaurantManage) via restaurantManagers. The
	// content-draft review queue reuses the same permission gate and creates
	// the real published Event/Promo on approval inside one transaction (txm).
	eventsFacade := events.NewFacade(eventrepo.New(db), restaurantManagers)
	promosFacade := promos.NewFacade(promorepo.New(db), restaurantManagers)
	contentFacade := content.NewFacade(
		contentdraftrepo.New(db), eventrepo.New(db), promorepo.New(db), restaurantManagers, txm)
	bookingLinks := bookingrepo.NewTables(db)
	bookingItems := bookingrepo.NewItems(db)
	bookingMessages := bookingrepo.NewMessages(db)
	bookingSurveys := bookingrepo.NewSurveys(db)
	bookingHistory := bookingrepo.NewHistory(db)
	bookingOutbox := bookingrepo.NewOutbox(db)
	bookingBlacklist := bookingrepo.NewBlacklist(db)
	bookingRateLog := bookingrepo.NewRateLog(db)
	bookingExternal := bookingrepo.NewExternalReservations(db)
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
	cancelDeadline := cancelDeadlineAdapter{settings: restRepo, cfg: paymentsCfg}

	// Shared schedule-override repo: the admin panel edits special-day overrides
	// (including the paid-booking flag), and the payments create path reads them
	// to decide whether a booking on that date needs a prepayment.
	scheduleRepo := schedulerepo.New(db)
	specialDays := specialDayAdapter{overrides: scheduleRepo, fallbackTZ: cfg.Booking.TimezoneFallback}

	paymentCreate := payments.NewCreateUseCase(paymentsRepo, paymentOutboxRepo, bookingRepo, bookingItems,
		paymentSettings, specialDays, paymentGateways, restaurantManagers, txm, paymentsCfg)
	paymentCapture := payments.NewCaptureUseCase(paymentsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, restaurantManagers, txm)
	paymentVoid := payments.NewVoidUseCase(paymentsRepo, paymentOutboxRepo, paymentGateways, restaurantManagers, txm)
	paymentRefund := payments.NewRefundUseCase(paymentsRepo, paymentRefundsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, restaurantManagers, bookingRepo, cancelDeadline, txm, paymentsCfg)
	paymentWebhook := payments.NewWebhookUseCase(paymentsRepo, paymentEventsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, txm)
	paymentStatus := payments.NewStatusUseCase(paymentsRepo, restaurantManagers)
	// Deposit hold settlement on booking cancel / no-show (void-early /
	// capture-late-or-noshow), reusing the same window resolver RefundUseCase
	// uses. Hooked into the booking cancel/no-show transitions in bootstrap.
	paymentDepositCancel := payments.NewDepositCancellationUseCase(paymentsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, restaurantManagers, bookingRepo, cancelDeadline, paymentRefund, txm)

	// Named facade/usecase variables so both the Deps struct and the admin
	// panel below share the SAME instances (rather than re-constructing them).
	restaurantsFacade := restaurants.NewFacade(restRepo, restRelated, restCategories, restPartners, txm)
	menuFacade := menu.NewFacade(menuItems, menuCategories, txm)
	bookingsFacade := bookings.NewFacade(bookingRepo, bookingLinks, bookingItems,
		bookingMessages, bookingSurveys, bookingHistory, bookingOutbox, restaurantManagers, txm,
		bookings.WithFreeCancelDeadlineResolver(cancelDeadline)) // same window as the money path
	bookingStatus := bookings.NewStatusUseCase(bookingRepo, bookingHistory, bookingOutbox,
		restRepo, restaurantManagers, txm, bookingCfg,
		bookings.WithDepositSettler(depositSettlerAdapter{uc: paymentDepositCancel}))

	// Restaurant admin panel (Ф1): an RBAC-guarded orchestration over the
	// existing building blocks. It reuses restaurantManagers for the RBAC
	// permission lookup, the restaurant/menu facades for profile+menu, the
	// booking facade/status usecase for the calendar+transitions (never an
	// ad-hoc status write), and dedicated schedule-override + guest read repos.
	adminPanel := admin.NewUseCase(
		restaurantManagers, restaurantsFacade, menuFacade, restRelated,
		scheduleRepo, guestrepo.New(db), bookingsFacade, bookingStatus,
		restRepo,                         // paymentSettingsWriter: edits free_cancel_window_minutes
		notificationrepo.NewSettings(db), // telegramSettings: connects/clears the venue's Telegram alert chat
	)

	// Superadmin platform dashboard (Ф1): read-only, platform-wide aggregates
	// for the global superadmin only. Pure reads over a dedicated read-model
	// repo (all aggregation in SQL); the usecase enforces the superadmin gate.
	dashboardUC := dashboard.NewUseCase(dashboardrepo.New(db))

	// Restaurant payouts (выплаты заведениям): what BookEat owes each venue
	// (computed from the payment ledger) and paying it out via the FreedomPay
	// payout gateway. The gateway is OFF by default (newPayoutGateway returns a
	// genuine nil unless FREEDOMPAY_PAYOUT_ENABLED=true) — payouts can be
	// generated (pending) but only SENT once the payout product is enabled and
	// verified. RBAC (owner/manager for destinations, superadmin for money out)
	// lives in the usecase.
	payoutUC := payouts.NewUseCase(payouts.Ports{
		Perms:        restaurantManagers,
		Destinations: payoutrepo.NewDestinations(db),
		Payouts:      payoutrepo.NewPayouts(db),
		Items:        payoutrepo.NewItems(db),
		Owed:         payoutrepo.NewOwed(db),
		Gateway:      newPayoutGateway(log),
		Tx:           txm,
	}, log)

	return &Deps{
		AuthFacade:         authFacade,
		AuthOTP:            authOTP,
		UsersFacade:        users.NewFacade(usersRepo, userCuisineRepo, refreshRepo, otpRepo, txm),
		UsersRepo:          usersRepo,
		RestaurantsFacade:  restaurantsFacade,
		RestaurantManagers: restaurantManagers,
		MyRestaurants:      myRestaurants,
		PushSubscriptions:  pushSubscriptions,
		FavoritesFacade:    favoritesFacade,
		ConsentFacade:      consentFacade,
		ReviewsFacade:      reviewsFacade,
		EventsFacade:       eventsFacade,
		PromosFacade:       promosFacade,
		ContentFacade:      contentFacade,
		MenuFacade:         menuFacade,
		BookingsFacade:     bookingsFacade,
		BookingCreate:      bookingCreate,
		BookingIdempotent:  bookings.NewIdempotentCreateUseCase(bookingCreate, idempotencyKeys, txm),
		BookingStatus:      bookingStatus,
		BookingUpdate: bookings.NewUpdateUseCase(bookingRepo, bookingLinks, bookingOutbox,
			restRepo, restRelated, restaurantManagers, txm, bookingCfg),
		BookingAvail:     bookings.NewAvailabilityUseCase(bookingLinks, restRepo, restRelated, bookingCfg),
		BookingBlacklist: bookings.NewBlacklistUseCase(bookingBlacklist, restaurantManagers),
		BookingPolicy:    bookings.NewPolicyUseCase(restRepo, restRepo, restaurantManagers, bookingCfg),
		AdminPanel:       adminPanel,
		Dashboard:        dashboardUC,
		BookingExternal: bookings.NewExternalReservationUseCase(bookingExternal, restRepo,
			restRelated, restaurantManagers, txm),
		Issuer: issuer,

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
		PaymentDepositCancel:  paymentDepositCancel,
		Payouts:               payoutUC,
		PaymentsPublicBaseURL: cfg.Payments.PublicBaseURL,
	}, nil
}

// newPayoutGateway builds the FreedomPay payout adapter, OFF by default. It
// returns a genuine nil interface (not a typed-nil) unless
// FREEDOMPAY_PAYOUT_ENABLED=true AND the FreedomPay credentials validate — so
// usecase/payouts.SendPayout's `gateway == nil` guard leaves payouts in
// `pending` (never stranded in `sent`) until payouts are deliberately turned
// on. Money-safety: going live requires FreedomPay to ENABLE the payout product
// for merchant 588079 and a sandbox verification of the pg_ contract (see the
// freedompay.PayoutGateway doc). Until then this stays nil in every deploy.
func newPayoutGateway(log *slog.Logger) domain.PayoutGateway {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("FREEDOMPAY_PAYOUT_ENABLED")), "true") {
		log.Info("freedompay payout gateway disabled (FREEDOMPAY_PAYOUT_ENABLED != true): payouts can be generated but not sent")
		return nil
	}
	fpCfg := freedompay.ConfigFromEnv()
	if err := fpCfg.Validate(); err != nil {
		log.Warn("freedompay payout gateway not configured, leaving payouts un-sendable", slog.String("reason", err.Error()))
		return nil
	}
	client := paymentgw.NewClient(nil, paymentgw.DefaultConfig(), log)
	gw, err := freedompay.NewPayoutGateway(fpCfg, client, log)
	if err != nil {
		log.Warn("building freedompay payout gateway failed, leaving payouts un-sendable", slog.String("reason", err.Error()))
		return nil
	}
	log.Warn("freedompay payout gateway ENABLED — payouts will move real money; ensure the payout product is verified on the sandbox")
	return gw
}

// NewPayoutReconciler wires the background payout reconciliation worker
// (usecase/payouts.Reconciler). Like NewPaymentsReconciler it is deliberately
// separate from NewDeps (no HTTP stack). It is safe-idle when the payout
// gateway is disabled (nil): Tick returns immediately.
func NewPayoutReconciler(cfg Config, db *pgxpool.Pool, log *slog.Logger) *payouts.Reconciler {
	return payouts.NewReconciler(
		payoutrepo.NewPayouts(db), payoutrepo.NewItems(db), newPayoutGateway(log), sqltx.NewManager(db),
		payouts.ReconcilerConfig{
			TickInterval: cfg.PaymentsReconciler.TickInterval,
			StuckAfter:   cfg.PaymentsReconciler.StuckAfter,
			BatchSize:    cfg.PaymentsReconciler.BatchSize,
			MaxAttempts:  cfg.PaymentsReconciler.MaxAttempts,
		}, log)
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
		FreeCancelWindow:        cfg.Payments.FreeCancelWindow,
	}
}

// paymentSettingsReader is the slice of the restaurant repo the money-path
// cancel-deadline adapter needs: one venue's payment-settings override,
// including free_cancel_window_minutes. Implemented by *restaurant.Repository
// (GetPaymentOverride).
type paymentSettingsReader interface {
	GetPaymentOverride(ctx context.Context, restaurantID uuid.UUID) (domain.PaymentSettingsOverride, error)
}

// cancelDeadlineAdapter implements usecase/payments' cancelDeadlineResolver
// port over the MONEY-path free-cancellation window
// (restaurants.free_cancel_window_minutes, migration 0034/0035), resolved
// through payments.FreeCancelDeadlineFor so BOTH settlement flows
// (RefundUseCase.Settle and DepositCancellationUseCase) read one window.
//
// NOTE: this reads free_cancel_window_minutes (owner-confirmed default 120m),
// which is now the SINGLE source of truth for the money decision on cancel/
// no-show. The older cancel_deadline_minutes column (migration 0004) no longer
// gates the guest self-cancel action — that hard gate was removed in the
// cancellation PR (a guest may cancel their own booking at any time; the window
// only decides free-vs-paid, never blocks). The column is kept but deprecated;
// nothing in the money path reads it.
type cancelDeadlineAdapter struct {
	settings paymentSettingsReader
	cfg      payments.Config
}

func (a cancelDeadlineAdapter) CancelDeadlineFor(ctx context.Context, booking domain.Booking) (time.Time, error) {
	o, err := a.settings.GetPaymentOverride(ctx, booking.RestaurantID)
	if err != nil {
		return time.Time{}, err
	}
	return payments.FreeCancelDeadlineFor(o, a.cfg, booking.StartsAt), nil
}

// depositSettlerAdapter binds usecase/payments.DepositCancellationUseCase to
// usecase/bookings.DepositSettler so a booking cancel / no-show transition
// settles the held deposit. It runs as a SYSTEM (admin) actor: the booking
// transition itself already authorized the caller (a guest may only cancel
// their own booking in time, a no-show is staff/worker-only), so re-checking
// staff/tenant permission on the settlement would be redundant — the money
// decision is a consequence of an already-authorized transition, not a new
// user-initiated action.
type depositSettlerAdapter struct {
	uc payments.DepositCancellationUseCase
}

func (a depositSettlerAdapter) SettleDepositOnCancel(ctx context.Context, bookingID uuid.UUID, trigger domain.RefundTrigger, cancelledAt *time.Time) error {
	_, err := a.uc.SettleDepositOnCancel(ctx, payments.Actor{Role: domain.RoleAdmin}, bookingID, payments.DepositCancelInput{
		Trigger: trigger, CancelledAt: cancelledAt,
	})
	return err
}

// specialDayAdapter implements usecase/payments' specialDayResolver over the
// schedule-override table (migration 0036). It maps a booking instant to the
// venue's local calendar date (in the venue timezone, falling back to the
// platform default) and reports whether that day is a PAID special day and the
// deposit it requires. A date with no override — the common case, bookings are
// FREE by default — resolves to (false, 0) via ErrNotFound.
type specialDayAdapter struct {
	overrides  domain.ScheduleOverrideRepository
	fallbackTZ string
}

func (a specialDayAdapter) PaidSpecialDayFor(ctx context.Context, restaurantID uuid.UUID, at time.Time) (bool, int64, error) {
	o, err := a.overrides.GetForBookingInstant(ctx, restaurantID, at, a.fallbackTZ)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, 0, nil // no override for that date → a normal, free day
		}
		return false, 0, err
	}
	if !o.BookingPaymentRequired || o.DepositAmountMinor == nil {
		return false, 0, nil // override exists but the day is not marked paid
	}
	return true, *o.DepositAmountMinor, nil
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
	bookingRepo := bookingrepo.New(db)
	restRepo := restrepo.New(db)
	txm := sqltx.NewManager(db)
	wcfg := bookings.WorkerConfig{
		TickInterval: cfg.Worker.TickInterval,
		NoShowGrace:  cfg.Worker.NoShowGrace,
		BatchSize:    cfg.Worker.BatchSize,
	}

	// The worker settles the held deposit of the bookings it closes (a no-show
	// forfeits it, an abandonment releases it). Building the acquirer registry
	// can fail on bad env; that must NOT stop the janitor — it just runs without
	// deposit settlement (the reconciliation worker / a manual action remains
	// the backstop), so the failure is logged and the worker starts anyway.
	gateways, gwErr := newPaymentGateways(cfg.Payments, paymentrepo.NewProviders(db), log)
	if gwErr != nil {
		log.Error("booking worker: deposit settlement disabled (payment gateway registry failed to build)",
			slog.String("error", gwErr.Error()))
		return bookings.NewWorker(
			bookingRepo, bookingrepo.NewHistory(db), bookingrepo.NewOutbox(db),
			restRepo, txm, newBookingConfig(cfg), wcfg, log)
	}
	restaurantManagers := restaurants.NewManagerUseCase(restrepo.NewManagers(db), userrepo.New(db), txm)
	cancelDeadline := cancelDeadlineAdapter{settings: restRepo, cfg: newPaymentsConfig(cfg)}
	paymentsRepo := paymentrepo.New(db)
	ledgerRepo := paymentrepo.NewLedger(db)
	outboxRepo := paymentrepo.NewOutbox(db)
	refundUC := payments.NewRefundUseCase(paymentsRepo, paymentrepo.NewRefunds(db), ledgerRepo, outboxRepo,
		gateways, restaurantManagers, bookingRepo, cancelDeadline, txm, newPaymentsConfig(cfg))
	depositCancel := payments.NewDepositCancellationUseCase(
		paymentsRepo, ledgerRepo, outboxRepo,
		gateways, restaurantManagers, bookingRepo, cancelDeadline, refundUC, txm)

	return bookings.NewWorker(
		bookingRepo, bookingrepo.NewHistory(db), bookingrepo.NewOutbox(db),
		restRepo, txm, newBookingConfig(cfg), wcfg, log,
		bookings.WithWorkerDepositSettler(depositSettlerAdapter{uc: depositCancel}))
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

// NewNotificationDispatcher wires the background notification dispatcher: it
// drains the booking transactional outbox and fans "booking.created" out to the
// registered channel notifiers. Increment 1 registers exactly one channel, web
// push. Deliberately separate from NewDeps (no HTTP stack) for the same reason
// NewBookingWorker is.
//
// When the VAPID keys are absent the web-push notifier is built DISABLED: the
// dispatcher still runs and drains the outbox, it just sends nothing until the
// owner provisions PUSH_VAPID_PUBLIC_KEY / PUSH_VAPID_PRIVATE_KEY — the worker
// never crashes for lack of keys.
func NewNotificationDispatcher(cfg Config, db *pgxpool.Pool, log *slog.Logger) *notifications.Dispatcher {
	txm := sqltx.NewManager(db)

	pushCfg := webpush.Config{
		PublicKey:  cfg.Push.VAPIDPublicKey,
		PrivateKey: cfg.Push.VAPIDPrivateKey,
		Subject:    cfg.Push.VAPIDSubject,
		TTL:        cfg.Push.TTL,
	}
	var sender notifications.PushSender
	if pushCfg.Configured() {
		sender = webpush.NewSender(pushCfg).Send
	} else {
		log.Warn("web push not configured (no VAPID keys) — the channel will no-op until PUSH_VAPID_* is set")
	}

	webPush := notifications.NewWebPushNotifier(
		notificationrepo.NewSubscriptions(db),
		notificationrepo.NewDeliveries(db),
		notificationrepo.NewSettings(db),
		sender,
		pushCfg.Configured(),
		log,
	)

	// Telegram channel: a second notifier on the SAME dispatcher, dedupe ledger
	// and per-restaurant toggle. Absent TELEGRAM_NOTIFY_BOT_TOKEN → built
	// disabled and no-ops (like web push without VAPID keys).
	tgCfg := telegramnotify.Config{BotToken: cfg.Push.TelegramBotToken}
	var tgSender notifications.TelegramSender
	if tgCfg.Configured() {
		tgSender = telegramnotify.NewSender(tgCfg).Send
	} else {
		log.Warn("telegram notifications not configured (no bot token) — the channel will no-op until TELEGRAM_NOTIFY_BOT_TOKEN is set")
	}
	telegram := notifications.NewTelegramNotifier(
		notificationrepo.NewSettings(db),
		notificationrepo.NewDeliveries(db),
		tgSender,
		tgCfg.Configured(),
		log,
	)

	return notifications.NewDispatcher(
		bookingrepo.NewOutbox(db), txm,
		notifications.DispatcherConfig{
			TickInterval: cfg.Push.DispatchTick,
			BatchSize:    cfg.Push.DispatchBatch,
		}, log, webPush, telegram)
}

// NewLegacySyncWorker wires cmd/worker's one-way legacy sync. It opens a
// SEPARATE, read-only pool to the OLD database (LEGACY_DB_URL) and upserts into
// the new-DB pool `db`. When LEGACY_DB_URL is empty the sync is disabled: this
// returns (nil, nil, nil) and RunWorker simply never starts the loop — a clean
// no-op, the same discipline as the other optional workers.
//
// The returned closer owns the legacy pool; RunWorker calls it on shutdown. The
// connection string is a credential and is never logged.
func NewLegacySyncWorker(cfg Config, db *pgxpool.Pool, log *slog.Logger) (*legacysync.Worker, func(), error) {
	if cfg.LegacySync.DatabaseURL == "" {
		log.Info("legacy sync disabled (LEGACY_DB_URL unset)")
		return nil, nil, nil
	}
	pool, err := legacysource.OpenReadOnlyPool(context.Background(), cfg.LegacySync.DatabaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("open legacy source: %w", err)
	}
	worker := legacysync.NewWorker(
		legacysource.NewSource(pool),
		legacysink.NewSink(db),
		legacysync.Config{
			TickInterval:    cfg.LegacySync.TickInterval,
			BatchSize:       cfg.LegacySync.BatchSize,
			DefaultDuration: cfg.Booking.DefaultDuration,
		},
		log,
	)
	return worker, pool.Close, nil
}
