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
