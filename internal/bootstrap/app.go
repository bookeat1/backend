package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	adminrest "backend-core/internal/transport/rest/admin"
	authrest "backend-core/internal/transport/rest/auth"
	bookingsrest "backend-core/internal/transport/rest/bookings"
	consentrest "backend-core/internal/transport/rest/consent"
	contentrest "backend-core/internal/transport/rest/content"
	dashboardrest "backend-core/internal/transport/rest/dashboard"
	devicetokensrest "backend-core/internal/transport/rest/devicetokens"
	eventsrest "backend-core/internal/transport/rest/events"
	favoritesrest "backend-core/internal/transport/rest/favorites"
	feedrest "backend-core/internal/transport/rest/feed"
	gastroguiderest "backend-core/internal/transport/rest/gastroguide"
	menurest "backend-core/internal/transport/rest/menu"
	"backend-core/internal/transport/rest/middleware"
	myrestaurantsrest "backend-core/internal/transport/rest/myrestaurants"
	paymentsrest "backend-core/internal/transport/rest/payments"
	payoutsrest "backend-core/internal/transport/rest/payouts"
	preorderrest "backend-core/internal/transport/rest/preorder"
	promosrest "backend-core/internal/transport/rest/promos"
	pushsubscriptionsrest "backend-core/internal/transport/rest/pushsubscriptions"
	restrest "backend-core/internal/transport/rest/restaurants"
	reviewsrest "backend-core/internal/transport/rest/reviews"
	rolesrest "backend-core/internal/transport/rest/roles"
	staticmaprest "backend-core/internal/transport/rest/staticmap"
	storiesrest "backend-core/internal/transport/rest/stories"
	"backend-core/internal/transport/rest/swaggerui"
	"backend-core/internal/transport/rest/telegramhook"
	ticketsrest "backend-core/internal/transport/rest/tickets"
	usersrest "backend-core/internal/transport/rest/users"
	venuedashboardrest "backend-core/internal/transport/rest/venuedashboard"
)

// HTTP server timeouts. httpWriteTimeout is a named constant rather than a
// literal because other subsystems have to fit inside it: anything a handler
// does synchronously (notably the OTP delivery waterfall, see newOTPSender)
// must finish before the server stops being able to write the response, or we
// keep working — and paying providers — for a guest whose connection is gone.
const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 15 * time.Second
	httpWriteTimeout      = 15 * time.Second
	httpIdleTimeout       = 60 * time.Second
)

