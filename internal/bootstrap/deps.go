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
	"backend-core/internal/infrastructure/amplitude"
	"backend-core/internal/infrastructure/expopush"
	"backend-core/internal/infrastructure/legacysource"
	"backend-core/internal/infrastructure/mediastore"
	"backend-core/internal/infrastructure/otpsender"
	paymentgw "backend-core/internal/infrastructure/payment"
	"backend-core/internal/infrastructure/payment/freedompay"
	"backend-core/internal/infrastructure/payment/tiptoppay"
	analyticsrepo "backend-core/internal/infrastructure/postgres/analytics"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	consentrepo "backend-core/internal/infrastructure/postgres/consent"
	contentdraftrepo "backend-core/internal/infrastructure/postgres/contentdraft"
	dashboardrepo "backend-core/internal/infrastructure/postgres/dashboard"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	eventticketrepo "backend-core/internal/infrastructure/postgres/eventticket"
	favoriterepo "backend-core/internal/infrastructure/postgres/favorite"
	feedrepo "backend-core/internal/infrastructure/postgres/feed"
	gastroguiderepo "backend-core/internal/infrastructure/postgres/gastroguide"
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
	storyrepo "backend-core/internal/infrastructure/postgres/story"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	credrepo "backend-core/internal/infrastructure/postgres/usercredential"
	usercuisinerepo "backend-core/internal/infrastructure/postgres/usercuisine"
	userrolerepo "backend-core/internal/infrastructure/postgres/userrole"
	venuedashboardrepo "backend-core/internal/infrastructure/postgres/venuedashboard"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/staticmap/twogis"
	"backend-core/internal/infrastructure/telegramnotify"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/webpush"
	"backend-core/internal/transport/rest/telegramhook"
	"backend-core/internal/usecase/admin"
	"backend-core/internal/usecase/analytics"
	"backend-core/internal/usecase/auth"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/consent"
	"backend-core/internal/usecase/content"
	"backend-core/internal/usecase/dashboard"
	"backend-core/internal/usecase/events"
	"backend-core/internal/usecase/favorites"
	"backend-core/internal/usecase/feed"
	"backend-core/internal/usecase/gastroguide"
	"backend-core/internal/usecase/legacysync"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/notifications"
	"backend-core/internal/usecase/payments"
	"backend-core/internal/usecase/payouts"
	"backend-core/internal/usecase/preorder"
	"backend-core/internal/usecase/promos"
	"backend-core/internal/usecase/restaurants"
	"backend-core/internal/usecase/reviews"
	rolesuc "backend-core/internal/usecase/roles"
	"backend-core/internal/usecase/staticmap"
	"backend-core/internal/usecase/stories"
	"backend-core/internal/usecase/tickets"
	"backend-core/internal/usecase/users"
	venuedashboarduc "backend-core/internal/usecase/venuedashboard"
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
	DeviceTokens       *notifications.DeviceTokenUseCase
	NotificationFeed   *notifications.NotificationFeedUseCase
	FavoritesFacade    favorites.Facade
	ConsentFacade      consent.Facade
	ReviewsFacade      reviews.Facade
	EventsFacade       events.Facade
	PromosFacade       promos.Facade
	GastroguideFacade  gastroguide.Facade
	GastroguideEditor  gastroguide.Editor
	ContentFacade      content.Facade
	FeedFacade         feed.Facade
	MenuFacade         menu.Facade
	StoriesFacade      stories.Facade
	BookingsFacade     bookings.Facade
	BookingCreate      bookings.CreateUseCase
	BookingIdempotent  bookings.IdempotentCreateUseCase
	BookingStatus      bookings.StatusUseCase
	// Roles changes global roles and reads their audit. Also the home of the
	// bootstrap that promotes the first administrator on an empty platform.
	Roles *rolesuc.UseCase
	// VenueDashboard serves one restaurant its own numbers (as opposed to the
	// platform-wide dashboard, which is the owner's view).
	VenueDashboard *venuedashboarduc.UseCase
	// VenueToday serves the operational top of the same panel: the requests
	// still waiting for an answer and the venue's current local day.
	VenueToday *venuedashboarduc.TodayUseCase
	// NotificationSettings backs the inbound Telegram webhook: it resolves the
	// chat a button press came from back to the venue that connected it.
	NotificationSettings domain.RestaurantNotificationSettingsRepository
	// TelegramAnswerer acknowledges a press and rewrites the alert. Nil when the
	// bot token is unset, which is also when the webhook stays unmounted.
	TelegramAnswerer telegramhook.Answerer
	// TelegramWebhookSecret gates the inbound webhook; empty leaves it unmounted.
	TelegramWebhookSecret string
	BookingUpdate         bookings.UpdateUseCase
	BookingAvail          bookings.AvailabilityUseCase
	BookingBlacklist      bookings.BlacklistUseCase
	BookingPolicy         bookings.PolicyUseCase
	AdminPanel            *admin.UseCase
	Dashboard             *dashboard.UseCase
	BookingExternal       bookings.ExternalReservationUseCase
	// BookingOverrides is the read side of the deliberate-overbooking audit
	// (migration 0056): the venue cabinet's answer to "who seated a party we
	// could not fit, and when".
	BookingOverrides bookings.CapacityOverrideUseCase
	Preorder         *preorder.UseCase
	StaticMap        *staticmap.UseCase
	Issuer           *token.RSAIssuer

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

	// Event ticketing (roadmap #2) — guest purchase + refund, admin sold view,
	// own-tickets. Money flows through the payments contour above.
	TicketPurchase tickets.PurchaseUseCase
	TicketRefund   tickets.RefundUseCase
	TicketAdmin    tickets.AdminUseCase
	MyTickets      tickets.MyTicketsUseCase

	// Payouts — restaurant settlement (выплаты заведениям). Destinations +
	// generation + send + statement, all behind usecase RBAC.
	Payouts *payouts.UseCase

	// PaymentsPublicBaseURL is threaded straight through from cfg.Payments so
	// the transport layer can build the FreedomPay CallbackURL without
	// importing bootstrap.Config.
	PaymentsPublicBaseURL string

	// MediaStore is the R2 object store backing the admin image-upload
	// endpoint. It is OPTIONAL: nil when R2 is not configured (missing creds or
	// no R2_PUBLIC_BASE), in which case the media routes answer 503 instead of
	// the server refusing to boot. Never nil-panics anywhere — the media
	// handler checks for nil.
	MediaStore *mediastore.Store
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
	// Built here, ahead of the other booking wiring below, because the OTP
	// usecase needs it: a successful phone verification hands the guest the
	// bookings that were made for their number before they had an account.
	bookingRepo := bookingrepo.New(db)

	authCfg := auth.Config{
		RefreshTTL:   cfg.Auth.RefreshTokenTTL,
		OTPTTL:       cfg.Auth.OTPCodeTTL,
		OTPPerMin:    cfg.Auth.OTPRateLimitPerMin,
		OTPPerHour:   cfg.Auth.OTPRateLimitPerHour,
		OTPDevExpose: cfg.Auth.OTPDevExpose,
	}
	authFacade := auth.NewFacade(usersRepo, credsRepo, refreshRepo, txm, issuer, authCfg)
	authOTP := auth.NewOTPUseCase(
		usersRepo, otpRepo, refreshRepo, bookingRepo, txm, issuer,
		newOTPSender(cfg, log),
		authCfg,
	)

	restRepo := restrepo.New(db)
	restRelated := restrepo.NewRelated(db)
	restCategories := restrepo.NewCategories(db)
	restManagers := restrepo.NewManagers(db)
	restPartners := restrepo.NewPartnership(db)

	menuItems := menurepo.New(db)
	menuCategories := menurepo.NewCategories(db)
	storyItems := storyrepo.New(db)

	restaurantManagers := restaurants.NewManagerUseCase(restManagers, usersRepo, txm)
	myRestaurants := restaurants.NewMyRestaurantsUseCase(restManagers, restRepo)
	pushSubscriptions := notifications.NewSubscriptionUseCase(notificationrepo.NewSubscriptions(db), restManagers)
	// Guest mobile push tokens: no restaurant, no RBAC — a guest device is
	// notified about the guest's own bookings, so owning the account IS the
	// authorization.
	deviceTokens := notifications.NewDeviceTokenUseCase(notificationrepo.NewDeviceTokens(db))
	// In-app «Уведомления» feed: the read side of the durable per-guest history
	// that FeedNotifier (wired on the dispatcher below) writes. Same "owning the
	// account IS the authorization" shape as device tokens — no restaurant, no
	// RBAC.
	notificationFeed := notifications.NewNotificationFeedUseCase(notificationrepo.NewFeed(db))
	favoritesRepo := favoriterepo.New(db)
	consentFacade := consent.NewFacade(
		consentrepo.NewConsentRepository(db),
		consentrepo.NewPreferenceRepository(db),
	)

	reviewsFacade := reviews.NewFacade(reviewrepo.New(db), bookingRepo, restManagers)

	// Events & promos (Ф2): admin CRUD + public listings, both gated by the
	// shared RBAC matrix (PermRestaurantManage) via restaurantManagers. The
	// content-draft review queue reuses the same permission gate and creates
	// the real published Event/Promo on approval inside one transaction (txm).
	//
	// The merchandising feed (main-screen "Акции") is a READ MODEL over those
	// same promos/events plus a platform moderation decision (migration 0050) —
	// no third entity. The feed repository is additionally injected into the
	// events/promos facades as their `feedModerator` port: editing an item's
	// content pulls it back into the review queue, so an approved card can
	// never be rewritten into something nobody approved.
	feedRepo := feedrepo.New(db)
	eventsFacade := events.NewFacade(eventrepo.New(db), restaurantManagers, feedRepo)
	promosFacade := promos.NewFacade(promorepo.New(db), restaurantManagers, feedRepo)
	feedFacade := feed.NewFacade(feedRepo, restaurantManagers)

	// Gastroguide (migration 0061): editorial collections of venues. Two halves
	// with two postures — the guest facade is read-only and takes no RBAC port
	// because nothing in it writes; the editor takes the write repository (which
	// needs the transaction manager, since attach/detach/reorder are only correct
	// as a unit) and enforces the superadmin gate itself.
	gastroguideFacade := gastroguide.NewFacade(gastroguiderepo.New(db), eventrepo.New(db), promorepo.New(db))
	gastroguideEditor := gastroguide.NewEditor(gastroguiderepo.NewEditor(db, txm))
	contentFacade := content.NewFacade(
		contentdraftrepo.New(db), eventrepo.New(db), promorepo.New(db), restaurantManagers, txm)
	bookingLinks := bookingrepo.NewTables(db)
	bookingCapacity := bookingrepo.NewCapacity(db)
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

	bookingCreate := bookings.NewCreateUseCase(bookingRepo, bookingLinks, bookingCapacity, bookingItems,
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
	// Event ticketing (roadmap #2): tickets reuse the SAME payments contour as
	// bookings. The ticket-payment usecase owns the money (gross-up, acquirer,
	// ledger, refund CAS); the ticket observer projects a payment's status onto
	// its ticket (pending→paid on capture, →cancelled on fail, →refunded on
	// refund) and is hooked into the webhook so a ticket confirms through the
	// exact same callback flow a booking does.
	eventTicketsRepo := eventticketrepo.New(db)
	ticketObserver := tickets.NewPaymentObserver(eventTicketsRepo)
	paymentWebhook := payments.NewWebhookUseCase(paymentsRepo, paymentEventsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, txm, payments.WithPaymentSubjectObserver(ticketObserver))
	paymentStatus := payments.NewStatusUseCase(paymentsRepo, restaurantManagers)
	ticketPayments := payments.NewTicketPaymentUseCase(paymentsRepo, paymentRefundsRepo, paymentLedgerRepo,
		paymentOutboxRepo, paymentSettings, paymentGateways, restaurantManagers, txm, paymentsCfg)
	ticketPurchase := tickets.NewPurchaseUseCase(eventTicketsRepo, eventrepo.New(db), ticketPayments, paymentsRepo, txm)
	ticketRefund := tickets.NewRefundUseCase(eventTicketsRepo, eventrepo.New(db), ticketPayments, restaurantManagers)
	ticketAdmin := tickets.NewAdminUseCase(eventTicketsRepo, eventrepo.New(db), restaurantManagers)
	myTickets := tickets.NewMyTicketsUseCase(eventTicketsRepo)
	// Deposit hold settlement on booking cancel / no-show (void-early /
	// capture-late-or-noshow), reusing the same window resolver RefundUseCase
	// uses. Hooked into the booking cancel/no-show transitions in bootstrap.
	paymentDepositCancel := payments.NewDepositCancellationUseCase(paymentsRepo, paymentLedgerRepo, paymentOutboxRepo,
		paymentGateways, restaurantManagers, bookingRepo, cancelDeadline, paymentRefund, txm)

	// Named facade/usecase variables so both the Deps struct and the admin
	// panel below share the SAME instances (rather than re-constructing them).
	// The public catalog reports each venue's structured weekly schedule, an
	// "open now" flag computed in the VENUE's timezone, and whether the venue
	// can take an online booking at all — so the guest app stops inferring all
	// three from the free-text opening_hours string. bookingCfg is passed as
	// the timezone resolver on purpose: same zone as the availability engine.
	//
	// ONE instance, shared by every endpoint that serves a catalog row (list,
	// search, detail, favorites). Wiring a second one — or forgetting one
	// endpoint — makes the same venue read differently on two screens.
	venueState := restaurants.NewVenueState(restRelated, bookingCfg)
	favoritesFacade := favorites.NewFacade(favoritesRepo, favorites.WithVenueState(venueState))
	// The catalog's "гости + дата" filter runs on the SAME engine as the slot
	// grid on a venue's booking screen (see bookings.AvailabilitySearch), only
	// batched by venue id. Wiring a second implementation here is how the
	// catalog would start promising tables the booking screen then refuses.
	availabilitySearch := bookings.NewAvailabilitySearch(restRelated, bookingLinks, bookingCapacity, bookingCfg)
	restaurantsFacade := restaurants.NewFacade(restRepo, restRelated, restCategories, restPartners, txm,
		restaurants.WithVenueState(venueState),
		restaurants.WithAvailabilityFilter(availabilitySearch))
	menuFacade := menu.NewFacade(menuItems, menuCategories, txm)
	storiesFacade := stories.NewFacade(storyItems, restaurantManagers)
	bookingsFacade := bookings.NewFacade(bookingRepo, bookingLinks, bookingItems,
		bookingMessages, bookingSurveys, bookingHistory, bookingOutbox, restaurantManagers, txm,
		bookings.WithFreeCancelDeadlineResolver(cancelDeadline), // same window as the money path
		bookings.WithVenueLocationResolver(venueLocationAdapter{restaurants: restRepo, cfg: bookingCfg}))
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
	payoutUC := payouts.NewUseCase(newPayoutPorts(db, restaurantManagers, txm, log), newPayoutsConfig(cfg), log)

	// Admin image upload (R2). OPTIONAL by design: if the R2 credentials or the
	// public base are not set, we log once and leave MediaStore nil — the media
	// routes then return 503 "not configured" instead of the server failing to
	// boot. This is the same "an optional dependency being absent is not a bug"
	// posture as the payout gateway above.
	mediaStore := newMediaStore(log)

	return &Deps{
		AuthFacade:            authFacade,
		AuthOTP:               authOTP,
		UsersFacade:           users.NewFacade(usersRepo, userCuisineRepo, refreshRepo, otpRepo, txm),
		UsersRepo:             usersRepo,
		RestaurantsFacade:     restaurantsFacade,
		RestaurantManagers:    restaurantManagers,
		MyRestaurants:         myRestaurants,
		PushSubscriptions:     pushSubscriptions,
		DeviceTokens:          deviceTokens,
		NotificationFeed:      notificationFeed,
		FavoritesFacade:       favoritesFacade,
		ConsentFacade:         consentFacade,
		ReviewsFacade:         reviewsFacade,
		EventsFacade:          eventsFacade,
		PromosFacade:          promosFacade,
		GastroguideFacade:     gastroguideFacade,
		GastroguideEditor:     gastroguideEditor,
		ContentFacade:         contentFacade,
		FeedFacade:            feedFacade,
		MenuFacade:            menuFacade,
		StoriesFacade:         storiesFacade,
		BookingsFacade:        bookingsFacade,
		BookingCreate:         bookingCreate,
		BookingIdempotent:     bookings.NewIdempotentCreateUseCase(bookingCreate, idempotencyKeys, txm),
		BookingStatus:         bookingStatus,
		Roles:                 rolesuc.NewUseCase(userrolerepo.New(db, txm), usersRepo),
		VenueDashboard:        venuedashboarduc.NewUseCase(venuedashboardrepo.New(db)),
		VenueToday:            venuedashboarduc.NewTodayUseCase(venuedashboardrepo.NewToday(db)),
		NotificationSettings:  notificationrepo.NewSettings(db),
		TelegramAnswerer:      newTelegramAnswerer(cfg),
		TelegramWebhookSecret: strings.TrimSpace(cfg.Push.TelegramWebhookSecret),
		BookingUpdate: bookings.NewUpdateUseCase(bookingRepo, bookingLinks, bookingCapacity, bookingOutbox,
			restRepo, restRelated, restaurantManagers, txm, bookingCfg),
		BookingAvail: bookings.NewAvailabilityUseCase(bookingLinks, bookingCapacity, restRepo,
			restRelated, bookingCfg),
		BookingBlacklist: bookings.NewBlacklistUseCase(bookingBlacklist, restaurantManagers),
		BookingPolicy: bookings.NewPolicyUseCase(restRepo, restRepo, restaurantManagers,
			restRelated, bookingCapacity, bookingLinks, bookingRepo, txm, bookingCfg),
		AdminPanel: adminPanel,
		Dashboard:  dashboardUC,
		BookingExternal: bookings.NewExternalReservationUseCase(bookingExternal, restRepo,
			restRelated, restaurantManagers, txm),
		BookingOverrides: bookings.NewCapacityOverrideUseCase(bookingCapacity, restaurantManagers),
		// Pre-order (roadmap #1): the guest attaches menu items to a booking to be
		// prepared and PAID FOR upfront. Reuses booking_items for the lines; the
		// total it computes server-side (never a client amount) is what
		// usecase/payments charges as PurposePreorder. menuItems = current price +
		// availability lookup, restRepo = the venue's preorder minimum, paymentsRepo
		// = the "already paid → frozen" guard.
		Preorder: preorder.NewUseCase(bookingRepo, menuItems, bookingItems, restRepo,
			restaurantManagers, paymentsRepo, txm),
		// Server-side map preview. Always constructed, even without a provider
		// key: the endpoint then answers a clean map_not_configured instead of
		// disappearing from the routing table, so the app gets one stable
		// contract and the day the key arrives it is one env var and a restart.
		StaticMap: newStaticMap(cfg, restRepo, log),
		Issuer:    issuer,

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
		TicketPurchase:        ticketPurchase,
		TicketRefund:          ticketRefund,
		TicketAdmin:           ticketAdmin,
		MyTickets:             myTickets,
		PaymentStatus:         paymentStatus,
		PaymentDepositCancel:  paymentDepositCancel,
		Payouts:               payoutUC,
		PaymentsPublicBaseURL: cfg.Payments.PublicBaseURL,
		MediaStore:            mediaStore,
	}, nil
}

