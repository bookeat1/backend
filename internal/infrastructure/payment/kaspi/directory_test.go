package kaspi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// The directory read is not a money path, but it is the input to the setting
// that decides whose till a guest's money lands in. So: the id must survive the
// JSON number → string trip intact (it becomes account_ref), a service that is
// down must be an unavailable, and no credential may ever come back out in an
// error.

func directoryServer(t *testing.T, handler http.HandlerFunc) (*Directory, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir, err := NewDirectory(DirectoryConfig{
		BaseURL:           srv.URL,
		BasicAuthUser:     "bookeat-backend",
		BasicAuthPassword: "s3cr3t-password",
		AdminToken:        "s3cr3t-admin-token",
		Timeout:           2 * time.Second,
	}, srv.Client())
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	return dir, srv
}

func TestDirectoryListCompanies(t *testing.T) {
	var gotPath, gotAuth, gotAdmin string
	dir, _ := directoryServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAdmin = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Admin-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":2,"name":"ИП САРКУЛИН ДАМИР","status":"active","org_name":"ИП САРКУЛИН ДАМИР",
			 "has_active_session":true,"active_cashiers":1,"last_session_ok_at":"2026-08-27T14:36:52.149Z"},
			{"id":3,"name":"ТОО «Без сессии»","status":"pending","org_name":null,
			 "has_active_session":false,"active_cashiers":0,"last_session_ok_at":null}
		]}`))
	})

	companies, err := dir.ListCompanies(context.Background())
	if err != nil {
		t.Fatalf("ListCompanies: %v", err)
	}
	if gotPath != "/api/companies" {
		t.Errorf("path = %q, want /api/companies", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("basic auth was not sent: %q", gotAuth)
	}
	if gotAdmin != "s3cr3t-admin-token" {
		t.Errorf("X-Admin-Token = %q, want the configured token", gotAdmin)
	}
	if len(companies) != 2 {
		t.Fatalf("got %d companies, want 2", len(companies))
	}
	// The service answers a NUMBER; account_ref is a string, and "2" is the
	// exact value the venue mapping already holds.
	if companies[0].ID != "2" {
		t.Errorf("id = %q, want \"2\"", companies[0].ID)
	}
	if !companies[0].HasActiveSession || companies[0].ActiveCashiers != 1 {
		t.Errorf("readiness = %+v, want a live session", companies[0])
	}
	if companies[0].LastSessionOKAt == nil {
		t.Error("last_session_ok_at was dropped")
	}
	if companies[1].HasActiveSession || companies[1].LastSessionOKAt != nil {
		t.Errorf("second company invented a session: %+v", companies[1])
	}
}

func TestDirectorySkipsCompaniesWithoutAnID(t *testing.T) {
	dir, _ := directoryServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"Безымянная","status":"pending"},{"id":7,"name":"ТОО"}]}`))
	})
	companies, err := dir.ListCompanies(context.Background())
	if err != nil {
		t.Fatalf("ListCompanies: %v", err)
	}
	// A company we cannot address must not reach a picker: binding a venue to
	// an empty account_ref is refused downstream, so offering it only invites
	// the attempt.
	if len(companies) != 1 || companies[0].ID != "7" {
		t.Fatalf("got %+v, want only the addressable company", companies)
	}
}

func TestDirectoryFailuresAreUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"service 500", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"Internal server error"}`))
		}},
		{"credentials rejected", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid or missing X-Admin-Token"}`))
		}},
		{"malformed answer", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<!doctype html><html>Caddy says hello`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := directoryServer(t, tc.handler)
			companies, err := dir.ListCompanies(context.Background())
			if !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("err = %v, want domain.ErrUnavailable", err)
			}
			if companies != nil {
				t.Errorf("a failed read returned %v, want nothing", companies)
			}
			// Never echo what we sent: a 401 from Caddy can repeat it.
			if msg := err.Error(); strings.Contains(msg, "s3cr3t") {
				t.Errorf("error leaked a credential: %s", msg)
			}
		})
	}
}

func TestDirectoryUnreachableService(t *testing.T) {
	dir, srv := directoryServer(t, func(http.ResponseWriter, *http.Request) {})
	srv.Close() // nobody is listening any more

	_, err := dir.ListCompanies(context.Background())
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want domain.ErrUnavailable", err)
	}
}

func TestDirectoryTimesOut(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	dir, err := NewDirectory(DirectoryConfig{BaseURL: srv.URL, Timeout: 50 * time.Millisecond}, srv.Client())
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	start := time.Now()
	if _, err := dir.ListCompanies(context.Background()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want domain.ErrUnavailable", err)
	}
	// The panel must not hang on a service that never answers.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s, the configured budget was 50ms", elapsed)
	}
}

func TestDirectoryConfigFromEnv(t *testing.T) {
	t.Setenv("KASPI_API_URL", "https://kaspi.example.test/")
	t.Setenv("KASPI_BASIC_AUTH_USER", "bookeat-backend")
	t.Setenv("KASPI_BASIC_AUTH_PASSWORD", "pw")
	t.Setenv("KASPI_ADMIN_TOKEN", "tok")
	t.Setenv("KASPI_DIRECTORY_TIMEOUT", "3")

	cfg := DirectoryConfigFromEnv()
	if cfg.BaseURL != "https://kaspi.example.test/" || cfg.AdminToken != "tok" {
		t.Fatalf("config = %+v", cfg)
	}
	// A bare number is read as seconds, the friendlier of the two shapes an
	// operator might type.
	if cfg.Timeout != 3*time.Second {
		t.Errorf("timeout = %s, want 3s", cfg.Timeout)
	}

	t.Setenv("KASPI_DIRECTORY_TIMEOUT", "700ms")
	if got := DirectoryConfigFromEnv().Timeout; got != 700*time.Millisecond {
		t.Errorf("timeout = %s, want 700ms", got)
	}

	t.Setenv("KASPI_DIRECTORY_TIMEOUT", "nonsense")
	if got := DirectoryConfigFromEnv().Timeout; got != defaultDirectoryTimeout {
		t.Errorf("timeout = %s, want the default on an unreadable value", got)
	}

	// The trailing slash must not become "//api/companies".
	dir, err := NewDirectory(DirectoryConfigFromEnv(), http.DefaultClient)
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	if dir.cfg.BaseURL != "https://kaspi.example.test" {
		t.Errorf("base URL = %q, want it without the trailing slash", dir.cfg.BaseURL)
	}
}

func TestDirectoryNeedsABaseURL(t *testing.T) {
	if _, err := NewDirectory(DirectoryConfig{BaseURL: "  "}, http.DefaultClient); err == nil {
		t.Fatal("an empty base URL was accepted")
	}
}
