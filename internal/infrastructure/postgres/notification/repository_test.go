package notification

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func seedRestaurant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, id); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Staff')`,
		id, id.String()+"@example.com", "+7777"+id.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedOutboxEvent creates a real booking + booking.created outbox row so the
// notification_deliveries FK is satisfied.
func seedOutboxEvent(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, Name: "Гость", Phone: "+7 (777) 123-45-67",
		Email: "guest@example.com", PhoneNormalized: "+77771234567", Guests: 2,
		StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(26 * time.Hour),
		Status: domain.BookingPending, Source: domain.SourceApp,
	}
	if err := bookingrepo.New(pool).Create(ctx, b); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	ev := &domain.BookingOutboxEvent{
		ID: uuid.New(), BookingID: b.ID, EventType: domain.EventBookingCreated,
	}
	if err := bookingrepo.NewOutbox(pool).Create(ctx, ev); err != nil {
		t.Fatalf("seed outbox event: %v", err)
	}
	return ev.ID
}

func truncate(t *testing.T, pool *pgxpool.Pool) {
	testdb.Truncate(t, pool, "notification_deliveries", "push_subscriptions",
		"restaurant_notification_settings", "booking_outbox", "bookings", "restaurants", "users")
}

func TestPushSubscriptions_UpsertIsIdempotentOnEndpoint(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	repo := NewSubscriptions(pool)

	first := &domain.PushSubscription{UserID: uid, RestaurantID: rid, Endpoint: "https://push/x", P256dh: "p1", Auth: "a1"}
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Re-register the same endpoint with rotated keys → overwrites in place.
	again := &domain.PushSubscription{UserID: uid, RestaurantID: rid, Endpoint: "https://push/x", P256dh: "p2", Auth: "a2"}
	if err := repo.Upsert(ctx, again); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := repo.ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("subscriptions for endpoint = %d, want 1 (upsert, not duplicate)", len(got))
	}
	if got[0].P256dh != "p2" || got[0].Auth != "a2" {
		t.Fatalf("keys not overwritten: %+v", got[0])
	}
}

func TestPushSubscriptions_DeleteScopedToUser(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	owner := seedUser(t, pool)
	other := seedUser(t, pool)
	repo := NewSubscriptions(pool)

	sub := &domain.PushSubscription{UserID: owner, RestaurantID: rid, Endpoint: "https://push/y", P256dh: "p", Auth: "a"}
	if err := repo.Upsert(ctx, sub); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Another user cannot delete it even with the exact endpoint.
	if err := repo.DeleteByEndpointForUser(ctx, other, "https://push/y"); err != nil {
		t.Fatalf("delete by other: %v", err)
	}
	if got, _ := repo.ListByRestaurant(ctx, rid); len(got) != 1 {
		t.Fatal("subscription deleted by a non-owner")
	}
	// The owner can.
	if err := repo.DeleteByEndpointForUser(ctx, owner, "https://push/y"); err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
	if got, _ := repo.ListByRestaurant(ctx, rid); len(got) != 0 {
		t.Fatal("owner could not delete their own subscription")
	}
}

func TestSettings_DefaultOnWhenNoRow(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)

	enabled, err := NewSettings(pool).WebPushEnabled(ctx, rid)
	if err != nil {
		t.Fatalf("web push enabled: %v", err)
	}
	if !enabled {
		t.Fatal("web push must default to ON when the venue has no settings row")
	}

	// Explicit opt-out row → disabled.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_notification_settings (restaurant_id, web_push_enabled) VALUES ($1, false)`, rid); err != nil {
		t.Fatalf("insert settings: %v", err)
	}
	enabled, err = NewSettings(pool).WebPushEnabled(ctx, rid)
	if err != nil {
		t.Fatalf("web push enabled after opt-out: %v", err)
	}
	if enabled {
		t.Fatal("web push should be disabled after an explicit opt-out row")
	}
}