// newPayoutPorts wires the payout usecase's repositories. Extracted so the HTTP
// stack (NewDeps) and the worker (NewDailyPayoutRunner) build the SAME set of
// ports — a scheduled payout and a manual one must not be able to drift apart.
// perms may be nil for the worker: it never authorizes an Actor.
// newTelegramAnswerer builds the client that acknowledges a button press and
// rewrites the alert. It returns nil unless BOTH the bot token and the webhook
// secret are set: those are exactly the conditions under which buttons are put
// on a message in the first place, so the two decisions cannot drift apart.
func newTelegramAnswerer(cfg Config) telegramhook.Answerer {
	tgCfg := telegramnotify.Config{BotToken: cfg.Push.TelegramBotToken}
	if !tgCfg.Configured() || strings.TrimSpace(cfg.Push.TelegramWebhookSecret) == "" {
		return nil
	}
	return telegramnotify.NewSender(tgCfg)
}

func newPayoutPorts(db *pgxpool.Pool, perms permissionCheckerPort, txm domain.TxManager, log *slog.Logger) payouts.Ports {
	return payouts.Ports{
		Perms:        perms,
		Destinations: payoutrepo.NewDestinations(db),
		Payouts:      payoutrepo.NewPayouts(db),
		Items:        payoutrepo.NewItems(db),
		Owed:         payoutrepo.NewOwed(db),
		Ledger:       payoutrepo.NewLedger(db),
		Venues:       payoutrepo.NewVenues(db),
		Settings:     payoutrepo.NewSettings(db),
		Gateway:      newPayoutGateway(log),
		Tx:           txm,
	}
}

