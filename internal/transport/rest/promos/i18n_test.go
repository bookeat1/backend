package promos

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// localizedPromo carries a translated fine print and an untranslated title —
// the normal half-translated state of real content.
func localizedPromo() domain.Promo {
	rid := uuid.New()
	return domain.Promo{
		ID:           uuid.New(),
		RestaurantID: &rid,
		Title:        "Счастливые часы",
		TitleI18n:    domain.I18n{"ru": "Счастливые часы"},
		Description:  "С 17 до 19",
		Terms:        "Только зал, не суммируется",
		TermsI18n: domain.I18n{
			"ru": "Только зал, не суммируется",
			"kk": "Тек залда, басқа акциялармен қосылмайды",
		},
		Status:   domain.PromoPublished,
		StartsAt: time.Now(),
		EndsAt:   time.Now().Add(time.Hour),
	}
}

// A guest asking for Kazakh gets the translated fine print and the Russian
// title, because only one of the two has been translated.
func TestPublicResponseResolvesTerms(t *testing.T) {
	r := publicResponse(localizedPromo(), domain.LocaleKK)
	if r.Terms != "Тек залда, басқа акциялармен қосылмайды" {
		t.Errorf("terms = %q, want the Kazakh translation", r.Terms)
	}
	if r.Title != "Счастливые часы" {
		t.Errorf("title = %q, want the Russian fallback", r.Title)
	}
	if r.TermsI18n != nil {
		t.Errorf("the guest shape must not carry the raw map, got %v", r.TermsI18n)
	}
}

// No language asked for = the columns, unchanged, for every store build that
// never sends Accept-Language.
func TestPublicResponseWithoutALanguageLeavesTheColumns(t *testing.T) {
	r := publicResponse(localizedPromo(), "")
	if r.Terms != "Только зал, не суммируется" {
		t.Errorf("terms = %q, want the stored column", r.Terms)
	}
}

// The cabinet is shown the column plus the raw map, so it edits what it writes.
func TestAdminResponseCarriesRawTermsMap(t *testing.T) {
	body, err := json.Marshal(adminResponse(localizedPromo()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	terms, ok := got["terms_i18n"].(map[string]any)
	if !ok || terms["kk"] == "" {
		t.Fatalf("terms_i18n = %v, want the raw map", got["terms_i18n"])
	}
	if got["terms"] != "Только зал, не суммируется" {
		t.Errorf("terms = %v, want the stored Russian column", got["terms"])
	}
}

// null in a translation object means "remove this language" — the state the
// old full-replace map could not express.
func TestPromoRequestParsesTranslationPatch(t *testing.T) {
	var req promoRequest
	if err := json.Unmarshal([]byte(`{"terms":"Только зал","terms_i18n":{"kk":"Тек залда","en":null}}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v := req.TermsI18n["kk"]; v == nil || *v != "Тек залда" {
		t.Errorf("terms_i18n[kk] = %v", req.TermsI18n["kk"])
	}
	v, ok := req.TermsI18n["en"]
	if !ok {
		t.Fatal("an explicit null must be PRESENT in the patch, or it cannot mean 'remove'")
	}
	if v != nil {
		t.Errorf("terms_i18n[en] = %q, want nil", *v)
	}
}
