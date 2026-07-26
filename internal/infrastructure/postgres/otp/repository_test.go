package otp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func TestCreateLatestActiveAndUse(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "otp_codes")
	repo := New(db)
	ctx := context.Background()

	c := &domain.OTPCode{ID: uuid.New(), Phone: "+77070000000", CodeHash: "h", Channel: "stub", ExpiresAt: time.Now().Add(5 * time.Minute)}
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.LatestActiveByPhone(ctx, "+77070000000")
	if err != nil || got.ID != c.ID {
		t.Fatalf("LatestActiveByPhone = %+v, %v", got, err)
	}

	if err := repo.IncrementAttempts(ctx, c.ID); err != nil {
		t.Fatalf("IncrementAttempts: %v", err)
	}
	if err := repo.MarkUsed(ctx, c.ID); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if _, err := repo.LatestActiveByPhone(ctx, "+77070000000"); err == nil {
		t.Error("used code must not be active")
	}

	n, err := repo.CountSince(ctx, "+77070000000", time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("CountSince = %d, %v", n, err)
	}
}

func TestInvalidateActiveByPhone(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "otp_codes")
	repo := New(db)
	ctx := context.Background()

	active := &domain.OTPCode{ID: uuid.New(), Phone: "+77070000001", CodeHash: "h1", Channel: "stub", ExpiresAt: time.Now().Add(5 * time.Minute)}
	expired := &domain.OTPCode{ID: uuid.New(), Phone: "+77070000001", CodeHash: "h2", Channel: "stub", ExpiresAt: time.Now().Add(-time.Minute)}
	otherPhone := &domain.OTPCode{ID: uuid.New(), Phone: "+77070000002", CodeHash: "h3", Channel: "stub", ExpiresAt: time.Now().Add(5 * time.Minute)}
	for _, c := range []*domain.OTPCode{active, expired, otherPhone} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	if err := repo.InvalidateActiveByPhone(ctx, "+77070000001"); err != nil {
		t.Fatalf("InvalidateActiveByPhone: %v", err)
	}

	if _, err := repo.LatestActiveByPhone(ctx, "+77070000001"); err == nil {
		t.Error("expected no active code left for the invalidated phone")
	}
	if _, err := repo.LatestActiveByPhone(ctx, "+77070000002"); err != nil {
		t.Errorf("another phone's active code must be untouched: %v", err)
	}

	// Idempotent: nothing left to invalidate is a no-op success.
	if err := repo.InvalidateActiveByPhone(ctx, "+77070000001"); err != nil {
		t.Fatalf("second InvalidateActiveByPhone: %v", err)
	}
}

// The delivery memory is a query over rows that already exist, so it is only as
// good as this SQL: it must pick the NEWEST used code, ignore codes nobody
// verified, and ignore the bookkeeping channels ("stub", "undelivered") that
// name no real route.
func TestLastUsedChannelByPhone(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "otp_codes")
	repo := New(db)
	ctx := context.Background()

	phone := "+77070000010"
	now := time.Now()
	used := func(c *domain.OTPCode, at time.Time) *domain.OTPCode {
		c.UsedAt = &at
		return c
	}
	rows := []*domain.OTPCode{
		// Verified over WhatsApp two days ago.
		used(&domain.OTPCode{ID: uuid.New(), Phone: phone, CodeHash: "h1", Channel: domain.OTPChannelWhatsApp,
			ExpiresAt: now.Add(-48 * time.Hour), CreatedAt: now.Add(-48 * time.Hour)}, now.Add(-48*time.Hour)),
		// Verified over SMS yesterday — the newest real answer.
		used(&domain.OTPCode{ID: uuid.New(), Phone: phone, CodeHash: "h2", Channel: domain.OTPChannelSMS,
			ExpiresAt: now.Add(-24 * time.Hour), CreatedAt: now.Add(-24 * time.Hour)}, now.Add(-24*time.Hour)),
		// Accepted by Telegram an hour ago but never verified: acceptance is not
		// delivery, so it must NOT win over the SMS above.
		{ID: uuid.New(), Phone: phone, CodeHash: "h3", Channel: domain.OTPChannelTelegram,
			ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now.Add(-time.Hour)},
		// A failed delivery, recorded as used purely for the rate limit.
		used(&domain.OTPCode{ID: uuid.New(), Phone: phone, CodeHash: "h4", Channel: domain.OTPChannelUndelivered,
			ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now.Add(-time.Minute)}, now),
		// Another phone entirely.
		used(&domain.OTPCode{ID: uuid.New(), Phone: "+77070000011", CodeHash: "h5", Channel: domain.OTPChannelTelegram,
			ExpiresAt: now, CreatedAt: now}, now),
	}
	for _, c := range rows {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	got, err := repo.LastUsedChannelByPhone(ctx, phone)
	if err != nil {
		t.Fatalf("LastUsedChannelByPhone: %v", err)
	}
	if got != domain.OTPChannelSMS {
		t.Fatalf("channel = %q, want %q (newest VERIFIED code on a real channel)", got, domain.OTPChannelSMS)
	}

	// A number nobody has ever logged in with has no memory, and that is not an
	// error: the waterfall simply walks its configured order.
	got, err = repo.LastUsedChannelByPhone(ctx, "+77079999999")
	if err != nil {
		t.Fatalf("LastUsedChannelByPhone(unknown): %v", err)
	}
	if got != "" {
		t.Fatalf("channel = %q, want empty for an unknown phone", got)
	}
}
