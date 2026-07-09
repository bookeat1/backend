package bootstrap

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config is the whole application configuration, built from environment
// variables. Grow it with new sections (Redis, external services, …) as the
// domain requires — one struct per concern, wired in NewConfig.
type Config struct {
	App AppConfig
	DB  DBConfig
}

type AppConfig struct {
	Name        string
	Environment string
	URL         string
	LogLevel    string
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

	cfg := Config{
		App: AppConfig{
			Name:        getEnv("APP_NAME", "backend-core"),
			Environment: getEnv("APP_ENV", "development"),
			URL:         getEnv("APP_URL", "0.0.0.0:8080"),
			LogLevel:    getEnv("APP_LOG_LEVEL", "info"),
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