func TestTelegramSettings_DefaultSetClear(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	repo := NewSettings(pool)

	// No row → enabled, no chat id (silent by default).
	s, err := repo.TelegramSettings(ctx, rid)
	if err != nil {
		t.Fatalf("telegram settings (no row): %v", err)
	}
	if s.ChatID != "" || !s.Enabled {
		t.Fatalf("default = %+v, want {ChatID:'' Enabled:true}", s)
	}

	// Connect a chat id → stored, enabled.
	if err := repo.SetTelegramChatID(ctx, rid, "-1001234567890"); err != nil {
		t.Fatalf("set chat id: %v", err)
	}
	s, _ = repo.TelegramSettings(ctx, rid)
	if s.ChatID != "-1001234567890" || !s.Enabled {
		t.Fatalf("after set = %+v, want chat -1001234567890 / enabled", s)
	}
	// Web push default must be untouched by connecting telegram.
	if enabled, _ := repo.WebPushEnabled(ctx, rid); !enabled {
		t.Fatal("connecting telegram must not disable web push (row default)")
	}

	// Re-connect a different chat id → overwrites in place.
	if err := repo.SetTelegramChatID(ctx, rid, "@bookeat_venue"); err != nil {
		t.Fatalf("re-set chat id: %v", err)
	}
	s, _ = repo.TelegramSettings(ctx, rid)
	if s.ChatID != "@bookeat_venue" {
		t.Fatalf("after re-set chat = %q, want @bookeat_venue", s.ChatID)
	}

	// Clear → silent again (chat id gone), row preserved.
	if err := repo.ClearTelegramChatID(ctx, rid); err != nil {
		t.Fatalf("clear chat id: %v", err)
	}
	s, _ = repo.TelegramSettings(ctx, rid)
	if s.ChatID != "" {
		t.Fatalf("after clear chat = %q, want empty", s.ChatID)
	}
}

// The generalized ledger dedupes a telegram delivery keyed by the restaurant id
// (its target), independently of any web_push delivery of the same event.
func TestDeliveries_TelegramTargetDedupe(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	eventID := seedOutboxEvent(t, pool, rid)

	del := NewDeliveries(pool)
	if already, _ := del.AlreadyDelivered(ctx, eventID, domain.ChannelTelegram, rid); already {
		t.Fatal("nothing delivered yet, want false")
	}
	if err := del.RecordDelivered(ctx, eventID, domain.ChannelTelegram, rid); err != nil {
		t.Fatalf("record telegram: %v", err)
	}
	// Idempotent on the same (event, telegram, restaurant).
	if err := del.RecordDelivered(ctx, eventID, domain.ChannelTelegram, rid); err != nil {
		t.Fatalf("record telegram again (ON CONFLICT): %v", err)
	}
	if already, _ := del.AlreadyDelivered(ctx, eventID, domain.ChannelTelegram, rid); !already {
		t.Fatal("telegram delivery not recorded")
	}
	// A web_push delivery of the SAME event to the SAME target id is a DIFFERENT
	// ledger row (channel is part of the key) — proves per-channel dedupe.
	if already, _ := del.AlreadyDelivered(ctx, eventID, domain.ChannelWebPush, rid); already {
		t.Fatal("telegram delivery must not mark web_push as delivered")
	}
}

func TestDeliveries_DedupeOnConflict(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	eventID := seedOutboxEvent(t, pool, rid)

	subRepo := NewSubscriptions(pool)
	sub := &domain.PushSubscription{UserID: uid, RestaurantID: rid, Endpoint: "https://push/z", P256dh: "p", Auth: "a"}
	if err := subRepo.Upsert(ctx, sub); err != nil {
		t.Fatalf("upsert sub: %v", err)
	}

	del := NewDeliveries(pool)
	already, err := del.AlreadyDelivered(ctx, eventID, domain.ChannelWebPush, sub.ID)
	if err != nil {
		t.Fatalf("already delivered (pre): %v", err)
	}
	if already {
		t.Fatal("nothing delivered yet, want false")
	}

	if err := del.RecordDelivered(ctx, eventID, domain.ChannelWebPush, sub.ID); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	// A second record of the same (event, channel, subscription) is a no-op,
	// never a unique-violation.
	if err := del.RecordDelivered(ctx, eventID, domain.ChannelWebPush, sub.ID); err != nil {
		t.Fatalf("record 2 (ON CONFLICT DO NOTHING) errored: %v", err)
	}

	already, err = del.AlreadyDelivered(ctx, eventID, domain.ChannelWebPush, sub.ID)
	if err != nil {
		t.Fatalf("already delivered (post): %v", err)
	}
	if !already {
		t.Fatal("delivery not recorded")
	}
}

