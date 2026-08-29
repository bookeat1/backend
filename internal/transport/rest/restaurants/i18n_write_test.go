package restaurants

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The cabinet edits translations, so the cabinet read has to SHOW them. Before
// this only name_i18n travelled, and the venue's description/address/hours
// translations were invisible to the only screen that could fix them.
func TestAdminGetCarriesEveryTranslationMap(t *testing.T) {
	id := uuid.New()
	agg := hiddenVenue(id)
	agg.Description = "Уютное место"
	agg.DescriptionI18n = domain.I18n{"ru": "Уютное место", "kk": "Жайлы орын"}
	agg.Address = "Улица 1"
	agg.AddressI18n = domain.I18n{"ru": "Улица 1", "en": "1 Street"}
	agg.OpeningHours = "10:00–22:00"
	agg.OpeningHoursI18n = domain.I18n{"ru": "10:00–22:00", "kk": "10:00–22:00 KK"}
	agg.CuisineType = "Грузинская"
	agg.CuisineTypeI18n = domain.I18n{"ru": "Грузинская", "kk": "Грузин асханасы"}
	r := newScopedRouter(&fakeFacade{agg: agg})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/restaurants/"+id.String(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for field, lang := range map[string]string{
		"description_i18n":   "kk",
		"address_i18n":       "en",
		"opening_hours_i18n": "kk",
		"cuisine_type_i18n":  "kk",
	} {
		m, ok := env.Data[field].(map[string]any)
		if !ok {
			t.Errorf("%s is missing from the cabinet read: %v", field, env.Data[field])
			continue
		}
		if m[lang] == nil || m[lang] == "" {
			t.Errorf("%s[%s] = %v, want the stored translation", field, lang, m[lang])
		}
	}
}

// A guest gets the resolved text and NOT the raw maps: the catalog is the
// app's hottest read and must not carry three languages of every description.
func TestPublicListingCarriesNoRawTranslationMaps(t *testing.T) {
	agg := hiddenVenue(uuid.New())
	agg.DescriptionI18n = domain.I18n{"ru": "Уютное место", "kk": "Жайлы орын"}
	body, err := json.Marshal(listItemToResponse(domain.RestaurantListItem{Restaurant: agg.Restaurant}, domain.LocaleKK))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"description_i18n", "address_i18n", "opening_hours_i18n", "cuisine_type_i18n"} {
		if _, ok := got[k]; ok {
			t.Errorf("the catalog card must not carry %s", k)
		}
	}
	if got["description"] != "Жайлы орын" {
		t.Errorf("description = %v, want the Kazakh translation resolved", got["description"])
	}
}

// The wire shape the admin panel will implement: an object per field, three
// states per language. null must survive JSON decoding as a PRESENT key with a
// nil value, or "remove this language" is indistinguishable from "leave it".
func TestSaveRequestParsesTranslationPatch(t *testing.T) {
	var req saveRestaurantRequest
	body := `{"description":"Уютное место",
	          "description_i18n":{"kk":"Жайлы орын","en":null},
	          "address_i18n":{"ru":"Улица 2"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	in, err := req.toInput()
	if err != nil {
		t.Fatalf("toInput: %v", err)
	}
	if v := in.DescriptionI18n["kk"]; v == nil || *v != "Жайлы орын" {
		t.Errorf("description_i18n[kk] = %v", in.DescriptionI18n["kk"])
	}
	v, ok := in.DescriptionI18n["en"]
	if !ok {
		t.Fatal("an explicit null must be PRESENT in the patch, or it cannot mean 'remove'")
	}
	if v != nil {
		t.Errorf("description_i18n[en] = %q, want nil", *v)
	}
	if ru, ok := in.AddressI18n.Russian(); !ok || ru != "Улица 2" {
		t.Errorf("address_i18n[ru] = %q/%v, want it available for promotion to the column", ru, ok)
	}
}

// A field the body does not mention is not a patch at all: nil means "leave
// every language of this field alone", which is what read-modify-write needs.
func TestSaveRequestOmittedTranslationObjectIsNil(t *testing.T) {
	var req saveRestaurantRequest
	if err := json.Unmarshal([]byte(`{"name":"Кафе"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	in, err := req.toInput()
	if err != nil {
		t.Fatalf("toInput: %v", err)
	}
	if in.DescriptionI18n != nil || in.NameI18n != nil || in.AddressI18n != nil ||
		in.OpeningHoursI18n != nil || in.CuisineTypeI18n != nil {
		t.Error("an absent *_i18n object must arrive as a nil patch")
	}
}
