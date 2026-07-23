package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func strp(s string) *string { return &s }

func TestCreateGetUpdate(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	u := &domain.User{ID: uuid.New(), Email: strp("a@b.com"), FullName: "Alice", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.FullName != "Alice" || got.Email == nil || *got.Email != "a@b.com" {
		t.Errorf("unexpected user: %+v", got)
	}

	byEmail, err := repo.GetByEmail(ctx, "a@b.com")
	if err != nil || byEmail.ID != u.ID {
		t.Fatalf("GetByEmail: %v / %+v", err, byEmail)
	}

	u.FullName = "Alice B"
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = repo.GetByID(ctx, u.ID)
	if got.FullName != "Alice B" {
		t.Errorf("update not persisted: %q", got.FullName)
	}
}

func TestCreateDuplicateEmailMapsToAlreadyExists(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	first := &domain.User{ID: uuid.New(), Email: strp("dup@b.com"), FullName: "First", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Same email, different id: the UNIQUE constraint fires and must surface as
	// ErrAlreadyExists (SQLSTATE 23505 mapping), not a raw error.
	second := &domain.User{ID: uuid.New(), Email: strp("dup@b.com"), FullName: "Second", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, second); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate email Create = %v, want ErrAlreadyExists", err)
	}
}

func TestGetByIDNotFound(t *testing.T) {
	db := testdb.Connect(t)
	repo := New(db)
	if _, err := repo.GetByID(context.Background(), uuid.New()); err == nil {
		t.Error("expected ErrNotFound")
	}
}

func TestCreateAndUpdatePersistProfileExtensions(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	u := &domain.User{ID: uuid.New(), Email: strp("guest@b.com"), FullName: "Guest", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	bd := time.Date(1998, 5, 4, 0, 0, 0, 0, time.UTC)
	u.CountryCode = strp("KZ")
	u.BirthDate = &bd
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.CountryCode == nil || *got.CountryCode != "KZ" {
		t.Errorf("country_code not persisted: %+v", got)
	}
	if got.BirthDate == nil || !got.BirthDate.Equal(bd) {
		t.Errorf("birth_date not persisted: %+v", got)
	}
	if got.DeletedAt != nil {
		t.Errorf("expected DeletedAt nil for a live user, got %v", got.DeletedAt)
	}
}

func TestDeleteAnonymizesAndFreesPhoneForReuse(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	repo := New(db)
	ctx := context.Background()

	phone := "+77011234567"
	bd := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	u := &domain.User{
		ID: uuid.New(), Email: strp("del@b.com"), Phone: &phone, FullName: "Deleted Guy",
		Role: domain.RoleUser, IsActive: true, AvatarURL: strp("https://cdn/x.png"),
		PreferredLanguage: "ru", City: strp("almaty"), CountryCode: strp("KZ"), BirthDate: &bd,
	}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID after delete: %v", err)
	}
	if got.DeletedAt == nil {
		t.Error("expected DeletedAt set")
	}
	if got.Email != nil || got.Phone != nil || got.FullName != "" || got.AvatarURL != nil ||
		got.City != nil || got.CountryCode != nil || got.BirthDate != nil || got.IsActive {
		t.Errorf("expected fully anonymized user, got %+v", got)
	}

	// Idempotent: a repeat Delete on an already-deleted user succeeds and
	// changes nothing further.
	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("second Delete (idempotent): %v", err)
	}

	// The freed phone number is immediately usable by a brand-new signup —
	// this is the whole point of anonymizing to NULL rather than a
	// placeholder string, since the column stays UNIQUE.
	second := &domain.User{ID: uuid.New(), Phone: &phone, FullName: "New Owner", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, second); err != nil {
		t.Fatalf("Create with reused phone: %v", err)
	}
	byPhone, err := repo.GetByPhone(ctx, phone)
	if err != nil || byPhone.ID != second.ID {
		t.Fatalf("GetByPhone after reuse = %+v, %v, want the new user %v", byPhone, err, second.ID)
	}
}

func TestDeleteUnknownUserIsNotFound(t *testing.T) {
	db := testdb.Connect(t)
	repo := New(db)
	if err := repo.Delete(context.Background(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Delete unknown user = %v, want ErrNotFound", err)
	}
}

// TestDeletePreservesBookingReference is the "money/history is sacred"
// guarantee: deleting a user must anonymize the users row without touching
// (or losing) any booking that references it. bookings.user_id is
// ON DELETE SET NULL, which only matters for a real DELETE FROM users — this
// repository never issues one, so the reference must survive completely
// unchanged.
func TestDeletePreservesBookingReference(t *testing.T) {
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "users")
	ctx := context.Background()
	repo := New(db)

	phone := "+77019998877"
	u := &domain.User{ID: uuid.New(), Phone: &phone, FullName: "Booker", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create user: %v", err)
	}

	restaurantID := uuid.New()
	if _, err := db.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, restaurantID); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	bookingID := uuid.New()
	now := time.Now()
	if _, err := db.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, user_id, name, phone, phone_normalized, guests, starts_at, ends_at)
		 VALUES ($1,$2,$3,'Booker',$4,$4,2,$5,$6)`,
		bookingID, restaurantID, u.ID, phone, now, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}

	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var refUserID uuid.UUID
	if err := db.QueryRow(ctx, `SELECT user_id FROM bookings WHERE id = $1`, bookingID).Scan(&refUserID); err != nil {
		t.Fatalf("booking must survive user deletion: %v", err)
	}
	if refUserID != u.ID {
		t.Errorf("booking.user_id = %v, want unchanged reference %v", refUserID, u.ID)
	}
}
