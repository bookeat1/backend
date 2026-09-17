package pushcampaign

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func almatyCityID(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM cities WHERE code='almaty'`).Scan(&id); err != nil {
		t.Fatalf("read almaty city id (seeded by migration 0081): %v", err)
	}
	return id
}

func seedRestaurant(t *testing.T, pool *pgxpool.Pool, city string, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, is_active, created_at, updated_at)
		 VALUES ($1, $2, $3, '₸', $4, now(), now())`, id, "Venue "+id.String()[:8], city, active); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

func seedEvent(t *testing.T, pool *pgxpool.Pool, restaurantID *uuid.UUID, status string, startsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	endsAt := startsAt.Add(3 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO events (id, restaurant_id, title, title_i18n, starts_at, ends_at, status, created_at, updated_at)
		 VALUES ($1,$2,'Джазовый вечер','{"kk":"Джаз кеші","en":"Jazz night"}'::jsonb,$3,$4,$5, now(), now())`,
		id, restaurantID, startsAt, endsAt, status); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

func seedUserInCity(t *testing.T, pool *pgxpool.Pool, city string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, full_name, role, city, created_at, updated_at)
		 VALUES ($1,$2,'Guest','user',$3, now(), now())`, id, id.String()+"@example.test", city); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedDeviceToken(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO device_push_tokens (id, user_id, token, platform, is_active, created_at, updated_at)
		 VALUES ($1,$2,$3,'ios', true, now(), now())`, uuid.New(), userID, "tok-"+uuid.NewString()); err != nil {
		t.Fatalf("seed device token: %v", err)
	}
}

// TestSubjectResolve pins criterion 11's read shape: the event's own status,
// window and EFFECTIVE city (COALESCE(event.city_id, restaurant.city_id))
// come back in one row, matching for both a venue-bound and a platform event.
func TestSubjectResolve(t *testing.T) {
	pool := testdb.Connect(t)
	almaty := almatyCityID(t, pool)
	repo := NewSubjects(pool)

	t.Run("venue event inherits the venue's city", func(t *testing.T) {
		restaurantID := seedRestaurant(t, pool, "Алматы", true)
		eventID := seedEvent(t, pool, &restaurantID, "published", time.Now().Add(24*time.Hour))

		s, err := repo.Resolve(context.Background(), domain.PushCampaignKindEvent, eventID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if s.RestaurantID == nil || *s.RestaurantID != restaurantID {
			t.Errorf("restaurant id = %v, want %v", s.RestaurantID, restaurantID)
		}
		if s.CityID == nil || *s.CityID != almaty {
			t.Errorf("city id = %v, want %v (almaty)", s.CityID, almaty)
		}
		if !s.RestaurantIsActive {
			t.Error("restaurant_is_active = false, want true")
		}
		if s.Status != "published" {
			t.Errorf("status = %q, want published", s.Status)
		}
		if s.Expired(time.Now()) {
			t.Error("future event reported expired")
		}
	})

	t.Run("platform event with no venue has no city (everywhere)", func(t *testing.T) {
		eventID := seedEvent(t, pool, nil, "published", time.Now().Add(24*time.Hour))
		s, err := repo.Resolve(context.Background(), domain.PushCampaignKindEvent, eventID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if s.RestaurantID != nil {
			t.Errorf("restaurant id = %v, want nil", s.RestaurantID)
		}
		if s.CityID != nil {
			t.Errorf("city id = %v, want nil (everywhere)", s.CityID)
		}
		if !s.RestaurantIsActive {
			t.Error("platform subject restaurant_is_active = false, want true (vacuous)")
		}
	})

	t.Run("expired event", func(t *testing.T) {
		restaurantID := seedRestaurant(t, pool, "Алматы", true)
		eventID := seedEvent(t, pool, &restaurantID, "published", time.Now().Add(-time.Hour))
		s, err := repo.Resolve(context.Background(), domain.PushCampaignKindEvent, eventID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !s.Expired(time.Now()) {
			t.Error("past event not reported expired")
		}
	})

	t.Run("missing subject", func(t *testing.T) {
		_, err := repo.Resolve(context.Background(), domain.PushCampaignKindEvent, uuid.New())
		if err == nil {
			t.Fatal("want ErrNotFound for a missing subject")
		}
	})
}

// TestAudienceClassify pins criterion 12/13: city membership via
// city_aliases (several spellings of the same city), and the four raw
// classification signals.
func TestAudienceClassify(t *testing.T) {
	pool := testdb.Connect(t)
	almaty := almatyCityID(t, pool)
	audience := NewAudience(pool)
	campaigns := New(pool)
	recipients := NewRecipients(pool)
	now := time.Now()

	inCityAlmaty := seedUserInCity(t, pool, "Алматы")
	inCityAlmatyLower := seedUserInCity(t, pool, "алматы")
	inCityAlmatyLatin := seedUserInCity(t, pool, "Almaty")
	inAstana := seedUserInCity(t, pool, "Астана")
	noCity := seedUserInCity(t, pool, "")

	seedDeviceToken(t, pool, inCityAlmaty)
	seedDeviceToken(t, pool, inCityAlmatyLower)
	seedDeviceToken(t, pool, inCityAlmatyLatin)

	subjectID := uuid.New()
	rows, err := audience.Classify(context.Background(), &almaty, domain.PushCampaignKindEvent, subjectID, now, 1, 3)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	byUser := make(map[uuid.UUID]domain.PushAudienceRow, len(rows))
	for _, r := range rows {
		byUser[r.UserID] = r
	}
	for _, id := range []uuid.UUID{inCityAlmaty, inCityAlmatyLower, inCityAlmatyLatin} {
		if _, ok := byUser[id]; !ok {
			t.Errorf("user %s (almaty spelling) missing from audience", id)
		}
	}
	if _, ok := byUser[inAstana]; ok {
		t.Error("astana guest wrongly included in almaty audience")
	}
	if _, ok := byUser[noCity]; ok {
		t.Error("guest with no city wrongly included in a CITY-scoped audience")
	}
	if r := byUser[inCityAlmaty]; !r.HasDevice {
		t.Error("guest with an active token classified as has_device=false")
	}

	t.Run("platform subject with nil city includes everyone, including no-city guests", func(t *testing.T) {
		rows, err := audience.Classify(context.Background(), nil, domain.PushCampaignKindEvent, uuid.New(), now, 1, 3)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		byUser := make(map[uuid.UUID]bool, len(rows))
		for _, r := range rows {
			byUser[r.UserID] = true
		}
		if !byUser[noCity] {
			t.Error("everywhere campaign must include a guest with no city")
		}
		if !byUser[inAstana] {
			t.Error("everywhere campaign must include a guest in another city")
		}
	})

	t.Run("opt-out, cap and duplicate signals", func(t *testing.T) {
		guest := seedUserInCity(t, pool, "Алматы")
		seedDeviceToken(t, pool, guest)
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO user_notification_preferences (user_id, promo_push_enabled, updated_at)
			 VALUES ($1, false, now())`, guest); err != nil {
			t.Fatalf("seed preference: %v", err)
		}
		rows, err := audience.Classify(context.Background(), &almaty, domain.PushCampaignKindEvent, subjectID, now, 1, 3)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		var found domain.PushAudienceRow
		for _, r := range rows {
			if r.UserID == guest {
				found = r
			}
		}
		if found.Allowed {
			t.Error("guest with promo_push_enabled=false classified as allowed")
		}

		// Give the guest a `sent` row for this exact subject on a past
		// campaign — must show up as duplicate on a fresh classify.
		otherCampaign := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, EstimatedRecipients: 1}
		if err := campaigns.Create(context.Background(), otherCampaign); err != nil {
			t.Fatalf("create campaign: %v", err)
		}
		if err := campaigns.Finish(context.Background(), otherCampaign.ID, 0, 0, 0, now); err != nil {
			// Finish only applies from `sending`; force it directly for the
			// fixture instead of going through the full state machine.
			if _, err := pool.Exec(context.Background(), `UPDATE push_campaigns SET status='done' WHERE id=$1`, otherCampaign.ID); err != nil {
				t.Fatalf("force campaign done: %v", err)
			}
		}
		if _, err := recipients.ClaimSending(context.Background(), otherCampaign.ID, []uuid.UUID{guest}, now); err != nil {
			t.Fatalf("claim sending: %v", err)
		}
		if err := recipients.Resolve(context.Background(), otherCampaign.ID, guest, domain.RecipientSent, now); err != nil {
			t.Fatalf("resolve sent: %v", err)
		}

		rows, err = audience.Classify(context.Background(), &almaty, domain.PushCampaignKindEvent, subjectID, now, 1, 3)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		for _, r := range rows {
			if r.UserID == guest {
				if !r.Duplicate {
					t.Error("guest with a prior `sent` for this subject not classified as duplicate")
				}
				if !r.Capped {
					t.Error("guest with a `sent` in the last 24h not classified as capped (daily cap = 1)")
				}
			}
		}
	})
}

