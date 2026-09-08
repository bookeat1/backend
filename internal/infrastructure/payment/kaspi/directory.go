package kaspi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// The company registry of our Kaspi service, read-only
//
// Onboarding a venue to Kaspi means writing ONE string: which company inside
// the Kaspi service that venue's money belongs to (restaurant_split_accounts.
// account_ref — see usecase/admin.SetAcquirerAccount). Until now that string
// was typed by hand from the operator's own panel on kaspi.book-eat.com, which
// is exactly the kind of copy-paste that credits the wrong till.
//
// This reads the same registry over HTTP so the admin panel can offer a list
// instead of a text field. It is READ-ONLY and it is not a money path: it
// creates nothing, and the Kaspi service answers it with identifiers and
// readiness only (no API keys, no phones, no device material). Kaspi has no
// sandbox, so nothing here may ever grow a write.
// ---------------------------------------------------------------------------

// defaultDirectoryTimeout is the whole budget for the call, not per attempt. An
// admin panel is waiting on it: a directory that has not answered in five
// seconds is better reported as "unavailable, try again" than left spinning.
const defaultDirectoryTimeout = 5 * time.Second

// maxDirectoryBytes caps what we read from the service. The registry is a
// handful of companies; anything larger is a malfunction and must not be able
// to exhaust our memory.
const maxDirectoryBytes = 1 << 20

// Company is one company in the Kaspi service's registry, as an operator needs
// to see it before pointing a venue's money at it.
type Company struct {
	// ID is what goes into restaurant_split_accounts.account_ref. The service
	// answers it as a number; it is carried as a string because that is what
	// the mapping stores and compares.
	ID     string
	Name   string
	Status string
	// OrgName is the legal entity Kaspi itself knows (ИП/ТОО), empty until the
	// company finishes onboarding.
	OrgName string
	// HasActiveSession is the fact that decides whether this company can take
	// money AT ALL right now. A cashier session evicted by Kaspi leaves the
	// company looking `active` with no way to create a payment link, and an
	// operator who binds a venue to it would only find out from a guest.
	HasActiveSession bool
	ActiveCashiers   int
	LastSessionOKAt  *time.Time
}

// DirectoryConfig is where the Kaspi service lives and how we authenticate to
// it. Credentials come from the environment only (spec §8) — never from the
// database, never from a request.
type DirectoryConfig struct {
	// BaseURL is KASPI_API_URL, defaulting to DefaultBaseURL. Shared with the
	// payment adapter on purpose: one deployment, one address.
	BaseURL string
	// BasicAuthUser / BasicAuthPassword are KASPI_BASIC_AUTH_USER and
	// KASPI_BASIC_AUTH_PASSWORD — the Caddy basic auth in front of the
	// service. Optional, like in the payment adapter.
	BasicAuthUser     string
	BasicAuthPassword string
	// AdminToken is the optional KASPI_ADMIN_TOKEN. The service requires
	// X-Admin-Token on its platform routes only when ADMIN_TOKEN is set there;
	// leaving this empty matches a deployment that relies on the basic auth.
	AdminToken string
	// Timeout is the budget for one directory read. Zero means
	// defaultDirectoryTimeout.
	Timeout time.Duration
}

// DirectoryConfigFromEnv reads the directory client's configuration.
func DirectoryConfigFromEnv() DirectoryConfig {
	return DirectoryConfig{
		BaseURL:           envOr("KASPI_API_URL", DefaultBaseURL),
		BasicAuthUser:     strings.TrimSpace(os.Getenv("KASPI_BASIC_AUTH_USER")),
		BasicAuthPassword: os.Getenv("KASPI_BASIC_AUTH_PASSWORD"),
		AdminToken:        strings.TrimSpace(os.Getenv("KASPI_ADMIN_TOKEN")),
		Timeout:           envDuration("KASPI_DIRECTORY_TIMEOUT", defaultDirectoryTimeout),
	}
}

