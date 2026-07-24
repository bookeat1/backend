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
	contentrest "backend-core/internal/transport/rest/content"
	eventsrest "backend-core/internal/transport/rest/events"
	favoritesrest "backend-core/internal/transport/rest/favorites"
	menurest "backend-core/internal/transport/rest/menu"
	"backend-core/internal/transport/rest/middleware"
	myrestaurantsrest "backend-core/internal/transport/rest/myrestaurants"
	paymentsrest "backend-core/internal/transport/rest/payments"
	promosrest "backend-core/internal/transport/rest/promos"
	restrest "backend-core/internal/transport/rest/restaurants"
	reviewsrest "backend-core/internal/transport/rest/reviews"
	"backend-core/internal/transport/rest/swaggerui"
	usersrest "backend-core/internal/transport/rest/users"
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

	authed := api.Group("")
	authed.Use(middleware.Auth(deps.Issuer, deps.UsersRepo))
	authed.Use(middleware.LogUserContext())
	usersrest.NewHandler(deps.UsersFacade).RegisterRoutes(authed)
	favoritesrest.NewHandler(deps.FavoritesFacade).RegisterRoutes(authed)
	// "Which restaurants am I staff of" — the admin-panel post-login picker.
	// Authenticated but NOT restaurant-scoped, so it mounts on the plain authed
	// group (no RequireRestaurantManager gate); the usecase returns only the
	// caller's own memberships (a superadmin gets every venue).
	myrestaurantsrest.NewHandler(deps.MyRestaurants).RegisterRoutes(authed)

	menuHandler := menurest.NewHandler(deps.MenuFacade)
	menuHandler.RegisterPublic(api)

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

	contentrest.NewHandler(deps.ContentFacade).RegisterStaffRoutes(authed)

	bookingHandler := bookingsrest.NewHandler(deps.BookingsFacade, deps.BookingCreate,
		deps.BookingIdempotent, deps.BookingStatus, deps.BookingUpdate,
		deps.BookingAvail, deps.BookingBlacklist, deps.BookingPolicy, deps.BookingExternal)
	// The availability calendar is public — the storefront needs it before login.
	bookingHandler.RegisterPublic(api)
	// Booking-scoped routes carry a booking id, not a restaurant id, so
	// RequireRestaurantManager cannot gate them: the guest/manager/admin split is
	// resolved inside the usecases from the booking itself.
	bookingHandler.RegisterRoutes(authed)

	// Global admin-only routes (no single-restaurant scope).
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	restHandler.RegisterAdminGlobal(adminGlobal)
	menuHandler.RegisterAdmin(adminGlobal)

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
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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