// permissionCheckerPort mirrors the payouts usecase's unexported RBAC port so
// newPayoutPorts can accept a nil one without importing an unexported type.
type permissionCheckerPort interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// newPayoutsConfig maps the env-level payout money policy into the usecase's
// own Config. The fee bearer is validated here: an unrecognised
// PAYOUTS_FEE_BEARER falls back to "platform" (the safe policy) rather than
// silently deducting from venues.
func newPayoutsConfig(cfg Config) payouts.Config {
	return payouts.Config{
		FeeBps:          cfg.Payouts.FeeBps,
		FeeMinimumMinor: cfg.Payouts.FeeMinimumMinor,
		FeeBearer:       domain.PayoutFeeBearer(strings.ToLower(strings.TrimSpace(cfg.Payouts.FeeBearer))),
		// The PLATFORM policy — the fallback for a venue with no
		// restaurant_payout_settings row of its own, and the base every
		// per-venue override is layered onto.
		PlatformPolicy: domain.PayoutPolicy{
			MinPayoutMinor: cfg.Payouts.MinPayoutMinor,
			MaxHoldDays:    cfg.Payouts.MaxHoldDays,
		},
	}
}

// NewDailyPayoutRunner wires the scheduled end-of-day payout pass: one payout
// per venue per VENUE-LOCAL day. Like the reconcilers it lives outside NewDeps
// (no HTTP stack) and is safe to start unconditionally — with no venue owed
// money every tick is a single cheap query, and with PAYOUTS_DAILY_SEND_ENABLED
// unset it only GENERATES pending payouts, moving no money.
func NewDailyPayoutRunner(cfg Config, db *pgxpool.Pool, log *slog.Logger) *payouts.DailyRunner {
	ports := newPayoutPorts(db, nil, sqltx.NewManager(db), log)
	uc := payouts.NewUseCase(ports, newPayoutsConfig(cfg), log)
	return payouts.NewDailyRunner(uc, ports.Venues, payouts.DailyConfig{
		TickInterval: cfg.Payouts.DailyTickInterval,
		SendEnabled:  cfg.Payouts.DailySendEnabled,
		// The payout threshold and the max-hold window are NOT here: they are
		// part of the usecase's money policy (newPayoutsConfig) because they
		// are per-venue overridable and must be resolved in one place.
		// The venue's own timezone wins; this is only the fallback for a venue
		// that has none, and it is the platform's booking fallback so payouts
		// and bookings agree on what "a day" means for such a venue.
		TimezoneFallback: cfg.Booking.TimezoneFallback,
	}, log)
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

// newMediaStore builds the R2-backed object store for the admin image-upload
// endpoint, or returns nil (logged) when R2 is not configured. Returning nil is
// a supported, non-fatal state: the server still boots and the media routes
// answer 503. A public base is required in addition to the credentials —
// without it PutOriginal could succeed but we would have no URL to return, so
// we treat "no public base" as "not configured" here rather than shipping a
// half-working upload.
func newMediaStore(log *slog.Logger) *mediastore.Store {
	cfg, err := mediastore.ConfigFromEnv()
	if err != nil {
		log.Info("media upload disabled: R2 not configured", slog.String("reason", err.Error()))
		return nil
	}
	if cfg.PublicBase == "" {
		log.Info("media upload disabled: R2_PUBLIC_BASE is not set (no public URL to return)")
		return nil
	}
	store, err := mediastore.New(context.Background(), cfg)
	if err != nil {
		log.Warn("media upload disabled: building the R2 client failed", slog.String("reason", err.Error()))
		return nil
	}
	log.Info("media upload enabled", slog.String("bucket", cfg.Bucket), slog.String("public_base", cfg.PublicBase))
	return store
}

// NewPayoutReconciler wires the background payout reconciliation worker
// (usecase/payouts.Reconciler). Like NewPaymentsReconciler it is deliberately
// separate from NewDeps (no HTTP stack). It is safe-idle when the payout
// gateway is disabled (nil): Tick returns immediately.
func NewPayoutReconciler(cfg Config, db *pgxpool.Pool, log *slog.Logger) *payouts.Reconciler {
	return payouts.NewReconciler(
		payoutrepo.NewPayouts(db), payoutrepo.NewItems(db), payoutrepo.NewLedger(db),
		newPayoutGateway(log), sqltx.NewManager(db),
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
		Enabled:                      cfg.Payments.Enabled,
		DefaultProvider:              domain.PaymentProvider(cfg.Payments.DefaultProvider),
		ServiceFeeBps:                cfg.Payments.ServiceFeeBps,
		RefundAcquiringBps:           cfg.Payments.RefundAcquiringBps,
		RefundAcquiringBpsByProvider: refundAcquiringBpsByProvider(cfg.Payments.RefundAcquiringBpsByProvider),
		AcquirerMinFeeMinor:          cfg.Payments.AcquirerMinFeeMinor,
		DepositDefaultMinor:          cfg.Payments.DepositDefaultMinor,
		DepositRequired:              cfg.Payments.DepositRequired,
		PreorderPaymentRequired:      cfg.Payments.PreorderPaymentRequired,
		HoldTTL:                      cfg.Payments.HoldTTL,
		FreeCancelWindow:             cfg.Payments.FreeCancelWindow,
	}
}

// refundAcquiringBpsByProvider converts the config layer's plain string keys
// into domain providers, dropping any name the domain does not know (a typo in
// an env var must not become a silent, unreachable entry — the payment's real
// provider would then quietly fall back to the global rate anyway).
func refundAcquiringBpsByProvider(raw map[string]int) map[domain.PaymentProvider]int {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[domain.PaymentProvider]int, len(raw))
	for name, bps := range raw {
		p := domain.PaymentProvider(name)
		if !p.Valid() {
			continue
		}
		out[p] = bps
	}
	return out
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

// venueLocationAdapter implements usecase/bookings' venueLocationResolver: it
// answers which zone a venue's calendar day is measured in, for the ?date=
// filter of the venue calendar.
//
// It resolves the venue's OWN stored zone strictly (domain.LoadVenueLocation —
// an unusable value is an error, never a substituted zone), and falls back to
// the platform zone only when the venue has none. That is the same split the
// payout pass makes: "no zone of its own" is a configuration, "a zone we cannot
// read" is a data fault, and answering a fault with the platform default is how
// a whole screen of bookings ends up on the wrong day without anybody noticing.
type venueLocationAdapter struct {
	restaurants interface {
		GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error)
	}
	cfg bookings.Config
}

