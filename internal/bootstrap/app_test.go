package bootstrap

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/token/tokentest"
	"backend-core/internal/logger"
)

// buildTestApp wires a real app against the test DB with a fresh signing key and
// OTP dev-expose enabled.
func buildTestApp(t *testing.T) http.Handler {
	t.Helper()
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users", "otp_codes", "refresh_tokens")
	log := logger.New("error", "json")
	cfg := Config{}
	cfg.App.Environment = "test"
	cfg.App.CORSAllowedOrigins = []string{"*"} // NewConfig's default; set explicitly since this bypasses it

	cfg.Auth = AuthConfig{
		JWTPrivateKeyPEM:    tokentest.GenerateKeyPEM(t),
		JWTKeyID:            "test",
		AccessTokenTTL:      15 * time.Minute,
		RefreshTokenTTL:     time.Hour,
		OTPCodeTTL:          5 * time.Minute,
		OTPRateLimitPerMin:  5,
		OTPRateLimitPerHour: 20,
		OTPDevExpose:        true,
	}
	deps, err := NewDeps(cfg, db, log)
	if err != nil {
		t.Fatalf("NewDeps: %v", err)
	}
	return NewApp(cfg, deps, db, log)
}

func doJSON(t *testing.T, app http.Handler, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestSignupLoginMeFlow(t *testing.T) {
	app := buildTestApp(t)

	rec, out := doJSON(t, app, "POST", "/api/v1/auth/signup", "", map[string]string{
		"email": "e2e@bookeat.com", "password": "pw123456", "full_name": "E2E",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup status %d: %v", rec.Code, out)
	}
	data := out["data"].(map[string]any)
	access := data["access_token"].(string)

	rec, out = doJSON(t, app, "GET", "/api/v1/users/me", access, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status %d: %v", rec.Code, out)
	}
	me := out["data"].(map[string]any)
	if me["email"] != "e2e@bookeat.com" || me["full_name"] != "E2E" {
		t.Errorf("unexpected /me: %v", me)
	}

	// Unauthenticated /me is rejected.
	rec, _ = doJSON(t, app, "GET", "/api/v1/users/me", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", rec.Code)
	}
}

func TestHealthReadyAndCORSPreflight(t *testing.T) {
	app := buildTestApp(t)

	// Readiness probe pings the DB and reports ready.
	req := httptest.NewRequest("GET", "/health/ready", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness status %d", rec.Code)
	}

	// A CORS preflight is answered with 204 and permissive origin (default "*").
	req = httptest.NewRequest("OPTIONS", "/api/v1/auth/login", nil)
	req.Header.Set("Origin", "https://app.bookeat.com")
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
}

func TestOTPFlow(t *testing.T) {
	app := buildTestApp(t)

	rec, out := doJSON(t, app, "POST", "/api/v1/auth/otp/request", "", map[string]string{"phone": "8 701 555 0000"})
	if rec.Code != http.StatusOK {
		t.Fatalf("otp request status %d: %v", rec.Code, out)
	}
	code := out["data"].(map[string]any)["code"].(string)

	rec, out = doJSON(t, app, "POST", "/api/v1/auth/otp/verify", "", map[string]string{
		"phone": "+7 701 555 0000", "code": code,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("otp verify status %d: %v", rec.Code, out)
	}
	if out["data"].(map[string]any)["access_token"].(string) == "" {
		t.Error("expected access token from otp verify")
	}
}
