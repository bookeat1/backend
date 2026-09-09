package restaurants

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// storedVenue is a venue as it really looks in production: the plain columns
// carry the Russian text AND a name_i18n/description_i18n map repeats it, with
// other languages next to it.
func storedVenue(id uuid.UUID) *domain.RestaurantAggregate {
	return &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		ID:               id,
		Name:             "Старое имя",
		NameI18n:         domain.I18n{"ru": "Старое имя", "kk": "Ескі атауы", "en": "Old name"},
		Description:      "Старое описание",
		DescriptionI18n:  domain.I18n{"ru": "Старое описание", "en": "Old description"},
		Address:          "Улица 1",
		AddressI18n:      domain.I18n{"ru": "Улица 1", "kk": "Көше 1"},
		OpeningHours:     "10:00–22:00",
		OpeningHoursI18n: domain.I18n{"ru": "10:00–22:00", "en": "10am–10pm"},
		City:             domain.CityAlmaty,
		PriceCategory:    domain.PriceLow,
	}}
}

// TestUpdateRenameRewritesRussianTranslation is THE regression. Before this,
// PATCH {name} wrote a column that no read returns: every read resolves
// name_i18n["ru"] first, so the cabinet was shown the old name and sent it
// straight back on the next save — the rename undid itself.
func TestUpdateRenameRewritesRussianTranslation(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{Name: strp("Новое имя")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got == nil {
		t.Fatal("expected the venue to be updated")
	}
	if got.Name != "Новое имя" {
		t.Errorf("Name = %q, want the new name in the column", got.Name)
	}
	if got.NameI18n["ru"] != "Новое имя" {
		t.Errorf(`NameI18n["ru"] = %q, want it rewritten to the new name — otherwise every read still returns the old one`, got.NameI18n["ru"])
	}
	// What a read would actually serve to a Russian-speaking client.
	if resolved := got.NameI18n.Resolve(domain.LocaleRU, got.Name); resolved != "Новое имя" {
		t.Errorf("a ru read returns %q, want the new name", resolved)
	}
	if got.NameI18n["kk"] != "Ескі атауы" || got.NameI18n["en"] != "Old name" {
		t.Errorf("NameI18n = %v, want kk/en preserved by a Russian rename", got.NameI18n)
	}
}

// TestUpdateRenameIsIdempotent: saving twice must not resurrect anything. The
// second save carries the value the first one produced, which is what the panel
// sends after re-reading the venue.
func TestUpdateRenameIsIdempotent(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{Name: strp("Новое имя")}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	repo.agg = &domain.RestaurantAggregate{Restaurant: *repo.updated}
	if _, err := f.Update(context.Background(), id, SaveInput{Name: strp("Новое имя")}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if repo.updated.Name != "Новое имя" || repo.updated.NameI18n["ru"] != "Новое имя" {
		t.Errorf("after the second save: Name=%q ru=%q, want both to be the new name",
			repo.updated.Name, repo.updated.NameI18n["ru"])
	}
	if repo.updated.NameI18n["kk"] != "Ескі атауы" {
		t.Errorf("NameI18n = %v, want kk still there after two saves", repo.updated.NameI18n)
	}
}

// TestUpdateSyncsEveryLocalizedTextColumn: description/address/opening_hours
// have the same read-through-the-map behaviour as the name, so they must be
// fixed the same way — a cabinet that edits the description of a venue with a
// ru translation could not change it at all before.
func TestUpdateSyncsEveryLocalizedTextColumn(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{
		Description:  strp("Новое описание"),
		Address:      strp("Улица 2"),
		OpeningHours: strp("09:00–23:00"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got.DescriptionI18n["ru"] != "Новое описание" || got.DescriptionI18n["en"] != "Old description" {
		t.Errorf("DescriptionI18n = %v, want ru rewritten and en preserved", got.DescriptionI18n)
	}
	if got.AddressI18n["ru"] != "Улица 2" || got.AddressI18n["kk"] != "Көше 1" {
		t.Errorf("AddressI18n = %v, want ru rewritten and kk preserved", got.AddressI18n)
	}
	if got.OpeningHoursI18n["ru"] != "09:00–23:00" || got.OpeningHoursI18n["en"] != "10am–10pm" {
		t.Errorf("OpeningHoursI18n = %v, want ru rewritten and en preserved", got.OpeningHoursI18n)
	}
	// The name was not in the request, so neither its column nor its map moved.
	if got.Name != "Старое имя" || got.NameI18n["ru"] != "Старое имя" {
		t.Errorf("an omitted field changed: Name=%q ru=%q", got.Name, got.NameI18n["ru"])
	}
}

// TestUpdateCreatesTranslationMapForVenueWithout: a venue whose i18n columns
// are NULL must end up with a map that agrees with the column, so the very
// first ru translation added later cannot silently outrank the column.
func TestUpdateCreatesTranslationMapForVenueWithout(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		ID: id, Name: "Без переводов", City: domain.CityAlmaty, PriceCategory: domain.PriceLow,
	}}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{Name: strp("Новое имя")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated.NameI18n["ru"] != "Новое имя" {
		t.Errorf("NameI18n = %v, want {ru: новое имя} created", repo.updated.NameI18n)
	}
	// Untouched fields stay NULL — a PATCH must not invent translations for
	// fields it never mentioned.
	if repo.updated.DescriptionI18n != nil {
		t.Errorf("DescriptionI18n = %v, want it left NULL", repo.updated.DescriptionI18n)
	}
}

// TestUpdateClearingTextDropsRussianButKeepsOtherLanguages documents the edge
// case: an empty value means "there is no Russian text", not "the Kazakh
// translation is wrong".
func TestUpdateClearingTextDropsRussianButKeepsOtherLanguages(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{Description: strp("")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if _, ok := got.DescriptionI18n["ru"]; ok {
		t.Errorf("DescriptionI18n = %v, want the ru entry dropped", got.DescriptionI18n)
	}
	if got.DescriptionI18n["en"] != "Old description" {
		t.Errorf("DescriptionI18n = %v, want en preserved", got.DescriptionI18n)
	}
	if resolved := got.DescriptionI18n.Resolve(domain.LocaleRU, got.Description); resolved != "" {
		t.Errorf("a ru read returns %q, want the cleared (empty) description", resolved)
	}
}

// TestUpdateExplicitMapLosesToThePlainFieldForRussian: when a client sends both
// (the admin panel's current workaround does), the column is Russian by
// definition and wins, while the other languages in the sent map are honoured.
func TestUpdateExplicitMapLosesToThePlainFieldForRussian(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{
		Name:     strp("Новое имя"),
		NameI18n: domain.I18nPatch{"ru": strp("Старое имя"), "en": strp("Brand new")},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.updated.NameI18n["ru"] != "Новое имя" {
		t.Errorf(`NameI18n["ru"] = %q, want the plain name to win`, repo.updated.NameI18n["ru"])
	}
	if repo.updated.NameI18n["en"] != "Brand new" {
		t.Errorf(`NameI18n["en"] = %q, want the sent translation kept`, repo.updated.NameI18n["en"])
	}
}

// TestCreateSyncsRussianTranslation: a venue created through the API must start
// out consistent, or it is born with the same trap.
func TestCreateSyncsRussianTranslation(t *testing.T) {
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	in := validInput()
	in.Name = strp("Новое заведение")
	if _, err := f.Create(context.Background(), in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.created.NameI18n["ru"] != "Новое заведение" {
		t.Errorf("NameI18n = %v, want ru to mirror the name column", repo.created.NameI18n)
	}
}

// TestUpdateDoesNotMutateTheStoredMap guards the copy-on-write in
// domain.I18n.WithLocale: the map the facade edits comes straight out of the
// aggregate it just read, and mutating it in place would corrupt any other
// holder of that row (and make a failed transaction leave a changed value
// behind in memory).
func TestUpdateDoesNotMutateTheStoredMap(t *testing.T) {
	id := uuid.New()
	stored := storedVenue(id)
	repo := &fakeRestaurantRepo{agg: stored}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{Name: strp("Новое имя")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if stored.NameI18n["ru"] != "Старое имя" {
		t.Errorf("the map read from the repo was mutated in place: %v", stored.NameI18n)
	}
}

// --- writing translations, not just syncing ru (2026-08-29) ---

// The venue's description/address/cuisine/hours have HAD translation columns
// since the import, and there was no way to write them: the cabinet could only
// change the Russian column, and the 14 Kazakh descriptions in production were
// effectively read-only. This is that hole closed.
func TestUpdateWritesEveryLocalizedField(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{
		DescriptionI18n:  domain.I18nPatch{"kk": strp("Жаңа сипаттама")},
		CuisineTypeI18n:  domain.I18nPatch{"kk": strp("Қазақ асханасы")},
		AddressI18n:      domain.I18nPatch{"en": strp("1 Street")},
		OpeningHoursI18n: domain.I18nPatch{"kk": strp("10:00–22:00 KK")},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got.DescriptionI18n["kk"] != "Жаңа сипаттама" {
		t.Errorf("DescriptionI18n[kk] = %q", got.DescriptionI18n["kk"])
	}
	if got.CuisineTypeI18n["kk"] != "Қазақ асханасы" {
		t.Errorf("CuisineTypeI18n[kk] = %q", got.CuisineTypeI18n["kk"])
	}
	if got.AddressI18n["en"] != "1 Street" {
		t.Errorf("AddressI18n[en] = %q", got.AddressI18n["en"])
	}
	if got.OpeningHoursI18n["kk"] != "10:00–22:00 KK" {
		t.Errorf("OpeningHoursI18n[kk] = %q", got.OpeningHoursI18n["kk"])
	}
	// A guest asking for Kazakh gets the translation; asking for English, where
	// only the address has one, falls back to the Russian column.
	if v := got.DescriptionI18n.Resolve(domain.LocaleKK, got.Description); v != "Жаңа сипаттама" {
		t.Errorf("kk read = %q", v)
	}
	if v := got.OpeningHoursI18n.Resolve(domain.LocaleEN, got.OpeningHours); v != "10am–10pm" {
		t.Errorf("en read = %q, want the stored English translation", v)
	}
}

// The neighbour-language rule: a patch that names only kk must not cost the
// venue its English. This is why the request carries a PATCH and not a map.
func TestUpdateTranslationPatchKeepsOtherLanguages(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{
		DescriptionI18n: domain.I18nPatch{"kk": strp("Қазақша")},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got.DescriptionI18n["en"] != "Old description" {
		t.Errorf("DescriptionI18n[en] = %q, want the English translation untouched", got.DescriptionI18n["en"])
	}
	if got.DescriptionI18n["ru"] != "Старое описание" || got.Description != "Старое описание" {
		t.Errorf("the Russian text moved on a Kazakh-only edit: %q / %v", got.Description, got.DescriptionI18n)
	}
}

// Removing one language is a REQUEST, not an omission: an explicit null does
// it, and nothing else does.
func TestUpdateTranslationNullRemovesOnlyThatLanguage(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{
		AddressI18n: domain.I18nPatch{"kk": nil},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if _, ok := got.AddressI18n["kk"]; ok {
		t.Error("an explicit null must remove the Kazakh address")
	}
	if got.AddressI18n["ru"] != "Улица 1" {
		t.Errorf("AddressI18n[ru] = %q, want the base language untouched", got.AddressI18n["ru"])
	}
}

// A `ru` key writes the COLUMN, because that column is the Russian text. Any
// other reading would store a translation the reads prefer and leave the
// venue's real address stale everywhere else in the system.
func TestUpdateRussianInsideThePatchWritesTheColumn(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: storedVenue(id)}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{
		AddressI18n: domain.I18nPatch{"ru": strp("Улица 2")},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got.Address != "Улица 2" {
		t.Errorf("Address column = %q, want the ru value promoted to it", got.Address)
	}
	if got.AddressI18n["ru"] != "Улица 2" {
		t.Errorf("AddressI18n[ru] = %q, want it equal to the column", got.AddressI18n["ru"])
	}
	if got.AddressI18n["kk"] != "Көше 1" {
		t.Errorf("AddressI18n[kk] = %q, want it kept", got.AddressI18n["kk"])
	}
}

// Writing the Russian cuisine string re-syncs its map too. Before this the
// column was written and the stale ru translation kept being read back — the
// same trap the rename fix closed for the name.
func TestUpdateCuisineTypeSyncsItsRussianTranslation(t *testing.T) {
	id := uuid.New()
	stored := storedVenue(id)
	stored.CuisineType = "Итальянская"
	stored.CuisineTypeI18n = domain.I18n{"ru": "Итальянская", "kk": "Итальяндық"}
	repo := &fakeRestaurantRepo{agg: stored}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{
		CuisineType: strp("Грузинская"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.updated
	if got.CuisineTypeI18n["ru"] != "Грузинская" {
		t.Errorf(`CuisineTypeI18n["ru"] = %q, want it rewritten with the column`, got.CuisineTypeI18n["ru"])
	}
	if got.CuisineTypeI18n["kk"] != "Итальяндық" {
		t.Errorf("the Kazakh cuisine translation was lost: %v", got.CuisineTypeI18n)
	}
}

// A language nothing can serve is refused at the door (422), not stored: a
// translation no read can ever return is worse than no translation at all.
// (ko and zh used to be the example here; they are servable since 2026-09-02,
// so the example is now French, which is not.)
func TestUpdateRejectsUnsupportedAndAmbiguousLanguages(t *testing.T) {
	id := uuid.New()
	f := NewFacade(&fakeRestaurantRepo{agg: storedVenue(id)}, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	for name, in := range map[string]SaveInput{
		"unsupported":  {DescriptionI18n: domain.I18nPatch{"fr": strp("Joli endroit")}},
		"ambiguous":    {DescriptionI18n: domain.I18nPatch{"kk": strp("а"), "kk-KZ": strp("б")}},
		"deleting ru":  {DescriptionI18n: domain.I18nPatch{"ru": nil}},
		"blanking ru":  {AddressI18n: domain.I18nPatch{"ru": strp(" ")}},
		"in opening h": {OpeningHoursI18n: domain.I18nPatch{"fr": strp("dix heures")}},
	} {
		if _, err := f.Update(context.Background(), id, in); err == nil {
			t.Errorf("%s: want a validation error, got nil", name)
		}
	}
}
