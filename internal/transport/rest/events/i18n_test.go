package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// localizedEvent is an event as it looks once someone has actually translated
// it: every text column Russian, the maps carrying kk (and the button also en).
func localizedEvent() domain.Event {
	rid := uuid.New()
	return domain.Event{
		ID:              uuid.New(),
		RestaurantID:    &rid,
		Title:           "Греческая вечеринка",
		TitleI18n:       domain.I18n{"ru": "Греческая вечеринка", "kk": "Грек кеші"},
		Description:     "Сиртаки и узо",
		DescriptionI18n: domain.I18n{"ru": "Сиртаки и узо"},
		Venue:           "Летняя терраса",
		VenueI18n:       domain.I18n{"ru": "Летняя терраса", "kk": "Жазғы террасса"},
		Status:          domain.EventPublished,
		StartsAt:        time.Now(),
		EndsAt:          time.Now().Add(3 * time.Hour),
		Action: &domain.EventAction{
			Label:     "Купить билет",
			LabelI18n: domain.I18n{"ru": "Купить билет", "en": "Buy a ticket"},
		},
	}
}

// A guest asking for Kazakh gets the venue line translated and the button
// caption in Russian — the fallback is per FIELD, not per card, and it never
// invents a translation.
func TestPublicResponseResolvesVenueAndActionLabel(t *testing.T) {
	e := localizedEvent()
	r := publicResponse(e, domain.LocaleKK)

	if r.Venue != "Жазғы террасса" {
		t.Errorf("venue = %q, want the Kazakh translation", r.Venue)
	}
	if r.Description != "Сиртаки и узо" {
		t.Errorf("description = %q, want the Russian fallback (no kk translation exists)", r.Description)
	}
	if r.Action == nil || r.Action.Label != "Купить билет" {
		t.Errorf("action label = %#v, want the Russian fallback", r.Action)
	}

	// English: the button has a translation, the venue does not.
	r = publicResponse(e, domain.LocaleEN)
	if r.Action.Label != "Buy a ticket" {
		t.Errorf("action label = %q, want the English caption", r.Action.Label)
	}
	if r.Venue != "Летняя терраса" {
		t.Errorf("venue = %q, want the Russian fallback", r.Venue)
	}
}

// A caller that asks for nothing gets exactly what the columns hold, so an old
// store build sees byte-identical output.
func TestPublicResponseWithoutALanguageLeavesTheColumns(t *testing.T) {
	r := publicResponse(localizedEvent(), "")
	if r.Venue != "Летняя терраса" || r.Action.Label != "Купить билет" {
		t.Errorf("no language asked for, got venue %q / label %q", r.Venue, r.Action.Label)
	}
}

// The guest shape carries no raw maps: the server already resolved the text,
// and shipping three languages of everything would pay for an editing screen
// on the app's hottest read.
func TestPublicResponseDropsRawTranslationMaps(t *testing.T) {
	body, err := json.Marshal(publicResponse(localizedEvent(), domain.LocaleKK))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"title_i18n", "description_i18n", "venue_i18n"} {
		if _, ok := got[k]; ok {
			t.Errorf("guest payload must not carry %s", k)
		}
	}
	action, _ := got["action"].(map[string]any)
	if _, ok := action["label_i18n"]; ok {
		t.Error("guest payload must not carry action.label_i18n")
	}
}

// The cabinet edits what it is shown, so it is shown the columns and the raw
// maps — never a resolved translation it would then post back as the column.
func TestAdminResponseCarriesRawTranslationMaps(t *testing.T) {
	body, err := json.Marshal(adminResponse(localizedEvent()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	venue, ok := got["venue_i18n"].(map[string]any)
	if !ok || venue["kk"] != "Жазғы террасса" {
		t.Fatalf("venue_i18n = %v, want the raw map", got["venue_i18n"])
	}
	if got["venue"] != "Летняя терраса" {
		t.Errorf("venue = %v, want the stored column", got["venue"])
	}
	action, _ := got["action"].(map[string]any)
	label, ok := action["label_i18n"].(map[string]any)
	if !ok || label["en"] != "Buy a ticket" {
		t.Fatalf("action.label_i18n = %v, want the raw map", action["label_i18n"])
	}
}

// The translation objects on the wire are PATCHES: null means "remove this
// language", which a map[string]string could not express at all.
func TestEventRequestParsesTranslationPatch(t *testing.T) {
	var req eventRequest
	body := `{"venue":"Терраса","venue_i18n":{"kk":"Террасса","en":null},
	          "action":{"label":"Купить","label_i18n":{"en":"Buy"}}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := req.VenueI18n["kk"]; !ok || v == nil || *v != "Террасса" {
		t.Errorf("venue_i18n[kk] = %v", req.VenueI18n["kk"])
	}
	v, ok := req.VenueI18n["en"]
	if !ok {
		t.Fatal("an explicit null must be PRESENT in the patch, or it cannot mean 'remove'")
	}
	if v != nil {
		t.Errorf("venue_i18n[en] = %q, want nil", *v)
	}
	if l := req.Action.labelI18n(); l["en"] == nil || *l["en"] != "Buy" {
		t.Errorf("action.label_i18n = %v", l)
	}
}