func (a venueLocationAdapter) VenueLocation(ctx context.Context, restaurantID uuid.UUID) (*time.Location, error) {
	agg, err := a.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	if tz := agg.BookingPolicy.Timezone; tz != nil && strings.TrimSpace(*tz) != "" {
		return domain.LoadVenueLocation(*tz)
	}
	return a.PlatformLocation()
}

func (a venueLocationAdapter) PlatformLocation() (*time.Location, error) {
	return domain.LoadVenueLocation(a.cfg.TimezoneFallback)
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
		ReminderLead: cfg.Worker.ReminderLead,
		BatchSize:    cfg.Worker.BatchSize,
	}
	// The pre-visit guest reminder pass. OFF by default, and deliberately so:
	// while the old Supabase system is still live it sends its own 60' and 30'
	// reminders for the same bookings (the legacy sync keeps importing them),
	// and a guest reminded by both systems is a complaint, not a feature. The
	// claim predicate already skips bookings the old system has ALREADY
	// reminded, but a freshly imported booking carries no such marker yet — only
	// the owner knows when the old reminder path is switched off, so that
	// knowledge lives in an env flag rather than in a guess.
	//
	// The pass is either on or absent: a disabled pass must not stamp
	// guest_reminder_sent_at, otherwise turning it on later would find every
	// upcoming booking already marked and silently remind nobody.
	var reminders bookings.WorkerOption = func(*bookings.Worker) {}
	if cfg.Worker.GuestRemindersEnabled {
		reminders = bookings.WithGuestReminders(bookingrepo.NewReminders(db))
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
			restRepo, txm, newBookingConfig(cfg), wcfg, log, reminders)
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
		bookings.WithWorkerDepositSettler(depositSettlerAdapter{uc: depositCancel}), reminders)
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
	// Same ticket projection the HTTP webhook uses, so a payment the reconciler
	// transitions (expired ticket hold voided, lost ticket capture synced in)
	// reaches its ticket (seat freed / ticket marked paid) instead of stranding
	// it `pending` forever.
	ticketObserver := tickets.NewPaymentObserver(eventticketrepo.New(db))
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
		}, log,
		payments.WithReconcilerObserver(ticketObserver)), nil
}

