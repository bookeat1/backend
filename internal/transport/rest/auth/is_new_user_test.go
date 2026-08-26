package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/auth"
)

// The wire contract of the "was this account just created" signal.
//
// The app shows a birth-date onboarding step to genuinely new guests. It used
// to guess that from an empty full name, which dragged a months-old account
// that never filled one in back through registration. The server knows the
// truth (the OTP login is the only find-or-create path), so it says it — on
// that endpoint and nowhere else.

// pairOTP answers every VerifyOTP with the same fixed pair.
type pairOTP struct{ pair *uc.TokenPair }

func (p *pairOTP) RequestOTP(context.Context, string) (string, error) { return "", nil }
func (p *pairOTP) VerifyOTP(context.Context, string, string) (*uc.TokenPair, error) {
	return p.pair, nil
}
func (p *pairOTP) RequestPhoneChangeOTP(context.Context, uuid.UUID, string) (string, error) {
	return "", nil
}
func (p *pairOTP) VerifyPhoneChange(context.Context, uuid.UUID, string, string) (*domain.User, error) {
	return nil, domain.ErrForbidden
}

// pairFacade answers Refresh with a pair, so the refresh route can be exercised.
type pairFacade struct{ pair *uc.TokenPair }

func (pairFacade) Signup(context.Context, string, string, string) (*uc.TokenPair, error) {
	return nil, domain.ErrForbidden
}
func (pairFacade) Login(context.Context, string, string) (*uc.TokenPair, error) {
	return nil, domain.ErrForbidden
}
func (f pairFacade) Refresh(context.Context, string) (*uc.TokenPair, error) { return f.pair, nil }
func (pairFacade) Logout(context.Context, string) error                     { return nil }

func newPairRouter(pair *uc.TokenPair) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(pairFacade{pair: pair}, &pairOTP{pair: pair}).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func decodeData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	return env.Data
}

func TestOTPVerifyReportsIsNewUser(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "account created by this very verify", want: true},
		{name: "returning guest", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := &uc.TokenPair{
				AccessToken: "a", RefreshToken: "r",
				ExpiresAt: time.Now().Add(time.Hour), IsNewUser: tc.want,
			}
			w := postJSON(t, newPairRouter(pair), "/api/v1/auth/otp/verify",
				`{"phone":"+77070000001","code":"123456"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			data := decodeData(t, w.Body.Bytes())
			// The pair itself must still be there, unchanged: the flag is an
			// addition, not a reshaping.
			if data["access_token"] != "a" || data["refresh_token"] != "r" {
				t.Fatalf("token pair not carried through: %v", data)
			}
			got, ok := data["is_new_user"]
			if !ok {
				t.Fatalf("is_new_user missing from verify response: %v", data)
			}
			if got != tc.want {
				t.Errorf("is_new_user = %v, want %v", got, tc.want)
			}
		})
	}
}

// A refresh creates nobody. Answering it with is_new_user at all — even false —
// would invite the app to read a re-login as evidence about the account, so the
// key is absent from that response entirely.
func TestRefreshDoesNotClaimNewUser(t *testing.T) {
	pair := &uc.TokenPair{
		AccessToken: "a", RefreshToken: "r",
		ExpiresAt: time.Now().Add(time.Hour), IsNewUser: true, // even if it leaked in
	}
	w := postJSON(t, newPairRouter(pair), "/api/v1/auth/refresh", `{"refresh_token":"r"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if _, ok := decodeData(t, w.Body.Bytes())["is_new_user"]; ok {
		t.Error("refresh response must not carry is_new_user")
	}
}