// Validate reports whether the client can be wired. Only the address is
// required: a deployment that reaches the service on a private network needs no
// credentials, exactly like the payment adapter.
func (c DirectoryConfig) Validate() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("kaspi directory: empty base URL (KASPI_API_URL)")
	}
	return nil
}

// Directory reads the Kaspi service's company registry.
type Directory struct {
	cfg  DirectoryConfig
	http Doer
}

// Doer is the minimal http.Client surface this client needs. Satisfied by
// *http.Client and by an httptest server's client.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewDirectory builds the client. client may be nil, in which case one with the
// configured timeout is created.
func NewDirectory(cfg DirectoryConfig, client Doer) (*Directory, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultDirectoryTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Directory{cfg: cfg, http: client}, nil
}

// directoryCompany is the service's wire shape (GET /api/companies). The id is
// a JSON number there and a string here.
type directoryCompany struct {
	ID               json.Number `json:"id"`
	Name             string      `json:"name"`
	Status           string      `json:"status"`
	OrgName          string      `json:"org_name"`
	HasActiveSession bool        `json:"has_active_session"`
	ActiveCashiers   int         `json:"active_cashiers"`
	LastSessionOKAt  string      `json:"last_session_ok_at"`
}

type directoryEnvelope struct {
	Data  []directoryCompany `json:"data"`
	Error string             `json:"error"`
}

// ListCompanies returns every company in the Kaspi service's registry.
//
// Every failure mode — network, timeout, a rejected credential, an
// unparseable body — comes back wrapping domain.ErrUnavailable, i.e. HTTP 503
// with "temporarily unavailable". That is deliberate: this is a picker list,
// and NOTHING about it is the caller's fault, so nothing about it should look
// like a client error the panel would render as "wrong request". The reason is
// in the error text for the log; the credential never is.
func (d *Directory) ListCompanies(ctx context.Context) ([]Company, error) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.BaseURL+"/api/companies", nil)
	if err != nil {
		return nil, fmt.Errorf("kaspi directory: build request: %w", domain.ErrUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	if d.cfg.BasicAuthUser != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
			[]byte(d.cfg.BasicAuthUser+":"+d.cfg.BasicAuthPassword)))
	}
	if d.cfg.AdminToken != "" {
		req.Header.Set("X-Admin-Token", d.cfg.AdminToken)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		// The URL can carry no credential (the password travels in a header),
		// but the error is still not echoed to the caller verbatim anywhere
		// above this layer — it is a log line.
		return nil, fmt.Errorf("kaspi directory: %s: %w", transportReason(err), domain.ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDirectoryBytes))
	if err != nil {
		return nil, fmt.Errorf("kaspi directory: read answer: %w", domain.ErrUnavailable)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Do NOT echo the body: a 401 from Caddy may repeat what we sent.
		return nil, fmt.Errorf("kaspi directory: credentials rejected: %w", domain.ErrUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kaspi directory: service answered HTTP %d: %w", resp.StatusCode, domain.ErrUnavailable)
	}

	var env directoryEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("kaspi directory: malformed answer: %w", domain.ErrUnavailable)
	}

	out := make([]Company, 0, len(env.Data))
	for _, c := range env.Data {
		id := strings.TrimSpace(c.ID.String())
		if id == "" {
			// A company we cannot address is not offerable: binding a venue to
			// an empty account_ref is refused downstream anyway, and showing it
			// in a picker would only invite the attempt.
			continue
		}
		out = append(out, Company{
			ID:               id,
			Name:             strings.TrimSpace(c.Name),
			Status:           strings.TrimSpace(c.Status),
			OrgName:          strings.TrimSpace(c.OrgName),
			HasActiveSession: c.HasActiveSession,
			ActiveCashiers:   c.ActiveCashiers,
			LastSessionOKAt:  parseDirectoryTime(c.LastSessionOKAt),
		})
	}
	return out, nil
}

// transportReason names the kind of transport failure without repeating the
// error, which for a proxied request can contain the target URL.
func transportReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "unreachable"
	}
}

func parseDirectoryTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &ts
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	// A plain number is read as seconds: the friendlier half of the two shapes
	// an operator might type.
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}