// NewTicketSweeper builds the pending-ticket sweep worker: it releases seats
// held by pending tickets whose payment never completed (or was never created),
// the backstop the payments reconciler cannot cover because a pending ticket may
// have no payment row at all. Safe-idle when there are no stale pending tickets.
func NewTicketSweeper(cfg Config, db *pgxpool.Pool, log *slog.Logger) *tickets.PendingSweeper {
	// StaleAfter must comfortably exceed the payments HoldTTL so a ticket whose
	// hold is still legitimately in flight is never swept; the configured value
	// (default 100h) is floored at HoldTTL+4h as a safety net against a
	// misconfigured-too-low override.
	staleAfter := cfg.TicketsSweep.StaleAfter
	if floor := newPaymentsConfig(cfg).HoldTTL + 4*time.Hour; staleAfter < floor {
		staleAfter = floor
	}
	return tickets.NewPendingSweeper(
		eventticketrepo.New(db), paymentrepo.New(db),
		tickets.SweepConfig{
			TickInterval: cfg.TicketsSweep.TickInterval,
			StaleAfter:   staleAfter,
			BatchSize:    cfg.TicketsSweep.BatchSize,
		}, log)
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
	// Buttons go under the alert only when the venue can actually answer them:
	// that needs both the bot token (to send) and the webhook secret (to receive
	// the press). Buttons that lead nowhere are worse than none — staff press
	// them and conclude the product is broken.
	if tgCfg.Configured() && strings.TrimSpace(cfg.Push.TelegramWebhookSecret) != "" {
		sender := telegramnotify.NewSender(tgCfg)
		telegram = telegram.WithActions(sender.SendWithActions, func(bookingID uuid.UUID) [][2]string {
			return [][2]string{
				{"Подтвердить", telegramhook.CallbackConfirm(bookingID)},
				{"Отклонить", telegramhook.CallbackReject(bookingID)},
			}
		})
	}

	// Guest channel: a THIRD notifier on the same dispatcher — the first one
	// addressed to the guest rather than to venue staff, so it is the only one
	// that consults the guest opt-out gate. Absent GUEST_PUSH_PROVIDER → built
	// disabled and no-ops (like web push without VAPID keys).
	var guestSender notifications.MobilePushSender
	if cfg.Push.GuestPushConfigured() {
		switch strings.ToLower(strings.TrimSpace(cfg.Push.GuestPushProvider)) {
		case "expo":
			guestSender = expopush.NewSender(expopush.Config{
				AccessToken: cfg.Push.ExpoAccessToken,
				Endpoint:    cfg.Push.ExpoEndpoint,
			}).Send
		default:
			// An unknown provider name is a config typo, not a reason to crash the
			// worker: the channel stays a no-op and says so, loudly.
			log.Error("unknown GUEST_PUSH_PROVIDER — the guest push channel will no-op",
				slog.String("provider", cfg.Push.GuestPushProvider))
		}
	} else {
		log.Warn("guest push not configured (no GUEST_PUSH_PROVIDER) — guests will not be notified until it is set")
	}
	guestPush := notifications.NewGuestPushNotifier(
		notificationrepo.NewDeviceTokens(db),
		notificationrepo.NewDeliveries(db),
		notifications.NewGuestNotificationGate(consentrepo.NewPreferenceRepository(db)),
		notificationrepo.NewVenues(db),
		guestSender,
		guestSender != nil,
		log,
	)

	// In-app feed channel: a FOURTH notifier on the same dispatcher. Unlike the
	// three push channels it needs no provider or credentials — it only writes a
	// durable row — so it is always active. Its idempotency is the notifications
	// table's own unique key, not the delivery ledger.
	feedNotifier := notifications.NewFeedNotifier(
		notificationrepo.NewFeed(db),
		notificationrepo.NewVenues(db),
		log,
	)

	return notifications.NewDispatcher(
		bookingrepo.NewOutbox(db), txm,
		notifications.DispatcherConfig{
			TickInterval: cfg.Push.DispatchTick,
			BatchSize:    cfg.Push.DispatchBatch,
		}, log, webPush, telegram, guestPush, feedNotifier)
}

