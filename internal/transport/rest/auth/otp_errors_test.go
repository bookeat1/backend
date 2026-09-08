package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/auth"
)

// What the wire must look like for each OTP failure. The usecase tests fix
// WHICH error each situation produces; this file fixes what a client actually
// receives: the status (unchanged), the human message (unchanged), the
// machine-readable code, and the Retry-After header on the rate limits.

type stubOTP struct{ err error }

func (s *stubOTP) RequestOTP(context.Context, string) (string, error) { return "", s.err }
func (s *stubOTP) VerifyOTP(context.Context, string, string) (*uc.TokenPair, error) {
	return nil, s.err
}
func (s *stubOTP) RequestPhoneChangeOTP(context.Context, uuid.UUID, string) (string, error) {
	return "", s.err
}
func (s *stubOTP) VerifyPhoneChange(context.Context, uuid.UUID, string, string) (*domain.User, error) {
	return nil, s.err
}

// stubFacade satisfies the handler's other dependency; none of these routes are
// exercised here.
type stubFacade struct{}

func (stubFacade) Signup(context.Context, string, string, string) (*uc.TokenPair, error) {
	return nil, domain.ErrForbidden
}
func (stubFacade) Login(context.Context, string, string) (*uc.TokenPair, error) {
	return nil, domain.ErrForbidden
}
func (stubFacade) Refresh(context.Context, string) (*uc.TokenPair, error) {
	return nil, domain.ErrForbidden
}
func (stubFacade) Logout(context.Context, string) error { return nil }

func newOTPRouter(err error) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(stubFacade{}, &stubOTP{err: err}).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func postJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestOTPErrorEnvelopes(t *testing.T) {
	tests := []struct {
		name            string
		path, body      string
		err             error
		wantStatus      int
		wantMessage     string
		wantCode        domain.ErrorCode
		wantRetryAfter  string
		requestOTPRoute bool
	}{
		{
			name: "malformed phone", path: "/api/v1/auth/otp/request", body: `{"phone":"нет"}`,
			err: domain.WithCode(domain.CodeOTPInvalidPhone,
				fmt.Errorf("%w: phone required", domain.ErrValidation)),
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "validation failed",
			wantCode: domain.CodeOTPInvalidPhone,
		},
		{
			name: "per-minute limit carries Retry-After",
			path: "/api/v1/auth/otp/request", body: `{"phone":"+77070000001"}`,
			err: domain.WithCode(domain.CodeOTPRateLimitedMinute,
				domain.WithRetryAfter(time.Minute,
					fmt.Errorf("%w: too many requests, wait a minute", domain.ErrValidation))),
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "validation failed",
			wantCode: domain.CodeOTPRateLimitedMinute, wantRetryAfter: "60",
		},
		{
			name: "hourly limit carries Retry-After",
			path: "/api/v1/auth/otp/request", body: `{"phone":"+77070000001"}`,
			err: domain.WithCode(domain.CodeOTPRateLimitedHour,
				domain.WithRetryAfter(time.Hour,
					fmt.Errorf("%w: hourly OTP limit reached", domain.ErrValidation))),
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "validation failed",
			wantCode: domain.CodeOTPRateLimitedHour, wantRetryAfter: "3600",
		},
		{
			name: "code not accepted", path: "/api/v1/auth/otp/verify",
			body: `{"phone":"+77070000001","code":"000000"}`,
			err:  domain.WithCode(domain.CodeOTPInvalid, domain.ErrUnauthorized),
			// The status and the message are exactly what they were before the
			// codes existed — no client that reads them breaks.
			wantStatus: http.StatusUnauthorized, wantMessage: "unauthorized",
			wantCode: domain.CodeOTPInvalid,
		},
		{
			name: "locked out", path: "/api/v1/auth/otp/verify",
			body:       `{"phone":"+77070000001","code":"000000"}`,
			err:        domain.WithCode(domain.CodeOTPTooManyAttempts, domain.ErrUnauthorized),
			wantStatus: http.StatusUnauthorized, wantMessage: "unauthorized",
			wantCode: domain.CodeOTPTooManyAttempts,
		},
		{
			name: "no code submitted", path: "/api/v1/auth/otp/verify",
			body: `{"phone":"+77070000001","code":"x"}`,
			err: domain.WithCode(domain.CodeOTPCodeRequired,
				fmt.Errorf("%w: phone and code required", domain.ErrValidation)),
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "validation failed",
			wantCode: domain.CodeOTPCodeRequired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := postJSON(t, newOTPRouter(tc.err), tc.path, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			var env struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if env.Error != tc.wantMessage {
				t.Errorf("error = %q, want %q (the human text is not allowed to change)",
					env.Error, tc.wantMessage)
			}
			if env.Code != string(tc.wantCode) {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
		})
	}
}

// Two different failures must not produce identical bodies, or the app is back
// where it started. This is the same guard the booking 409s carry.
func TestOTPVerifyFailuresAreDistinguishableOnTheWire(t *testing.T) {
	invalid := postJSON(t, newOTPRouter(domain.WithCode(domain.CodeOTPInvalid, domain.ErrUnauthorized)),
		"/api/v1/auth/otp/verify", `{"phone":"+77070000001","code":"000000"}`)
	locked := postJSON(t, newOTPRouter(domain.WithCode(domain.CodeOTPTooManyAttempts, domain.ErrUnauthorized)),
		"/api/v1/auth/otp/verify", `{"phone":"+77070000001","code":"000000"}`)

	if invalid.Code != locked.Code {
		t.Fatalf("statuses drifted: %d vs %d — both must stay 401", invalid.Code, locked.Code)
	}
	if invalid.Body.String() == locked.Body.String() {
		t.Errorf("a rejected code and a locked-out number answer identically: %s", invalid.Body.String())
	}
}
