package pos

import (
	"errors"
	"fmt"

	"backend-core/internal/domain"
)

// Registry errors. Both wrap a domain sentinel so transport keeps mapping them
// (422 / 404) without learning what a POS is — the same sentinel-error style as
// infrastructure/payment's Registry. Neither panics: an unknown or unwired POS
// is an ordinary, expected outcome (an admin picking a POS whose credentials
// are not in this build yet), not a programming error.
var (
	// ErrProviderUnknown — the code is not a POS this build knows.
	ErrProviderUnknown = fmt.Errorf("unknown POS provider: %w", domain.ErrValidation)
	// ErrProviderNotConfigured — a known code with no adapter wired in (its
	// credentials are missing from env, so bootstrap skipped it, or it is still
	// a template stub).
	ErrProviderNotConfigured = fmt.Errorf("POS provider is not configured: %w", domain.ErrNotFound)
)

// Registry maps a POSProvider code to the connector this process actually has.
//
// It is deliberately thinner than the payment Registry: there is no
// enabled/default/priority table yet. The restaurant→POS binding and its
// enable/disable switch are a follow-up that needs a migration and is
// intentionally out of scope here (see doc.go). Until then this registry only
// answers "does this build have an adapter for this POS?".
type Registry struct {
	connectors map[domain.POSProvider]domain.POSConnector
}

// NewRegistry wires the connectors this process has. Passing two connectors
// that report the same Name() is a wiring bug and is rejected here rather than
// silently letting one win (mirrors payment.NewRegistry).
func NewRegistry(connectors ...domain.POSConnector) (*Registry, error) {
	m := make(map[domain.POSProvider]domain.POSConnector, len(connectors))
	for _, c := range connectors {
		if c == nil {
			return nil, errors.New("pos registry: nil connector")
		}
		name := c.Name()
		if !name.Valid() {
			return nil, fmt.Errorf("pos registry: connector reports %q: %w", name, ErrProviderUnknown)
		}
		if _, dup := m[name]; dup {
			return nil, fmt.Errorf("pos registry: duplicate connector for %q", name)
		}
		m[name] = c
	}
	return &Registry{connectors: m}, nil
}

// Configured reports whether an adapter for p exists in this process.
func (r *Registry) Configured(p domain.POSProvider) bool {
	_, ok := r.connectors[p]
	return ok
}

// For returns the connector for p.
//
// An invalid code is ErrProviderUnknown (wraps domain.ErrValidation → 422); a
// valid code with no adapter wired in is ErrProviderNotConfigured (wraps
// domain.ErrNotFound → 404). Callers distinguish the two with errors.Is.
func (r *Registry) For(p domain.POSProvider) (domain.POSConnector, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("%q: %w", p, ErrProviderUnknown)
	}
	c, ok := r.connectors[p]
	if !ok {
		return nil, fmt.Errorf("%q: %w", p, ErrProviderNotConfigured)
	}
	return c, nil
}
