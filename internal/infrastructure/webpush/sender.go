// Package webpush wraps the Web Push protocol library (VAPID + RFC 8291
// message encryption) behind the notifications.PushSender seam, so the usecase
// layer stays free of the transport library and the network.
package webpush

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	wp "github.com/SherClockHolmes/webpush-go"

	"backend-core/internal/domain"
)

// Config holds the VAPID application-server keys. They are read from env only
// (PUSH_VAPID_PUBLIC_KEY / PUSH_VAPID_PRIVATE_KEY / PUSH_VAPID_SUBJECT) and are
// never logged. Configured reports whether a real sender can be built.
type Config struct {
	PublicKey  string
	PrivateKey string
	// Subject is the VAPID "sub" claim: a mailto: or https: URL identifying the
	// application server to the push service (RFC 8292).
	Subject string
	// TTL is how long the push service should retain an undelivered message.
	TTL time.Duration
}

// Configured reports whether both VAPID keys are present. When false the
// caller runs the web-push channel as a clean no-op.
func (c Config) Configured() bool {
	return c.PublicKey != "" && c.PrivateKey != ""
}

// Sender delivers encrypted Web Push messages using VAPID.
type Sender struct {
	cfg    Config
	client *http.Client
}

// NewSender builds a Web Push sender. Callers should only build one when
// cfg.Configured() is true.
func NewSender(cfg Config) *Sender {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	cfg.TTL = ttl
	return &Sender{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}}
}

// Send delivers payload to one subscription and returns the push service's HTTP
// status code. A 201/200 means accepted; 404/410 means the subscription is
// gone; other codes are transient failures the caller retries.
func (s *Sender) Send(ctx context.Context, sub domain.PushSubscription, payload []byte) (int, error) {
	resp, err := wp.SendNotificationWithContext(ctx, payload, &wp.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     wp.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &wp.Options{
		HTTPClient:      s.client,
		Subscriber:      s.cfg.Subject,
		VAPIDPublicKey:  s.cfg.PublicKey,
		VAPIDPrivateKey: s.cfg.PrivateKey,
		TTL:             int(s.cfg.TTL.Seconds()),
		Urgency:         wp.UrgencyHigh,
	})
	if err != nil {
		return 0, fmt.Errorf("send web push: %w", err)
	}
	defer resp.Body.Close()
	// Drain and discard: the push service returns an empty body on success, but
	// leaving it unread would leak the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
