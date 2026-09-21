package restaurants

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

var testRulesDefaults = BookingRulesDefaults{
	HoldMinutes:                    15,
	LateArrivalText:                "Опаздываете — позвоните в заведение.",
	DefaultFreeCancelWindowMinutes: 120,
}

// --- ResolveBookingRules: pure resolution, no repository involved ---

func TestResolveBookingRulesFallsBackToPlatformDefaults(t *testing.T) {
	r := domain.Restaurant{} // nothing overridden, no venue-side data loaded at all
	got := ResolveBookingRules(r, testRulesDefaults, domain.LocaleRU)
	want := domain.EffectiveBookingRules{HoldMinutes: 15, FreeCancelHours: 2, LateArrivalText: testRulesDefaults.LateArrivalText}
	if got != want {
		t.Errorf("ResolveBookingRules(zero value) = %+v, want %+v", got, want)
	}
}

func TestResolveBookingRulesHonoursVenueOverrides(t *testing.T) {
	hold := 30
	text := "Опоздали? Звоните хостес."
	r := domain.Restaurant{
		BookingRules:            domain.BookingRulesOverride{HoldMinutes: &hold, LateArrivalText: &text},
		FreeCancelWindowMinutes: intp(180),
	}
	got := ResolveBookingRules(r, testRulesDefaults, domain.LocaleRU)
	want := domain.EffectiveBookingRules{HoldMinutes: 30, FreeCancelHours: 3, LateArrivalText: text}
	if got != want {
		t.Errorf("ResolveBookingRules(overridden) = %+v, want %+v", got, want)
	}
}

// A non-positive stored HoldMinutes is nonsense that should never have been
// written by this endpoint (validateProvided refuses it going forward), but a
// legacy/hand-written row is not trusted either — same discipline as
// usecase/bookings.resolvePolicy.
func TestResolveBookingRulesIgnoresNonsenseOverride(t *testing.T) {
	zero := 0
	r := domain.Restaurant{BookingRules: domain.BookingRulesOverride{HoldMinutes: &zero}}
	got := ResolveBookingRules(r, testRulesDefaults, domain.LocaleRU)
	if got.HoldMinutes != testRulesDefaults.HoldMinutes {
		t.Errorf("HoldMinutes = %d, want the platform default for a non-positive override", got.HoldMinutes)
	}
}

// FreeCancelHours must track restaurants.free_cancel_window_minutes — the
// MONEY path — never an independent value: this is the whole point of not
// giving free cancellation its own override column.
func TestResolveBookingRulesFreeCancelHoursTracksMoneyPathWindow(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		hours   int
	}{
		{minutes: 60, hours: 1},
		{minutes: 90, hours: 2},  // rounds to nearest hour
		{minutes: 119, hours: 2}, // rounds to nearest hour
		{minutes: 0, hours: 0},
	} {
		r := domain.Restaurant{FreeCancelWindowMinutes: intp(tc.minutes)}
		got := ResolveBookingRules(r, testRulesDefaults, domain.LocaleRU)
		if got.FreeCancelHours != tc.hours {
			t.Errorf("minutes=%d: FreeCancelHours = %d, want %d", tc.minutes, got.FreeCancelHours, tc.hours)
		}
	}
}

// LateArrivalText resolves through the venue's own translations, never the
// platform default's (which carries none).
func TestResolveBookingRulesResolvesLateArrivalTextLocale(t *testing.T) {
	text := "Позвоните нам"
	r := domain.Restaurant{BookingRules: domain.BookingRulesOverride{
		LateArrivalText:     &text,
		LateArrivalTextI18n: domain.I18n{"ru": text, "kk": "Бізге қоңырау шалыңыз"},
	}}
	if got := ResolveBookingRules(r, testRulesDefaults, "kk").LateArrivalText; got != "Бізге қоңырау шалыңыз" {
		t.Errorf("kk resolve = %q, want the Kazakh translation", got)
	}
	if got := ResolveBookingRules(r, testRulesDefaults, "en").LateArrivalText; got != text {
		t.Errorf("en resolve = %q, want the fallback (base) text — no English translation stored", got)
	}
}

// --- facade.Update: writing hold_minutes / late_arrival_text(+i18n) ---