// newOTPSender builds the login-code delivery sender.
//
// The rule is one line long and deliberately has no "enabled" switch: a channel
// exists if and only if its credentials are present. Nothing configured →
// otpsender.Stub, i.e. exactly today's behaviour (the code is logged in
// development, and a loud WARN everywhere else). So the day a token arrives it
// is one env var and a restart, and the day it is revoked the contour degrades
// to the remaining channels instead of failing every login.
// otpBudgetSafetyMargin is how much of the HTTP server's write budget must be
// left over after the waterfall has given up: the handler still has to record
// the attempt, render the error and get the bytes onto the wire.
const otpBudgetSafetyMargin = 3 * time.Second

// maxOTPDeliveryBudget is the hard ceiling on synchronous OTP delivery.
func maxOTPDeliveryBudget() time.Duration { return httpWriteTimeout - otpBudgetSafetyMargin }

// otpDeliveryDeadlines validates OTP_SEND_TIMEOUT / OTP_DELIVERY_BUDGET against
// the HTTP server's WriteTimeout and clamps whatever does not fit.
//
// Without this an operator can set OTP_DELIVERY_BUDGET=20s, the service boots
// happily, and every slow login re-creates the exact failure the budget exists
// to prevent: the guest's connection dies at WriteTimeout while the waterfall
// keeps walking channels and paying providers for a response nobody will read.
//
// Clamping rather than refusing to boot, deliberately: this repo only refuses
// to start for values it cannot interpret (a malformed basis-point rate); a
// duration that parses fine but is too generous is an ops mistake, and the same
// treatment as a bad APP_TRUSTED_PROXIES or an unknown OTP_SMS_PROVIDER applies
// — log it loudly and keep serving logins. The log line says what was clamped,
// to what, and why, so it is actionable rather than mysterious.
func otpDeliveryDeadlines(channelTimeout, budget time.Duration, log *slog.Logger) (time.Duration, time.Duration) {
	max := maxOTPDeliveryBudget()
	if budget > max {
		log.Error("OTP_DELIVERY_BUDGET exceeds what the HTTP server can wait for — clamped",
			slog.Duration("configured", budget),
			slog.Duration("applied", max),
			slog.Duration("http_write_timeout", httpWriteTimeout),
			slog.Duration("safety_margin", otpBudgetSafetyMargin),
			slog.String("detail", "a longer budget would keep paying OTP providers after the guest's connection is already dead"),
		)
		budget = max
	}
	// One channel may not outlast the whole waterfall either: the Waterfall
	// would otherwise raise the budget to the per-channel timeout and undo the
	// clamp above.
	if channelTimeout > max {
		log.Error("OTP_SEND_TIMEOUT exceeds what the HTTP server can wait for — clamped",
			slog.Duration("configured", channelTimeout),
			slog.Duration("applied", max),
			slog.Duration("http_write_timeout", httpWriteTimeout),
		)
		channelTimeout = max
	}
	return channelTimeout, budget
}

