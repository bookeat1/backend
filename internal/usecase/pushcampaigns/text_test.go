package pushcampaigns

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func eventSubject() domain.PushCampaignSubject {
	rid := uuid.New()
	return domain.PushCampaignSubject{
		Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), RestaurantID: &rid,
		Status: "published", StartsAt: time.Date(2026, 9, 26, 19, 0, 0, 0, time.UTC),
		EndsAt: time.Date(2026, 9, 26, 22, 0, 0, 0, time.UTC),
		Title:  "Джазовый вечер", TitleI18n: domain.I18n{"kk": "Джаз кеші"},
		VenueName: "Абай",
	}
}

// TestRenderTextTranslatedVsFallback pins criterion 20: kk with a translation
// uses it, en (no translation given) falls back to Russian.
func TestRenderTextTranslatedVsFallback(t *testing.T) {
	s := eventSubject()
	loc := time.UTC

	kk := renderText(s, "kk", loc)
	if !strings.Contains(kk.Body, "Джаз кеші") {
		t.Fatalf("kk body = %q, want the kk translation", kk.Body)
	}
	if want := "«Абай» мекемесінде жаңа іс-шара"; kk.Title != want {
		t.Fatalf("kk title = %q, want exactly %q (guard against double guillemets)", kk.Title, want)
	}

	en := renderText(s, "en", loc)
	if !strings.Contains(en.Body, "Джазовый вечер") {
		t.Fatalf("en body (no translation) = %q, want the Russian fallback", en.Body)
	}

	ru := renderText(s, "ru", loc)
	if want := "Новое событие в «Абай»"; ru.Title != want {
		t.Fatalf("ru title = %q, want exactly %q (guard against double guillemets, e.g. ««Абай»»)", ru.Title, want)
	}
}

// TestRenderTextPlatformSubjectHasNoVenueName pins the "Афиша/Акция BookEat"
// header for a subject with no host restaurant.
func TestRenderTextPlatformSubjectHasNoVenueName(t *testing.T) {
	s := eventSubject()
	s.RestaurantID = nil
	got := renderText(s, "ru", time.UTC)
	if got.Title != "Афиша BookEat" {
		t.Fatalf("platform event title = %q, want %q", got.Title, "Афиша BookEat")
	}

	promo := s
	promo.Kind = domain.PushCampaignKindPromo
	got = renderText(promo, "ru", time.UTC)
	if got.Title != "Акция BookEat" {
		t.Fatalf("platform promo title = %q, want %q", got.Title, "Акция BookEat")
	}
}

// TestRenderTextBodyNeverExceedsLimit pins criterion 20's "тело ≤ 120
// символов" — a character count, not a byte count, so it must be checked in
// runes: Cyrillic is 2 bytes/rune and a byte-length assertion here would pass
// even if renderText silently halved the guest-visible budget.
func TestRenderTextBodyNeverExceedsLimit(t *testing.T) {
	s := eventSubject()
	s.Title = strings.Repeat("Очень длинное название события ", 10)
	got := renderText(s, "ru", time.UTC)
	if n := len([]rune(got.Body)); n > maxBodyLen {
		t.Fatalf("body length = %d runes, want <= %d", n, maxBodyLen)
	}
}

// TestRenderTextBodyTruncatesRunesNotBytes pins the exact regression from
// code review: a Cyrillic subject long enough to need truncation must not
// have a multi-byte UTF-8 rune sliced in half (which would break Expo's JSON
// encoding downstream), and the date/time suffix must survive intact — it is
// the part truncation must never touch.
func TestRenderTextBodyTruncatesRunesNotBytes(t *testing.T) {
	s := eventSubject()
	// 113 runes + the 16-rune date suffix below = 129, over maxBodyLen (120):
	// long enough to actually force truncation, unlike a shorter title that
	// would silently pass this test without ever exercising truncateBody.
	s.Title = "Джазовый вечер с оркестром имени Курмангазы и специальными гостями, приглашённые артисты, живая музыка и угощение"
	s.TitleI18n = domain.I18n{}
	got := renderText(s, "ru", time.UTC)

	if n := len([]rune(got.Body)); n > maxBodyLen {
		t.Fatalf("body length = %d runes, want <= %d", n, maxBodyLen)
	}
	if !utf8.ValidString(got.Body) {
		t.Fatalf("body is not valid UTF-8: %q", got.Body)
	}
	const suffix = " · 26.09 в 19:00"
	if !strings.HasSuffix(got.Body, suffix) {
		t.Fatalf("body = %q, want it to end with the intact date suffix %q", got.Body, suffix)
	}
	if !strings.HasSuffix(got.Body[:len(got.Body)-len(suffix)], "…") {
		t.Fatalf("body = %q, want the truncated subject to end with an ellipsis", got.Body)
	}
}

// TestRenderPreviewCoversRuKkEn pins criterion 3: the preview always has all
// three languages, regardless of what any single guest's language is.
func TestRenderPreviewCoversRuKkEn(t *testing.T) {
	preview := renderPreview(eventSubject(), time.UTC)
	for _, lang := range []string{"ru", "kk", "en"} {
		if _, ok := preview[lang]; !ok {
			t.Fatalf("preview missing language %q: %+v", lang, preview)
		}
	}
}

// TestRenderTextPromoBodyNamesEndDate is a light sanity check that a promo's
// date fragment is its END date, not its start.
func TestRenderTextPromoBodyNamesEndDate(t *testing.T) {
	s := eventSubject()
	s.Kind = domain.PushCampaignKindPromo
	s.EndsAt = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	got := renderText(s, "ru", time.UTC)
	if !strings.Contains(got.Body, "30.09") {
		t.Fatalf("promo body = %q, want it to mention the end date 30.09", got.Body)
	}
}
