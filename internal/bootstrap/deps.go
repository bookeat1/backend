package bootstrap

import (
	"database/sql"
	"fmt"
	"log/slog"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/otpsender"
	otprepo "backend-core/internal/infrastructure/postgres/otp"
	rtrepo "backend-core/internal/infrastructure/postgres/refreshtoken"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	credrepo "backend-core/internal/infrastructure/postgres/usercredential"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/usecase/auth"
	"backend-core/internal/usecase/users"
)

// Deps holds the constructed usecases and shared infrastructure.
type Deps struct {
	AuthService *auth.Service
	UsersFacade *users.Facade
	UsersRepo   domain.UserRepository
	Issuer      *token.RSAIssuer
}

// NewDeps constructs repositories, infrastructure clients, and usecases.
func NewDeps(cfg Config, db *sql.DB, log *slog.Logger) (*Deps, error) {
	issuer, err := token.NewRSAIssuer(cfg.Auth.JWTPrivateKeyPEM, cfg.Auth.JWTKeyID, cfg.Auth.AccessTokenTTL)
	if err != nil {
		return nil, fmt.Errorf("build token issuer: %w", err)
	}
	txm := sqltx.NewManager(db)

	usersRepo := userrepo.New(db)
	credsRepo := credrepo.New(db)
	refreshRepo := rtrepo.New(db)
	otpRepo := otprepo.New(db)

	authSvc := auth.NewService(auth.Deps{
		Users:       usersRepo,
		Credentials: credsRepo,
		Refresh:     refreshRepo,
		OTP:         otpRepo,
		Tx:          txm,
		Tokens:      issuer,
		OTPSender:   otpsender.NewStub(log),
		Config: auth.Config{
			RefreshTTL:   cfg.Auth.RefreshTokenTTL,
			OTPTTL:       cfg.Auth.OTPCodeTTL,
			OTPPerMin:    cfg.Auth.OTPRateLimitPerMin,
			OTPPerHour:   cfg.Auth.OTPRateLimitPerHour,
			OTPDevExpose: cfg.Auth.OTPDevExpose,
		},
	})

	return &Deps{
		AuthService: authSvc,
		UsersFacade: users.NewFacade(usersRepo),
		UsersRepo:   usersRepo,
		Issuer:      issuer,
	}, nil
}