// newStaticMap wires the restaurant map-preview proxy.
//
// Same rule as every other optional provider in this file: the feature exists
// if and only if its credential does, and its absence is a warning at startup,
// not a boot failure. An unknown STATIC_MAP_PROVIDER is an operator typo — it
// is reported loudly and treated as "no provider", never as a reason to crash.
//
// restaurants is the ordinary Postgres restaurant repository: resolving an id
// to its coordinates needs no new query, and the cache keeps the lookup off the
// hot path anyway.
func newStaticMap(cfg Config, restaurants staticmap.RestaurantCoords, log *slog.Logger) *staticmap.UseCase {
	cache := staticmap.NewMemoryCache(cfg.StaticMap.CacheTTL, cfg.StaticMap.CacheMaxBytes)

	var provider staticmap.Provider
	switch name := strings.ToLower(strings.TrimSpace(cfg.StaticMap.Provider)); name {
	case "":
		log.Warn("static map proxy not configured (no STATIC_MAP_PROVIDER) — /restaurants/:id/map will answer map_not_configured")
	case "2gis":
		twoGIS := twogis.Config{
			APIKey:  cfg.StaticMap.TwoGISAPIKey,
			BaseURL: cfg.StaticMap.TwoGISBaseURL,
			Timeout: cfg.StaticMap.Timeout,
		}
		if !twoGIS.Configured() {
			// Never log the key — only the fact that it is missing.
			log.Error("STATIC_MAP_PROVIDER=2gis but STATIC_MAP_2GIS_API_KEY is empty — the map proxy stays disabled")
			break
		}
		provider = twogis.NewClient(twoGIS)
	default:
		log.Error("unknown STATIC_MAP_PROVIDER — the map proxy stays disabled",
			slog.String("provider", cfg.StaticMap.Provider))
	}

	return staticmap.New(restaurants, provider, cache, log)
}

