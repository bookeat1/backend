package pushcampaigns

import (
	"strings"
	"testing"
	"time"

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

	en := renderText(s, "en", loc)
	if !strings.Contains(en.Body, "Джазовый вечер") {
		t.Fatalf("en body (no translation) = %q, want the Russian fallback", en.Body)
	}

	ru := renderText(s, "ru", loc)
	if !strings.Contains(ru.Title, "«Абай»") {
		t.Fatalf("ru title = %q, want it to name the venue", ru.Title)
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

// TestRenderTextBodyNeverExceedsLimit pins criterion 20's "тело ≤ 120 символов".
func TestRenderTextBodyNeverExceedsLimit(t *testing.T) {
	s := eventSubject()
	s.Title = strings.Repeat("Очень длинное название события ", 10)
	got := renderText(s, "ru", time.UTC)
	if len(got.Body) > maxBodyLen {
		t.Fatalf("body length = %d, want <= %d", len(got.Body), maxBodyLen)
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
