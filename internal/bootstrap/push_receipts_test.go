package bootstrap

import (
	"testing"
	"time"
)

// Without a push provider nothing is ever SENT, so there is no ticket to poll:
// the worker must not be started at all. This is deliberately the opposite of
// the reconcilers' "always armed, safe when idle" rule — here an armed loop
// could only produce scheduled round-trips to a provider we are not using.
func TestPushReceiptWorkerNotBuiltWithoutProvider(t *testing.T) {
	cfg := Config{}
	if w := NewPushReceiptWorker(cfg, nil, discardLogger()); w != nil {
		t.Fatal("the receipt worker was built with no GUEST_PUSH_PROVIDER set")
	}
}

// An unknown provider name is a config typo. It must not start a worker either
// — and it must not panic the process on boot.
func TestPushReceiptWorkerNotBuiltForUnknownProvider(t *testing.T) {
	cfg := Config{}
	cfg.Push.GuestPushProvider = "onesignal"
	if w := NewPushReceiptWorker(cfg, nil, discardLogger()); w != nil {
		t.Fatal("the receipt worker was built for an unimplemented provider")
	}
}

// With Expo configured the worker exists, so RunWorker starts it.
func TestPushReceiptWorkerBuiltForExpo(t *testing.T) {
	cfg := Config{}
	cfg.Push.GuestPushProvider = "Expo" // case-insensitive, like the send path
	cfg.Push.ReceiptsTick = time.Minute
	if w := NewPushReceiptWorker(cfg, nil, discardLogger()); w == nil {
		t.Fatal("the receipt worker was not built with GUEST_PUSH_PROVIDER=expo")
	}
}