// NewApp builds the Gin engine with all routes wired. db is used by the
// readiness probe to verify database connectivity.
func NewApp(cfg Config, deps *Deps, db *pgxpool.Pool, log *slog.Logger) *gin.Engine {
	// response.HandleError logs failures via slog.Default(); point it at the
	// configured logger so those logs use the app's handler and level.
	slog.SetDefault(log)
	if cfg.App.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	// Only trust X-Forwarded-For/X-Real-IP from cfg.App.TrustedProxies
	// (empty by default — see AppConfig.TrustedProxies's doc). gin.New()
	// otherwise trusts EVERY proxy by default, which would let any caller
	// spoof its own ClientIP() (used both by AccessLog's "ip" field and by
	// middleware.RateLimit's per-IP bucket key) with a forged header. A
	// misconfigured value here is an ops mistake, not a reason to refuse to
	// serve traffic, so the error is logged and otherwise ignored.
	if err := r.SetTrustedProxies(cfg.App.TrustedProxies); err != nil {
		log.Warn("invalid APP_TRUSTED_PROXIES, falling back to trusting nobody", slog.String("error", err.Error()))
	}
	// Order matters: RequestID must run first so every later middleware and
	// handler can log through a context that already carries request_id.
	// AccessLog wraps Recovery so a panic converted to a 500 downstream is
	// still measured and logged as one request line, not lost. RateLimit
	// runs after CORS so a rejected request still carries CORS headers, and
	// before any route/auth work so a throttled caller never reaches a DB
	// lookup.
	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLog())
	r.Use(middleware.Recovery())
	r.Use(middleware.CORS(cfg.App.CORSAllowedOrigins))
	r.Use(middleware.RateLimit(cfg.RateLimit.RateLimitConfig, middleware.NewInMemoryLimiter(cfg.RateLimit.IdleTTL, cfg.RateLimit.SweepEvery)))

	// /health is a liveness probe (process is up). /health/ready is a readiness
	// probe (dependencies reachable) and pings the database.
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}}) })
	r.GET("/health/ready", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"data": gin.H{"status": "unavailable"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ready"}})
	})
	r.GET("/.well-known/jwks.json", func(c *gin.Context) { c.JSON(http.StatusOK, deps.Issuer.JWKS()) })

	// Interactive API docs at /docs — mounted only outside production.
	swaggerui.Register(r, cfg.App.Environment)

	// The first administrator. Runs once per start and does nothing unless the
	// platform has none, so it cannot silently restore rights that were taken
	// away on purpose. A failure is logged, never fatal: the service must still
	// come up.
	if err := deps.Roles.EnsureBootstrapAdmin(context.Background(), cfg.App.BootstrapAdminEmail); err != nil {
		log.Error("bootstrap admin failed", slog.String("error", err.Error()))
	}

	api := r.Group("/api/v1")
	authrest.NewHandler(deps.AuthFacade, deps.AuthOTP).RegisterRoutes(api)

	restHandler := restrest.NewHandler(deps.RestaurantsFacade, deps.RestaurantManagers, deps.FavoritesFacade)
	// OptionalAuth (not Auth): the catalog itself is public, but a logged-in
	// caller gets an "is_favorite" flag on each item — see
	// restrest.Handler.attachFavorites. A missing/invalid token behaves
	// exactly like no token at all, never a 401 on a public route.
	restPublic := api.Group("")
	restPublic.Use(middleware.OptionalAuth(deps.Issuer, deps.UsersRepo))
	restHandler.RegisterPublic(restPublic)

	// Server-side static map preview. Fully public and NOT on the OptionalAuth
	// group: the picture is identical for every caller, so parsing a token
	// would only add a user lookup to a route meant to be cheap. Registered
	// unconditionally — without a provider key it answers a clean
	// 503/map_not_configured, which the app already handles like "no map".
	staticmaprest.NewHandler(deps.StaticMap).RegisterPublic(api)

	// Inbound Telegram updates: the Confirm/Reject buttons under a venue's
	// booking alert. Mounted OUTSIDE every auth group because Telegram cannot
	// carry a bearer token — the handler authenticates the request by a secret
	// header and the chat it came from instead (see package telegramhook).
	// Registered unconditionally: without the secret the handler answers 404 and
	// decides nothing, which is the same as not existing but keeps the wiring in
	// one place.
	telegramhook.NewHandler(
		deps.BookingStatus, deps.NotificationSettings,
		deps.TelegramAnswerer, deps.TelegramWebhookSecret,
	).RegisterRoutes(api)

	// Merchandising feed (main-screen "Акции"). The guest rail mounts on the
	// SAME OptionalAuth group and for the same reason as the catalog: it is a
	// public screen, but a signed-in guest gets their cuisine preferences folded
	// into the ranking. The venue and platform sides mount further down.
	feedHandler := feedrest.NewHandler(deps.FeedFacade)
	feedHandler.RegisterPublic(restPublic)

	authed := api.Group("")
	authed.Use(middleware.Auth(deps.Issuer, deps.UsersRepo))
	authed.Use(middleware.LogUserContext())
	usersrest.NewHandler(deps.UsersFacade).RegisterRoutes(authed)
	favoritesrest.NewHandler(deps.FavoritesFacade).RegisterRoutes(authed)
	// Guest data-processing consent + notification opt-out. Authenticated and
	// scoped to the caller's OWN user id (no restaurant/RBAC gate) — same
	// own-user pattern as /users/me and /favorites.
	consentrest.NewHandler(deps.ConsentFacade).RegisterRoutes(authed)
	// "Which restaurants am I staff of" — the admin-panel post-login picker.
	// Authenticated but NOT restaurant-scoped, so it mounts on the plain authed
	// group (no RequireRestaurantManager gate); the usecase returns only the
	// caller's own memberships (a superadmin gets every venue).
	myrestaurantsrest.NewHandler(deps.MyRestaurants).RegisterRoutes(authed)
	// Web-push subscription register/unregister. Authenticated but NOT
	// restaurant-scoped by the router: a staff member manages their OWN devices,
	// and the usecase authorizes a registration against the caller's staff
	// membership of the target restaurant (superadmin bypasses). The endpoint
	// carries no restaurant id in the path, so RequireRestaurantManager cannot
	// gate it — same pattern as the booking/review staff routes.
	pushsubscriptionsrest.NewHandler(deps.PushSubscriptions).RegisterRoutes(authed)
	// Guest MOBILE push tokens (Expo/React Native app). Same own-user pattern as
	// /users/me: the account is the authorization, so no restaurant gate — a
	// guest registers the device they are signed in on and is notified about
	// their own bookings only.
	devicetokensrest.NewHandler(deps.DeviceTokens).RegisterRoutes(authed)

	menuHandler := menurest.NewHandler(deps.MenuFacade)
	menuHandler.RegisterPublic(api)

	// Restaurant stories — Instagram-highlight-style promo cards a venue pins to
	// its storefront. Public read only (no auth), same plain public group as the
	// menu GET: nothing here is personalized, so the user lookup an OptionalAuth
	// group costs would buy nothing. The curating venue cabinet is a later task.
	storiesHandler := storiesrest.NewHandler(deps.StoriesFacade)
	storiesHandler.RegisterPublic(api)

	// Reviews & ratings. Public: a restaurant's published reviews + aggregate
	// rating (no auth). Guest own-review + staff reply/moderation mount on the
	// authenticated group — the staff RBAC check (PermStaffManage at the
	// review's own restaurant) is resolved inside usecase/reviews, so these
	// routes need no RequireRestaurantManager gate (the review id, not a
	// restaurant id, identifies the staff-action target).
	reviewsHandler := reviewsrest.NewHandler(deps.ReviewsFacade)
	reviewsHandler.RegisterPublic(api)
	reviewsHandler.RegisterGuestRoutes(authed)
	reviewsHandler.RegisterStaffRoutes(authed)

	// Events & promos (Ф2). Public: a restaurant's published upcoming events +
	// one event, and its active promos (no auth, localized). Admin CRUD and the
	// content-draft review queue mount on the authenticated group — the RBAC
	// gate (PermRestaurantManage at the entity's own restaurant) is resolved
	// inside the usecase, same reason reviews' staff routes need no
	// RequireRestaurantManager gate (the entity id, not a restaurant id,
	// identifies the target).
	eventsHandler := eventsrest.NewHandler(deps.EventsFacade)
	eventsHandler.RegisterPublic(api)
	eventsHandler.RegisterAdminRoutes(authed)

	promosHandler := promosrest.NewHandler(deps.PromosFacade)
	promosHandler.RegisterPublic(api)
	promosHandler.RegisterAdminRoutes(authed)

	// Gastroguide — the home screen's editorial collections. Plain public group,
	// NOT OptionalAuth: unlike the feed, nothing here is personalized, so the
	// user lookup would only cost a query. Guest reads only; the editor cabinet
	// that fills these collections is a separate task.
	gastroguiderest.NewHandler(deps.GastroguideFacade).RegisterPublic(api)

	contentrest.NewHandler(deps.ContentFacade).RegisterStaffRoutes(authed)

	// Venue side of the feed: submit an item for the main screen and see where
	// the submission stands. Item-scoped (the path carries a promo/event id, not
	// a restaurant id), so the PermRestaurantManage gate is resolved inside
	// usecase/feed — same reason the events/promos admin routes need no
	// RequireRestaurantManager here.
	feedHandler.RegisterVenueRoutes(authed)

	bookingHandler := bookingsrest.NewHandler(deps.BookingsFacade, deps.BookingCreate,
		deps.BookingIdempotent, deps.BookingStatus, deps.BookingUpdate,
		deps.BookingAvail, deps.BookingBlacklist, deps.BookingPolicy, deps.BookingExternal,
		deps.BookingOverrides)
	// The availability calendar is public — the storefront needs it before login.
	bookingHandler.RegisterPublic(api)
	// Booking-scoped routes carry a booking id, not a restaurant id, so
	// RequireRestaurantManager cannot gate them: the guest/manager/admin split is
	// resolved inside the usecases from the booking itself.
	bookingHandler.RegisterRoutes(authed)

	// Booking pre-order (roadmap #1): read/replace a booking's pre-ordered menu
	// items. Booking-scoped (/bookings/:id/preorder), so authorized inside
	// usecase/preorder (owner or venue staff), not via RequireRestaurantManager.
	preorderrest.NewHandler(deps.Preorder).RegisterRoutes(authed)

	// Global admin-only routes (no single-restaurant scope).
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	restHandler.RegisterAdminGlobal(adminGlobal)
	menuHandler.RegisterAdmin(adminGlobal)

	// Global role management. This is the endpoint that hands out the rights to
	// every other admin endpoint, so the usecase checks the caller's role again
	// on top of this group's gate.
	rolesrest.NewHandler(deps.Roles).RegisterAdmin(adminGlobal)
	// Superadmin platform dashboard (Ф1): read-only, platform-wide aggregate
	// statistics. Mounted on the RequireRole(RoleAdmin) group so ONLY the global
	// superadmin passes; a restaurant owner/manager/hostess or a guest gets 403.
	// The usecase re-checks the superadmin role as defense-in-depth.
	dashboardrest.NewHandler(deps.Dashboard).RegisterRoutes(adminGlobal)

	// Platform side of the feed: the moderation queue, approve/reject, and the
	// paid-placement dial. Superadmin ONLY — mounted on the RequireRole(RoleAdmin)
	// group so a venue owner cannot reach it, and re-checked in usecase/feed as
	// defense-in-depth. A venue must never be able to price its own placement.
	feedHandler.RegisterPlatformRoutes(adminGlobal)

	// Gastroguide editor cabinet: create/edit/publish collections, manage the
	// rubrics, attach/detach and reorder venues. Superadmin ONLY, mounted on the
	// RequireRole(RoleAdmin) group for the same reason the feed's platform side
	// is — the guide is the PLATFORM's editorial opinion about which venues are
	// worth eating at, and a restaurant owner who could reach it could put their
	// own venue into "лучшие завтраки". The usecase re-checks the role.
	gastroguiderest.NewEditorHandler(deps.GastroguideEditor).RegisterAdminRoutes(adminGlobal)

	// Restaurant payouts (выплаты заведениям). The money-OUT routes (generate +
	// send) are mounted on the superadmin group; the venue-scoped routes
	// (set/read destination, read statement) are mounted on the plain authed
	// group below, same choice as the staff-roster routes — the usecase resolves
	// the owner/manager (restaurant.manage) gate per (actor, restaurant).
	payoutHandler := payoutsrest.NewHandler(deps.Payouts)
	payoutHandler.RegisterSuperadminRoutes(adminGlobal)
	payoutHandler.RegisterStaffRoutes(authed)

	// Staff-roster management (list/assign/set role/remove a restaurant's own
	// manager/hostess accounts): NOT gated by RequireRole/RequireRestaurantManager
	// here — the RBAC matrix (who may manage which restaurant's staff) is
	// resolved entirely inside usecase/restaurants.ManagerUseCase, which needs
	// to look the target restaurant up per-call anyway (SetRole/Remove resolve
	// it from the manager row itself, not the URL). Any authenticated caller
	// may reach the handler; the usecase returns ErrForbidden for anyone who
	// isn't that restaurant's own owner or a superadmin.
	restHandler.RegisterStaffRoutes(authed)

	// Restaurant-scoped mutations: admin OR the restaurant's own manager.
	// Every route under /restaurants/:… uses the ":id" param (gin forbids mixing
	// ":id" and ":restaurantId" at the same position), so both gates read "id".
	restScoped := authed.Group("")
	restScoped.Use(middleware.RequireRestaurantManager(deps.RestaurantManagers, "id"))
	restHandler.RegisterRestaurantScoped(restScoped)

	menuScoped := authed.Group("")
	menuScoped.Use(middleware.RequireRestaurantManager(deps.RestaurantManagers, "id"))
	menuHandler.RegisterScoped(menuScoped)

	// A venue's own dashboard. Same gate as every other venue screen: the
	// numbers are the venue's, so the caller must manage it.
	venueDashScoped := authed.Group("")
	venueDashScoped.Use(middleware.RequireRestaurantManager(deps.RestaurantManagers, "id"))
	venuedashboardrest.NewHandler(deps.VenueDashboard, deps.VenueToday).RegisterScoped(venueDashScoped)

	// Venue cabinet: the calendar, manual bookings and the guest stop list.
	bookingScoped := authed.Group("")
	bookingScoped.Use(middleware.RequireRestaurantManager(deps.RestaurantManagers, "id"))
	bookingHandler.RegisterRestaurantScoped(bookingScoped)

	// Restaurant admin panel (/admin/restaurants/:id/…): profile, menu,
	// stop-list, schedule, bookings, guests. Mounted behind
	// RequireRestaurantManager as defense-in-depth (a non-staff caller never
	// reaches a handler); the fine-grained owner/manager/hostess gate lives in
	// usecase/admin's RBAC matrix (e.g. a hostess may run the stop list but not
	// edit the menu or the profile).
	adminScoped := authed.Group("")
	adminScoped.Use(middleware.RequireRestaurantManager(deps.RestaurantManagers, "id"))
	adminrest.NewHandler(deps.AdminPanel).RegisterRoutes(adminScoped)

	paymentHandler := paymentsrest.NewHandler(deps.PaymentCreate, deps.PaymentCapture, deps.PaymentVoid,
		deps.PaymentRefund, deps.PaymentWebhook, deps.PaymentStatus, deps.PaymentGateways, deps.PaymentsPublicBaseURL)
	// Guest checkout + read + settle: a guest may have no account at all (a
	// payment link opened without ever logging in), so this group runs
	// OptionalAuth, not Auth — see the payments package doc.
	paymentGuest := api.Group("")
	paymentGuest.Use(middleware.OptionalAuth(deps.Issuer, deps.UsersRepo))
	paymentHandler.RegisterGuestRoutes(paymentGuest)
	// Capture/void are venue-only actions on a booking-scoped route (no
	// restaurant id in the path, same reason bookingHandler.RegisterRoutes
	// cannot use RequireRestaurantManager); mounted on the standard
	// authenticated group, restaurant ownership is checked inside the usecase.
	paymentHandler.RegisterStaffRoutes(authed)
	// Acquirer webhooks: public, unauthenticated, NOT under /api/v1 (the
	// acquirer calls the bare route it was configured with).
	paymentHandler.RegisterWebhooks(r.Group("/"))

	// Event ticketing (roadmap #2). Guest buy/view/cancel run on the same
	// OptionalAuth group as the payments checkout (a buyer may have no account);
	// admin "tickets sold" runs on the authed group (usecase RBAC). Ticket
	// payment webhooks reuse the payments webhook routes registered above.
	ticketHandler := ticketsrest.NewHandler(deps.TicketPurchase, deps.TicketRefund, deps.TicketAdmin, deps.MyTickets, deps.PaymentsPublicBaseURL)
	ticketHandler.RegisterGuestRoutes(paymentGuest)
	ticketHandler.RegisterAdminRoutes(authed)

	return r
}

// Run loads config, connects the DB, wires deps, and serves HTTP with graceful
// shutdown on SIGINT/SIGTERM.
func Run(cfg Config, log *slog.Logger) error {
	db, err := NewDB(cfg.DB.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()

	deps, err := NewDeps(cfg, db, log)
	if err != nil {
		return err
	}
	app := NewApp(cfg, deps, db, log)

	srv := &http.Server{
		Addr:              cfg.App.URL,
		Handler:           app,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server starting", slog.String("addr", cfg.App.URL))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	log.Info("server stopped gracefully")
	return nil
}
