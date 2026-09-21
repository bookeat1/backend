package kwaaka

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPathPrefix is the Table Booking API's base path (confirmed in the
// spec's `servers:` block and by a live call, 2026-09-21). It is configurable
// because it is a Kwaaka-assigned value, same posture as every other adapter's
// base URL, not because it is expected to change.
const DefaultPathPrefix = "/v1/table-booking"

// DefaultService is the partner integration name Kwaaka dispatches on
// (`service` query param, enum bookeat|atlanti-ai). BookEat is always
// "bookeat".
const DefaultService = "bookeat"

// DefaultTimeout bounds one HTTP attempt. A menu fetch is a background sync,
// not a guest-facing request, so it can afford to be patient — but not
// unbounded, or a hung Kwaaka call would pile up worker goroutines.
const DefaultTimeout = 15 * time.Second

// Config is the adapter's endpoint and credential, read from the environment
// only (never the database, never a request) — same rule as every acquirer
// adapter in infrastructure/payment.
type Config struct {
	BaseURL     string        // KWAAKA_BASE_URL
	PathPrefix  string        // KWAAKA_PATH_PREFIX, defaults to DefaultPathPrefix
	Token       string        // KWAAKA_TOKEN — sent as-is in the Authorization header, no "Bearer " prefix
	Service     string        // KWAAKA_SERVICE, defaults to DefaultService
	Timeout     time.Duration // KWAAKA_HTTP_TIMEOUT, defaults to DefaultTimeout
	MaxAttempts int           // KWAAKA_HTTP_MAX_ATTEMPTS, defaults to 3 (GET is naturally idempotent)
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.PathPrefix) == "" {
		c.PathPrefix = DefaultPathPrefix
	}
	if strings.TrimSpace(c.Service) == "" {
		c.Service = DefaultService
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	return c
}

// Validate reports whether the adapter can be wired at all. bootstrap should
// skip starting the sync worker (rather than start with half a credential)
// when this fails.
func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.BaseURL) == "" {
		missing = append(missing, "KWAAKA_BASE_URL")
	}
	if strings.TrimSpace(c.Token) == "" {
		missing = append(missing, "KWAAKA_TOKEN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("kwaaka: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// ConfigFromEnv reads the adapter's configuration from the environment. It is
// the only place the Kwaaka token enters the process; bootstrap must not pass
// it through the database or a request.
func ConfigFromEnv() Config {
	return Config{
		BaseURL:     strings.TrimRight(os.Getenv("KWAAKA_BASE_URL"), "/"),
		PathPrefix:  os.Getenv("KWAAKA_PATH_PREFIX"),
		Token:       os.Getenv("KWAAKA_TOKEN"),
		Service:     os.Getenv("KWAAKA_SERVICE"),
		Timeout:     envDuration("KWAAKA_HTTP_TIMEOUT", DefaultTimeout),
		MaxAttempts: envInt("KWAAKA_HTTP_MAX_ATTEMPTS", 3),
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
