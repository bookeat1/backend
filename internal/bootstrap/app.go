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
	authrest "backend-core/internal/transport/rest/auth"
	bookingsrest "backend-core/internal/transport/rest/bookings"
	menurest "backend-core/internal/transport/rest/menu"
	"backend-core/internal/transport/rest/middleware"
	restrest "backend-core/internal/transport/rest/restaurants"
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
	r.Use(gin.Recovery())
	r.Use(middleware.CORS(cfg.App.CORSAllowedOrigins))

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

	restHandler := restrest.NewHandler(deps.RestaurantsFacade, deps.RestaurantManagers)
	restHandler.RegisterPublic(api)

	authed := api.Group("")
	authed.Use(middleware.Auth(deps.Issuer, deps.UsersRepo))
	usersrest.NewHandler(deps.UsersFacade).RegisterRoutes(authed)

	menuHandler := menurest.NewHandler(deps.MenuFacade)
	menuHandler.RegisterPublic(api)

	bookingHandler := bookingsrest.NewHandler(deps.BookingsFacade, deps.BookingCreate,
		deps.BookingIdempotent, deps.BookingStatus, deps.BookingUpdate,
		deps.BookingAvail, deps.BookingBlacklist, deps.BookingPolicy)
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