// WhatsApp повторяет форму телеграма, поэтому проверяется то же самое плюс
// одно, чего у телеграма нет: обратный поиск по номеру — это АВТОРИЗАЦИЯ
// входящего нажатия, и выключённый канал не должен по нему находиться.
func TestWhatsAppSettings_DefaultSetClearAndReverseLookup(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	repo := NewSettings(pool)

	// Нет строки → канал включён, но номера нет: молчит.
	s, err := repo.WhatsAppSettings(ctx, rid)
	if err != nil {
		t.Fatalf("whatsapp settings (нет строки): %v", err)
	}
	if s.Phone != "" || !s.Enabled {
		t.Fatalf("по умолчанию = %+v, want {Phone:'' Enabled:true}", s)
	}

	if err := repo.SetWhatsAppPhone(ctx, rid, "+77010000001"); err != nil {
		t.Fatalf("set phone: %v", err)
	}
	s, _ = repo.WhatsAppSettings(ctx, rid)
	if s.Phone != "+77010000001" || !s.Enabled {
		t.Fatalf("после записи = %+v, want номер и включён", s)
	}
	// Подключение WhatsApp не должно трогать соседние каналы.
	if enabled, _ := repo.WebPushEnabled(ctx, rid); !enabled {
		t.Fatal("подключение whatsapp выключило браузерные уведомления")
	}
	if tg, _ := repo.TelegramSettings(ctx, rid); tg.ChatID != "" || !tg.Enabled {
		t.Fatalf("подключение whatsapp задело телеграм: %+v", tg)
	}

	// Обратный поиск находит заведение по номеру отправителя.
	got, err := repo.RestaurantByWhatsAppPhone(ctx, "+77010000001")
	if err != nil || got != rid {
		t.Fatalf("обратный поиск = %v / %v, want %v", got, err, rid)
	}
	if _, err := repo.RestaurantByWhatsAppPhone(ctx, "+77019999999"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("чужой номер: err = %v, want ErrNotFound", err)
	}

	// Выключенный канал не резолвится: снятая галочка должна обезвредить и
	// кнопки в сообщениях, которые уже лежат у человека в переписке.
	if _, err := pool.Exec(ctx,
		`UPDATE restaurant_notification_settings SET whatsapp_enabled = false WHERE restaurant_id = $1`, rid); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	if _, err := repo.RestaurantByWhatsAppPhone(ctx, "+77010000001"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("выключенный канал всё ещё авторизует нажатие: err = %v", err)
	}

	// Очистка номера: канал снова молчит, строка настроек цела.
	if err := repo.ClearWhatsAppPhone(ctx, rid); err != nil {
		t.Fatalf("clear phone: %v", err)
	}
	s, _ = repo.WhatsAppSettings(ctx, rid)
	if s.Phone != "" {
		t.Fatalf("после очистки номер = %q, want пусто", s.Phone)
	}
}

// Один и тот же номер не может обслуживать два заведения: иначе нажатие
// «подтвердить» невозможно приписать кому-то одному, и чужая бронь
// подтверждалась бы с чужого телефона.
func TestWhatsAppPhone_IsUniqueAcrossVenues(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	first := seedRestaurant(t, pool)
	second := seedRestaurant(t, pool)
	repo := NewSettings(pool)

	if err := repo.SetWhatsAppPhone(ctx, first, "+77010000002"); err != nil {
		t.Fatalf("первое заведение: %v", err)
	}
	if err := repo.SetWhatsAppPhone(ctx, second, "+77010000002"); err == nil {
		t.Fatal("один номер записался двум заведениям — обратный поиск станет неоднозначным")
	}
}