func TestUpdateWritesBookingRulesOverride(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{
		HoldMinutes: intp(20), LateArrivalText: strp("Опоздали? Звоните!"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.rulesID != id {
		t.Fatalf("UpdateBookingRules was not called for %s", id)
	}
	if got := repo.rules.HoldMinutes; got == nil || *got != 20 {
		t.Errorf("HoldMinutes = %v, want 20", got)
	}
	if got := repo.rules.LateArrivalText; got == nil || *got != "Опоздали? Звоните!" {
		t.Errorf("LateArrivalText = %v, want the new text", got)
	}
	// The ru entry of the i18n map is kept in sync, same discipline as every
	// other localized field (syncRussianTranslations).
	if repo.rules.LateArrivalTextI18n["ru"] != "Опоздали? Звоните!" {
		t.Errorf("LateArrivalTextI18n[ru] = %q, want it synced to the column", repo.rules.LateArrivalTextI18n["ru"])
	}
}

// A request that never mentions the booking-rules fields must not call
// UpdateBookingRules at all — an unrelated PATCH (e.g. just `phone`) has no
// business touching this venue's copy override.
func TestUpdateSkipsBookingRulesWhenNotTouched(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{Phone: strp("+77011234567")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.rulesID != uuid.Nil {
		t.Errorf("UpdateBookingRules was called for %s, want it skipped", repo.rulesID)
	}
}

// hold_minutes=0 and an empty late_arrival_text are the "reset to the
// platform default" sentinel (Repository.UpdateBookingRules), not a
// validation error — and clearing the text also clears its translations, or
// the CHECK (late_arrival_text_i18n needs late_arrival_text) would refuse it.
func TestUpdateClearingHoldMinutesAndLateArrivalTextIsAllowed(t *testing.T) {
	hold, text := 20, "Звоните"
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		ID: id,
		BookingRules: domain.BookingRulesOverride{
			HoldMinutes: &hold, LateArrivalText: &text, LateArrivalTextI18n: domain.I18n{"ru": text, "kk": "Қоңырау шалыңыз"},
		},
	}}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{HoldMinutes: intp(0), LateArrivalText: strp("")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.rules.HoldMinutes == nil || *repo.rules.HoldMinutes != 0 {
		t.Errorf("HoldMinutes = %v, want the clear sentinel (0) forwarded to the repository", repo.rules.HoldMinutes)
	}
	if repo.rules.LateArrivalText == nil || *repo.rules.LateArrivalText != "" {
		t.Errorf("LateArrivalText = %v, want the clear sentinel (empty string) forwarded", repo.rules.LateArrivalText)
	}
	if repo.rules.LateArrivalTextI18n != nil {
		t.Errorf("LateArrivalTextI18n = %v, want it cleared alongside the base text", repo.rules.LateArrivalTextI18n)
	}
}

// A translation-only PATCH (no late_arrival_text in the request) merges onto
// the STORED text and translations, so a cabinet can add a kk translation
// without resending the Russian text it already saved.
func TestUpdateTranslatesLateArrivalTextWithoutResendingBaseText(t *testing.T) {
	text := "Звоните"
	id := uuid.New()
	repo := &fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		ID:           id,
		BookingRules: domain.BookingRulesOverride{LateArrivalText: &text, LateArrivalTextI18n: domain.I18n{"ru": text}},
	}}}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	_, err := f.Update(context.Background(), id, SaveInput{
		LateArrivalTextI18n: domain.I18nPatch{"kk": strp("Қоңырау шалыңыз")},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	// LateArrivalText itself is nil — "untouched" — NOT the current value:
	// Repository.UpdateBookingRules tells "clear" from "untouched" by the
	// pointer, so resending the unchanged text here would be indistinguishable
	// from an explicit (no-op) rewrite. i18nTouched is what tells the
	// repository to persist the merged translations map on its own.
	if repo.rules.LateArrivalText != nil {
		t.Errorf("LateArrivalText = %v, want nil (untouched) for a translation-only PATCH", repo.rules.LateArrivalText)
	}
	if !repo.rulesI18nTouched {
		t.Error("want i18nTouched = true for a translation-only PATCH")
	}
	if repo.rules.LateArrivalTextI18n["kk"] != "Қоңырау шалыңыз" {
		t.Errorf("LateArrivalTextI18n[kk] = %q, want the new translation", repo.rules.LateArrivalTextI18n["kk"])
	}
	if repo.rules.LateArrivalTextI18n["ru"] != text {
		t.Errorf("LateArrivalTextI18n[ru] = %q, want the stored base text preserved in the merged map", repo.rules.LateArrivalTextI18n["ru"])
	}
}

func TestUpdateRejectsNegativeHoldMinutes(t *testing.T) {
	id := uuid.New()
	f := NewFacade(&fakeRestaurantRepo{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}},
		&fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	if _, err := f.Update(context.Background(), id, SaveInput{HoldMinutes: intp(-5)}); err == nil {
		t.Fatal("want a validation error for a negative hold_minutes")
	}
}