// TestCampaignClaimLease pins criterion 10: a claimed campaign is not handed
// out again while its lease is still in the future, and IS handed out again
// once the lease has expired.
func TestCampaignClaimLease(t *testing.T) {
	pool := testdb.Connect(t)
	repo := New(pool)
	now := time.Now()

	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), EstimatedRecipients: 1}
	if err := repo.Create(context.Background(), c); err != nil {
		t.Fatalf("create: %v", err)
	}

	txm := sqltx.NewManager(pool)
	claimOnce := func(now time.Time) ([]domain.PushCampaign, []uuid.UUID) {
		var claimed []domain.PushCampaign
		var expired []uuid.UUID
		err := txm.WithinTx(context.Background(), func(ctx context.Context) error {
			var e error
			claimed, expired, e = repo.Claim(ctx, now, 10*time.Minute, 6*time.Hour, 10)
			return e
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return claimed, expired
	}

	claimed, expired := claimOnce(now)
	if len(expired) != 0 {
		t.Fatalf("unexpected expired: %v", expired)
	}
	found := false
	for _, cc := range claimed {
		if cc.ID == c.ID {
			found = true
			if cc.Status != domain.PushCampaignSending {
				t.Errorf("claimed campaign status = %s, want sending", cc.Status)
			}
		}
	}
	if !found {
		t.Fatal("fresh queued campaign was not claimed")
	}

	// Immediately re-claiming (lease still active) must NOT return it again.
	claimed2, _ := claimOnce(now.Add(time.Minute))
	for _, cc := range claimed2 {
		if cc.ID == c.ID {
			t.Fatal("a campaign with an active lease was claimed a second time")
		}
	}

	// After the lease expires, it becomes claimable again.
	claimed3, _ := claimOnce(now.Add(11 * time.Minute))
	found = false
	for _, cc := range claimed3 {
		if cc.ID == c.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("campaign with an expired lease was not reclaimed")
	}
}

// TestCampaignClaimExpiresStaleQueue pins criterion 17: a queued campaign
// older than maxQueueAge is expired instead of claimed.
func TestCampaignClaimExpiresStaleQueue(t *testing.T) {
	pool := testdb.Connect(t)
	repo := New(pool)
	now := time.Now()

	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), EstimatedRecipients: 1}
	if err := repo.Create(context.Background(), c); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Backdate created_at past the queue-age ceiling.
	if _, err := pool.Exec(context.Background(),
		`UPDATE push_campaigns SET created_at = $2 WHERE id = $1`, c.ID, now.Add(-7*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	txm := sqltx.NewManager(pool)
	var claimed []domain.PushCampaign
	var expired []uuid.UUID
	err := txm.WithinTx(context.Background(), func(ctx context.Context) error {
		var e error
		claimed, expired, e = repo.Claim(ctx, now, 10*time.Minute, 6*time.Hour, 10)
		return e
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, cc := range claimed {
		if cc.ID == c.ID {
			t.Fatal("a stale queued campaign was claimed instead of expired")
		}
	}
	foundExpired := false
	for _, id := range expired {
		if id == c.ID {
			foundExpired = true
		}
	}
	if !foundExpired {
		t.Fatal("a stale queued campaign was not reported expired")
	}
	got, err := repo.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.PushCampaignExpired {
		t.Errorf("status = %s, want expired", got.Status)
	}
}

// TestRecipientClaimSendingAtMostOnce pins criterion 14: a user already
// decided for a campaign (any status) is excluded from a second ClaimSending
// call — the guard against a double push after a crash between send and
// record.
func TestRecipientClaimSendingAtMostOnce(t *testing.T) {
	pool := testdb.Connect(t)
	repo := NewRecipients(pool)
	campaigns := New(pool)
	now := time.Now()

	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), EstimatedRecipients: 2}
	if err := campaigns.Create(context.Background(), c); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	u1, u2 := uuid.New(), uuid.New()
	// Users need to exist for the FK.
	for _, id := range []uuid.UUID{u1, u2} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO users (id, email, full_name, role, created_at, updated_at)
			 VALUES ($1,$2,'G','user', now(), now())`, id, id.String()+"@example.test"); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	claimed, err := repo.ClaimSending(context.Background(), c.ID, []uuid.UUID{u1, u2}, now)
	if err != nil {
		t.Fatalf("claim sending: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed = %v, want both users", claimed)
	}
	if err := repo.Resolve(context.Background(), c.ID, u1, domain.RecipientSent, now); err != nil {
		t.Fatalf("resolve u1: %v", err)
	}
	// u2 stays `sending` — simulating a crash between send and record.

	// A second pass (retry) must not re-claim EITHER user.
	claimed2, err := repo.ClaimSending(context.Background(), c.ID, []uuid.UUID{u1, u2}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("claim sending (retry): %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("retry re-claimed %v, want none (both already decided)", claimed2)
	}

	counts, err := repo.CountByStatus(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("count by status: %v", err)
	}
	if counts[domain.RecipientSent] != 1 || counts[domain.RecipientSending] != 1 {
		t.Fatalf("counts = %+v, want 1 sent + 1 sending", counts)
	}
}
