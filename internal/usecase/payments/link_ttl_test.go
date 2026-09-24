package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestConfigLinkTTLFor(t *testing.T) {
	c := Config{}.withDefaults()
	if c.LinkTTL != 15*time.Minute || c.HoldTTL != 96*time.Hour {
		t.Fatalf("defaults link=%s hold=%s, want 15m/96h", c.LinkTTL, c.HoldTTL)
	}
	for _, p := range []domain.PaymentPurpose{domain.PurposeDeposit, domain.PurposePreorder} {
		if got := c.linkTTLFor(p); got != 15*time.Minute {
			t.Errorf("%s ttl = %s, want 15m", p, got)
		}
	}
	if got := c.linkTTLFor(domain.PurposeTicket); got != 96*time.Hour {
		t.Errorf("ticket ttl = %s, want 96h", got)
	}
	c = Config{LinkTTL: 5 * time.Minute, HoldTTL: time.Hour}.withDefaults()
	if c.linkTTLFor(domain.PurposeDeposit) != 5*time.Minute || c.linkTTLFor(domain.PurposeTicket) != time.Hour {
		t.Errorf("explicit values not honoured")
	}
}

func TestCreateForBooking_UsesLinkTTL(t *testing.T) {
	b := testBooking(uuid.New())
	u, _, _, _ := newCreateHarness(t, b, 1_000_000, 350)
	before := time.Now()
	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ExpiresAt == nil {
		t.Fatal("expires_at nil")
	}
	d := p.ExpiresAt.Sub(before)
	if d < 15*time.Minute || d > 15*time.Minute+time.Minute {
		t.Fatalf("expires_at in %s, want ~15m", d)
	}
}

func TestWebhookAuthorized_DepositHoldExtendedToHoldTTL(t *testing.T) {
	short := time.Now().Add(15 * time.Minute)
	p := testPayment(uuid.New(), domain.PaymentCreated, "gw-ttl")
	p.ExpiresAt = &short
	u, repo, _, _, _, gw := newWebhookHarness(p)
	u.holdTTL = 96 * time.Hour
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderFreedomPay, ProviderEventID: "evt-ttl", ProviderPaymentID: "gw-ttl",
		Type: domain.WebhookPaymentAuthorized, Status: domain.PaymentAuthorized, SignatureValid: true,
	})
	if err := u.HandleWebhook(context.Background(), domain.ProviderFreedomPay, []byte("b"), nil); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentAuthorized || stored.ExpiresAt == nil || stored.ExpiresAt.Before(time.Now().Add(95*time.Hour)) {
		t.Fatalf("hold not extended: status=%s expires=%v", stored.Status, stored.ExpiresAt)
	}
}