func newOTPSender(cfg Config, log *slog.Logger) auth.OTPSender {
	available := make(map[string]otpsender.Channel, 3)
	channelTimeout, budget := otpDeliveryDeadlines(cfg.OTPDelivery.SendTimeout, cfg.OTPDelivery.DeliveryBudget, log)

	tgCfg := otpsender.TelegramGatewayConfig{
		Token:          cfg.OTPDelivery.TelegramGatewayToken,
		SenderUsername: cfg.OTPDelivery.TelegramSenderUser,
		CodeTTL:        cfg.Auth.OTPCodeTTL,
		Timeout:        channelTimeout,
		BaseURL:        cfg.OTPDelivery.TelegramGatewayAPIURL,
	}
	if tgCfg.Configured() {
		available[domain.OTPChannelTelegram] = otpsender.NewTelegramGateway(tgCfg)
	}

	waCfg := otpsender.WhatsAppConfig{
		AccessToken:    cfg.OTPDelivery.WhatsAppAccessToken,
		PhoneNumberID:  cfg.OTPDelivery.WhatsAppPhoneNumberID,
		TemplateName:   cfg.OTPDelivery.WhatsAppTemplateName,
		TemplateLang:   cfg.OTPDelivery.WhatsAppTemplateLang,
		APIVersion:     cfg.OTPDelivery.WhatsAppAPIVersion,
		CopyCodeButton: cfg.OTPDelivery.WhatsAppCopyButton,
		Timeout:        channelTimeout,
		BaseURL:        cfg.OTPDelivery.WhatsAppAPIURL,
	}
	if waCfg.Configured() {
		available[domain.OTPChannelWhatsApp] = otpsender.NewWhatsApp(waCfg)
	}

	if provider := newSMSProvider(cfg, channelTimeout, log); provider != nil {
		available[domain.OTPChannelSMS] = otpsender.NewSMS(provider, otpsender.SMSConfig{CodeTTL: cfg.Auth.OTPCodeTTL})
	}

	// Apply the configured order to whatever is actually available. An unknown
	// name is a typo in one env var; it must not cost the other channels, so it
	// is logged and skipped rather than fatal.
	ordered := make([]otpsender.Channel, 0, len(available))
	seen := make(map[string]bool, len(available))
	for _, name := range cfg.OTPDelivery.ChannelOrder {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		switch {
		case available[name] != nil:
			ordered = append(ordered, available[name])
		case domain.OTPRememberableChannel(name):
			// A real channel name whose credentials are absent: expected before
			// the tokens arrive, so INFO rather than a warning that cries wolf.
			log.Info("otp channel not configured — skipped", slog.String("channel", name))
		default:
			log.Error("unknown channel in OTP_CHANNEL_ORDER — ignored", slog.String("channel", name))
		}
	}
	// A configured channel missing from the order would be silently unreachable;
	// that is a footgun, so it is appended and reported instead.
	for name, ch := range available {
		if !seen[name] {
			log.Warn("configured OTP channel is absent from OTP_CHANNEL_ORDER — appended last",
				slog.String("channel", name))
			ordered = append(ordered, ch)
		}
	}

	waterfall := otpsender.NewWaterfall(log, otpsender.WaterfallConfig{
		ChannelTimeout: channelTimeout,
		TotalBudget:    budget,
	}, ordered...)
	if waterfall == nil {
		log.Warn("no OTP delivery channel configured — falling back to the stub sender; " +
			"guests cannot receive login codes until OTP_TELEGRAM_GATEWAY_TOKEN / OTP_WHATSAPP_* / OTP_SMS_* are set")
		return otpsender.NewStub(log, cfg.App.Environment)
	}
	log.Info("otp delivery configured", slog.Any("channels", waterfall.Channels()))
	return waterfall
}

// newSMSProvider selects the SMS backend. Returns nil when none is selected or
// the selected one lacks credentials — the SMS channel then simply does not
// exist, exactly like an unconfigured Telegram or WhatsApp.
func newSMSProvider(cfg Config, timeout time.Duration, log *slog.Logger) otpsender.SMSProvider {
	switch strings.ToLower(strings.TrimSpace(cfg.OTPDelivery.SMSProvider)) {
	case "":
		return nil
	case "twilio":
		twilio := otpsender.TwilioConfig{
			AccountSID:          cfg.OTPDelivery.TwilioAccountSID,
			AuthToken:           cfg.OTPDelivery.TwilioAuthToken,
			From:                cfg.OTPDelivery.TwilioFrom,
			MessagingServiceSID: cfg.OTPDelivery.TwilioMessagingSID,
			Timeout:             timeout,
			BaseURL:             cfg.OTPDelivery.TwilioAPIURL,
		}
		if !twilio.Configured() {
			log.Error("OTP_SMS_PROVIDER=twilio but its credentials are incomplete — the SMS channel is disabled")
			return nil
		}
		return otpsender.NewTwilio(twilio)
	case "mobizon":
		mobizon := otpsender.MobizonConfig{
			APIKey:  cfg.OTPDelivery.MobizonAPIKey,
			Sender:  cfg.OTPDelivery.MobizonSender,
			Timeout: timeout,
			BaseURL: cfg.OTPDelivery.MobizonAPIURL,
		}
		if !mobizon.Configured() {
			log.Error("OTP_SMS_PROVIDER=mobizon but OTP_SMS_MOBIZON_API_KEY is empty — the SMS channel is disabled")
			return nil
		}
		return otpsender.NewMobizon(mobizon)
	default:
		// A typo here must not crash the server: no SMS is survivable, a boot
		// loop is not.
		log.Error("unknown OTP_SMS_PROVIDER — the SMS channel is disabled",
			slog.String("provider", cfg.OTPDelivery.SMSProvider))
		return nil
	}
}

// NewAnalyticsDispatcher wires the background Amplitude analytics worker. It
// re-reads the EXISTING booking_outbox / payment_outbox through its own cursor
// (analytics_cursor, migration 0046) and ships a PII-free projection of the
// tracked rows to Amplitude in batches. It never touches the request path.
//
// When AMPLITUDE_API_KEY is absent the worker is built with a no-op sender: it
// still ticks and advances its cursor (a cheap read), it just sends nothing —
// so provisioning a key later never needs a schema change or a worker rebuild,
// only the env var and a restart (same discipline as the notification
// dispatcher without VAPID keys).
func NewAnalyticsDispatcher(cfg Config, db *pgxpool.Pool, log *slog.Logger) *analytics.Dispatcher {
	var sender analytics.Sender
	if cfg.Analytics.Configured() {
		sender = amplitude.NewClient(amplitude.Config{
			APIKey:   cfg.Analytics.APIKey,
			Endpoint: cfg.Analytics.Endpoint,
			Timeout:  cfg.Analytics.HTTPTimeout,
		}, log)
	} else {
		log.Warn("amplitude analytics not configured (no AMPLITUDE_API_KEY) — the worker will run but ship nothing until it is set")
		sender = analytics.NewNoopSender()
	}
	return analytics.NewDispatcher(
		analyticsrepo.NewSourceReader(db),
		analyticsrepo.NewCursorStore(db),
		sender,
		analytics.Config{
			TickInterval: cfg.Analytics.DispatchTick,
			BatchSize:    cfg.Analytics.DispatchBatch,
		}, log)
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
		sqltx.NewManager(db),
		legacysync.Config{
			TickInterval:    cfg.LegacySync.TickInterval,
			BatchSize:       cfg.LegacySync.BatchSize,
			DefaultDuration: cfg.Booking.DefaultDuration,
		},
		log,
	)
	return worker, pool.Close, nil
}