// The two migration writes of 0098 against real SQL. The half that matters is
// the demotion: MarkTelegramNewBotFailed must CLEAR ready_at in the same
// statement, because that is what silently returns the venue to the old bot
// instead of letting its alerts die.
func TestTelegramNewBotFlags_ReadyAndFailed(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(t, pool)
	repo := NewSettings(pool)

	if err := repo.SetTelegramChatID(ctx, rid, "-1009876"); err != nil {
		t.Fatalf("set chat id: %v", err)
	}
	s, err := repo.TelegramSettings(ctx, rid)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if s.NewBotReady() || s.NewBotFailedAt != nil {
		t.Fatalf("a freshly connected chat must start unmigrated, got %+v", s)
	}

	if err := repo.MarkTelegramNewBotReady(ctx, rid); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	s, _ = repo.TelegramSettings(ctx, rid)
	if !s.NewBotReady() {
		t.Fatal("ready_at was not written")
	}

	if err := repo.MarkTelegramNewBotFailed(ctx, rid); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	s, _ = repo.TelegramSettings(ctx, rid)
	if s.NewBotReady() {
		t.Fatal("a refusal must clear ready_at, otherwise the venue keeps a dead bot")
	}
	if s.NewBotFailedAt == nil {
		t.Fatal("failed_at was not written")
	}

	// Idempotent: pressing Start twice just refreshes the timestamp.
	if err := repo.MarkTelegramNewBotReady(ctx, rid); err != nil {
		t.Fatalf("re-mark ready: %v", err)
	}
	if err := repo.MarkTelegramNewBotReady(ctx, rid); err != nil {
		t.Fatalf("re-mark ready twice: %v", err)
	}

	// A venue with no settings row at all is a no-op, not an error.
	if err := repo.MarkTelegramNewBotReady(ctx, seedRestaurant(t, pool)); err != nil {
		t.Fatalf("mark ready without a settings row: %v", err)
	}
}

// The migration report lists only venues that actually have a chat connected,
// laggards first, and flags @username targets — those cannot press Start and
// need the bot added to the channel by hand.
func TestTelegramMigrationStatus(t *testing.T) {
	pool := testdb.Connect(t)
	truncate(t, pool)
	ctx := context.Background()
	repo := NewSettings(pool)

	migrated := seedRestaurant(t, pool)
	pending := seedRestaurant(t, pool)
	channel := seedRestaurant(t, pool)
	noChat := seedRestaurant(t, pool)

	if err := repo.SetTelegramChatID(ctx, migrated, "-100111"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := repo.MarkTelegramNewBotReady(ctx, migrated); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := repo.SetTelegramChatID(ctx, pending, "-100222"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := repo.SetTelegramChatID(ctx, channel, "@venue_channel"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// A venue that never connected Telegram is not behind on anything.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_notification_settings (restaurant_id) VALUES ($1)`, noChat); err != nil {
		t.Fatalf("seed settings without a chat: %v", err)
	}

	rows, err := repo.TelegramMigrationStatus(ctx)
	if err != nil {
		t.Fatalf("migration status: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (the venue without a chat must be excluded)", len(rows))
	}
	// Laggards first.
	if rows[len(rows)-1].RestaurantID != migrated {
		t.Fatalf("the migrated venue must sort last, got %+v", rows)
	}
	var sawChannel bool
	for _, r := range rows {
		if r.RestaurantID == channel {
			sawChannel = true
			if !r.ChatIsUsername() {
				t.Fatalf("@username target not flagged for manual work: %+v", r)
			}
			if r.NewBotReady() {
				t.Fatal("a channel target cannot be ready — nobody pressed Start")
			}
		}
		if r.RestaurantName == "" {
			t.Fatalf("row without a restaurant name: %+v", r)
		}
	}
	if !sawChannel {
		t.Fatal("the @username venue is missing from the report")
	}
}
