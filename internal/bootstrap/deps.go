package bootstrap

import (
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/otpsender"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	idemrepo "backend-core/internal/infrastructure/postgres/idempotency"
	menurepo "backend-core/internal/infrastructure/postgres/menu"
	otprepo "backend-core/internal/infrastructure/postgres/otp"
	rtrepo "backend-core/internal/infrastructure/postgres/refreshtoken"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	credrepo "backend-core/internal/infrastructure/postgres/usercredential"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/usecase/auth"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/restaurants"
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
	MenuFacade         menu.Facade
	BookingsFacade     bookings.Facade
	BookingCreate      bookings.CreateUseCase
	BookingIdempotent  bookings.IdempotentCreateUseCase
	BookingStatus      bookings.StatusUseCase
	BookingUpdate      bookings.UpdateUseCase
	BookingAvail       bookings.AvailabilityUseCase
	BookingBlacklist   bookings.BlacklistUseCase
	Issuer             *token.RSAIssuer
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

	authCfg := auth.Config{
		RefreshTTL:   cfg.Auth.RefreshTokenTTL,
		OTPTTL:       cfg.Auth.OTPCodeTTL,
		OTPPerMin:    cfg.Auth.OTPRateLimitPerMin,
		OTPPerHour:   cfg.Auth.OTPRateLimitPerHour,
		OTPDevExpose: cfg.Auth.OTPDevExpose,
	}
	authFacade := auth.NewFacade(usersRepo, credsRepo, refreshRepo, txm, issuer, authCfg)
	authOTP := auth.NewOTPUseCase(usersRepo, otpRepo, refreshRepo, txm, issuer, otpsender.NewStub(log), authCfg)

	restRepo := restrepo.New(db)
	restRelated := restrepo.NewRelated(db)
	restCategories := restrepo.NewCategories(db)
	restManagers := restrepo.NewManagers(db)
	restPartners := restrepo.NewPartnership(db)

	menuItems := menurepo.New(db)
	menuCategories := menurepo.NewCategories(db)

	restaurantManagers := restaurants.NewManagerUseCase(restManagers, usersRepo)

	bookingRepo := bookingrepo.New(db)
	bookingLinks := bookingrepo.NewTables(db)
	bookingItems := bookingrepo.NewItems(db)
	bookingMessages := bookingrepo.NewMessages(db)
	bookingSurveys := bookingrepo.NewSurveys(db)
	bookingHistory := bookingrepo.NewHistory(db)
	bookingOutbox := bookingrepo.NewOutbox(db)
	bookingBlacklist := bookingrepo.NewBlacklist(db)
	bookingRateLog := bookingrepo.NewRateLog(db)
	idempotencyKeys := idemrepo.New(db)

	// bookings.Config mirrors BookingConfig field-for-field so the usecase
	// layer never imports bootstrap (same arrangement as auth.Config).
	bookingCfg := bookings.Config{
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

	bookingCreate := bookings.NewCreateUseCase(bookingRepo, bookingLinks, bookingItems,
		bookingHistory, bookingOutbox, bookingBlacklist, bookingRateLog, restRepo,
		restRelated, restaurantManagers, txm, bookingCfg)

	return &Deps{
		AuthFacade:         authFacade,
		AuthOTP:            authOTP,
		UsersFacade:        users.NewFacade(usersRepo),
		UsersRepo:          usersRepo,
		RestaurantsFacade:  restaurants.NewFacade(restRepo, restRelated, restCategories, restPartners, txm),
		RestaurantManagers: restaurantManagers,
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
		Issuer:           issuer,
	}, nil
}
