package bootstrap

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	authrest "backend-core/internal/transport/rest/auth"
	"backend-core/internal/transport/rest/middleware"
	usersrest "backend-core/internal/transport/rest/users"
)

// NewApp builds the Gin engine with all routes wired.
func NewApp(cfg Config, deps *Deps, log *slog.Logger) *gin.Engine {
	// response.HandleError logs failures via slog.Default(); point it at the
	// configured logger so those logs use the app's handler and level.
	slog.SetDefault(log)
	if cfg.App.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}}) })
	r.GET("/.well-known/jwks.json", func(c *gin.Context) { c.JSON(http.StatusOK, deps.Issuer.JWKS()) })

	api := r.Group("/api/v1")
	authrest.NewHandler(deps.AuthFacade, deps.AuthOTP).RegisterRoutes(api)

	authed := api.Group("")
	authed.Use(middleware.Auth(deps.Issuer, deps.UsersRepo))
	usersrest.NewHandler(deps.UsersFacade).RegisterRoutes(authed)

	return r
}

// Run loads config, connects the DB, wires deps, and serves HTTP.
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
	app := NewApp(cfg, deps, log)
	log.Info("http server starting", slog.String("addr", cfg.App.URL))
	return app.Run(cfg.App.URL)
}
