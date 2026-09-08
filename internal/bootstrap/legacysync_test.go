package bootstrap

import (
	"testing"

	"backend-core/internal/logger"
)

// When LEGACY_DB_URL is unset the sync must be a clean no-op: no worker, no
// pool opened, no error — exactly like the other optional workers with absent
// credentials. This never touches a database (it returns before opening one),
// so it runs even in -short mode.
func TestNewLegacySyncWorker_DisabledWhenURLEmpty(t *testing.T) {
	log := logger.New("error", "text")
	worker, closer, err := NewLegacySyncWorker(Config{}, nil, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if worker != nil {
		t.Errorf("worker=%v want nil when LEGACY_DB_URL unset", worker)
	}
	if closer != nil {
		t.Errorf("closer must be nil when disabled")
	}
}
