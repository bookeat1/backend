package appversion

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fake repository --------------------------------------------------------

type fakeRepo struct {
	rows    map[domain.DevicePlatform]domain.MobileAppPolicy
	getErr  error
	saveErr error
	upserts int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[domain.DevicePlatform]domain.MobileAppPolicy{}}
}

func (f *fakeRepo) Get(_ context.Context, p domain.DevicePlatform) (*domain.MobileAppPolicy, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	row, ok := f.rows[p]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &row, nil
}

func (f *fakeRepo) List(context.Context) ([]domain.MobileAppPolicy, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make([]domain.MobileAppPolicy, 0, len(f.rows))
	for _, p := range []domain.DevicePlatform{domain.PlatformAndroid, domain.PlatformIOS} {
		if row, ok := f.rows[p]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeRepo) Upsert(_ context.Context, p *domain.MobileAppPolicy) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.upserts++
	f.rows[p.Platform] = *p
	return nil
}

var _ domain.MobileAppPolicyRepository = (*fakeRepo)(nil)

// --- fixtures ---------------------------------------------------------------

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }
func venueUser() Actor  { return Actor{UserID: uuid.New(), Role: domain.RoleRestaurant} }
func guest() Actor      { return Actor{UserID: uuid.New(), Role: domain.RoleUser} }

func seeded() *fakeRepo {
	r := newFakeRepo()
	r.rows[domain.PlatformIOS] = domain.MobileAppPolicy{
		Platform:              domain.PlatformIOS,
		MinSupportedVersion:   "1.5",
		MinRecommendedVersion: "1.7",
		StoreURL:              "https://apps.apple.com/app/id6757542577",
		RecommendedTitle:      "Доступно обновление",
		RecommendedMessage:    "Обновите приложение",
		RequiredTitle:         "Нужно обновить приложение",
		RequiredMessage:       "Эта версия больше не поддерживается",
		RequiredTitleI18n:     domain.I18n{"ru": "Нужно обновить приложение", "en": "Update required"},
	}
	r.rows[domain.PlatformAndroid] = domain.MobileAppPolicy{
		Platform:              domain.PlatformAndroid,
		MinSupportedVersion:   "1.2",
		MinRecommendedVersion: "1.9",
		StoreURL:              "https://play.google.com/store/apps/details?id=kz.bookeat.app",
		RecommendedTitle:      "Доступно обновление",
		RecommendedMessage:    "Обновите приложение",
		RequiredTitle:         "Нужно обновить приложение",
		RequiredMessage:       "Эта версия больше не поддерживается",
	}
	return r
}

func str(s string) *string { return &s }

// --- Check ------------------------------------------------------------------

// TestCheckPerPlatformThresholds: the two platforms are configured differently
// and the SAME client version gets different answers. A bug that read one row
// for both would pass every single-platform test and be caught only here.
func TestCheckPerPlatformThresholds(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	cases := []struct {
		platform domain.DevicePlatform
		version  string
		want     domain.AppUpdateAction
	}{
		{domain.PlatformIOS, "1.4", domain.AppUpdateRequired},
		{domain.PlatformAndroid, "1.4", domain.AppUpdateRecommended},
		{domain.PlatformIOS, "1.8", domain.AppUpdateNone},
		{domain.PlatformAndroid, "1.8", domain.AppUpdateRecommended},
		{domain.PlatformAndroid, "1.10", domain.AppUpdateNone},
		{domain.PlatformAndroid, "1.9", domain.AppUpdateNone},
		{domain.PlatformIOS, "1.5", domain.AppUpdateRecommended},
		{domain.PlatformIOS, "1.5.1", domain.AppUpdateRecommended},
	}
	for _, tc := range cases {
		got, err := u.Check(context.Background(), tc.platform, tc.version)
		if err != nil {
			t.Fatalf("Check(%s, %s): %v", tc.platform, tc.version, err)
		}
		if got.Action != tc.want {
			t.Errorf("Check(%s, %s).Action = %q, want %q", tc.platform, tc.version, got.Action, tc.want)
		}
		if got.Platform != tc.platform {
			t.Errorf("Check echoed platform %q, want %q", got.Platform, tc.platform)
		}
		if got.StoreURL == "" {
			t.Errorf("Check(%s) returned no store URL", tc.platform)
		}
	}
}

// TestCheckCarriesTheWordingOfTheChosenModeOnly. A client must not be able to
// render the blocking copy for a soft prompt, so only one mode's texts travel.
func TestCheckCarriesTheWordingOfTheChosenModeOnly(t *testing.T) {
	u := NewUseCase(seeded(), nil)

	forced, err := u.Check(context.Background(), domain.PlatformIOS, "1.0")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if forced.Title["ru"] != "Нужно обновить приложение" {
		t.Errorf("forced title = %q, want the required wording", forced.Title["ru"])
	}
	if forced.Title["en"] != "Update required" {
		t.Errorf("forced en title = %q, want the stored translation", forced.Title["en"])
	}
	if forced.Title["kk"] != "Нужно обновить приложение" {
		t.Errorf("a missing kk translation must fall back to Russian, got %q", forced.Title["kk"])
	}

	soft, err := u.Check(context.Background(), domain.PlatformIOS, "1.6")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if soft.Title["ru"] != "Доступно обновление" {
		t.Errorf("soft title = %q, want the recommended wording", soft.Title["ru"])
	}

	quiet, err := u.Check(context.Background(), domain.PlatformIOS, "2.0")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if quiet.Title != nil || quiet.Message != nil {
		t.Errorf("action=none must carry no wording, got title=%v message=%v", quiet.Title, quiet.Message)
	}
}

// TestCheckWithNoSettingsAtAll: an unconfigured platform is not an error and
// not a forced update — it is silence. This is the state the platform is in
// right after migration 0103 (both rows seeded with EMPTY thresholds) and the
// state a brand-new deployment is in before the seed even runs.
func TestCheckWithNoSettingsAtAll(t *testing.T) {
	// No row at all.
	empty := NewUseCase(newFakeRepo(), nil)
	for _, p := range []domain.DevicePlatform{domain.PlatformIOS, domain.PlatformAndroid} {
		d, err := empty.Check(context.Background(), p, "0.1")
		if err != nil {
			t.Fatalf("Check on an unconfigured platform must not fail: %v", err)
		}
		if d.Action != domain.AppUpdateNone {
			t.Errorf("Check(%s) with no row = %q, want none", p, d.Action)
		}
	}

	// A row exists but both thresholds are empty — exactly what 0103 seeds.
	repo := newFakeRepo()
	repo.rows[domain.PlatformIOS] = domain.MobileAppPolicy{
		Platform: domain.PlatformIOS,
		StoreURL: "https://apps.apple.com/app/id6757542577",
	}
	seededEmpty := NewUseCase(repo, nil)
	for _, v := range []string{"0.1", "1.0", "1.5", "99.99"} {
		d, err := seededEmpty.Check(context.Background(), domain.PlatformIOS, v)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if d.Action != domain.AppUpdateNone {
			t.Errorf("the seeded, unconfigured policy told %q to do %q; it must force nobody", v, d.Action)
		}
	}
}

// TestCheckOnGarbageVersion: a client sending nonsense is left alone even where
// a forced threshold IS configured.
func TestCheckOnGarbageVersion(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	for _, v := range []string{"", "   ", "abc", "null", "1.5.x", "'; DROP TABLE users; --"} {
		d, err := u.Check(context.Background(), domain.PlatformIOS, v)
		if err != nil {
			t.Fatalf("Check(%q) must not fail: %v", v, err)
		}
		if d.Action != domain.AppUpdateNone {
			t.Errorf("Check(%q) = %q, want none — garbage must never force an update", v, d.Action)
		}
	}
}

// TestCheckPropagatesADatabaseFailure. A read that failed is NOT "do nothing"
// dressed up as a 200: the caller answers with a status the client treats as
// "no answer", instead of us inventing a policy we could not read.
func TestCheckPropagatesADatabaseFailure(t *testing.T) {
	repo := seeded()
	boom := errors.New("connection refused")
	repo.getErr = boom
	if _, err := NewUseCase(repo, nil).Check(context.Background(), domain.PlatformIOS, "1.0"); !errors.Is(err, boom) {
		t.Fatalf("Check swallowed a database failure, err = %v", err)
	}
}

// --- Save: authorization ----------------------------------------------------

// TestOnlySuperadminMayWriteOrRead. The routes are mounted behind
// RequireRole(RoleAdmin); this is the defence-in-depth re-check, so a future
// re-mount on a wider group cannot hand the forced-update switch to venue staff.
func TestOnlySuperadminMayWriteOrRead(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	for _, a := range []Actor{venueUser(), guest(), {}} {
		if _, err := u.List(context.Background(), a); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("List as %q = %v, want ErrForbidden", a.Role, err)
		}
		_, err := u.Save(context.Background(), a, domain.PlatformIOS, SaveInput{MinSupportedVersion: str("9.9")})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("Save as %q = %v, want ErrForbidden", a.Role, err)
		}
	}
	if _, err := u.List(context.Background(), superadmin()); err != nil {
		t.Errorf("List as superadmin: %v", err)
	}
}

// TestForbiddenSaveWritesNothing: a refusal must not have reached the database.
func TestForbiddenSaveWritesNothing(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo, nil)
	if _, err := u.Save(context.Background(), venueUser(), domain.PlatformIOS, SaveInput{MinSupportedVersion: str("9.9")}); err == nil {
		t.Fatal("Save as venue staff must fail")
	}
	if repo.upserts != 0 {
		t.Errorf("a refused save still wrote %d rows", repo.upserts)
	}
}

// --- Save: behaviour --------------------------------------------------------

// TestSaveIsAPatchNotAReplace. Sending only the threshold must not blank the
// wording — full-replace admin writes are exactly how fields have been wiped in
// this codebase before.
func TestSaveIsAPatchNotAReplace(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo, nil)

	out, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{MinSupportedVersion: str("1.6")})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if out.MinSupportedVersion != "1.6" {
		t.Errorf("min_supported_version = %q, want 1.6", out.MinSupportedVersion)
	}
	if out.RequiredTitle != "Нужно обновить приложение" {
		t.Errorf("the untouched required title was lost: %q", out.RequiredTitle)
	}
	if out.RequiredTitleI18n["en"] != "Update required" {
		t.Errorf("the untouched en translation was lost: %v", out.RequiredTitleI18n)
	}
	if out.StoreURL == "" {
		t.Error("the untouched store URL was lost")
	}
	if out.MinRecommendedVersion != "1.7" {
		t.Errorf("the untouched recommended floor was lost: %q", out.MinRecommendedVersion)
	}
}

// TestSaveTranslationPatchKeepsOtherLanguages: editing Kazakh must not require
// resending English, and the Russian base stays mirrored in i18n["ru"].
func TestSaveTranslationPatchKeepsOtherLanguages(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo, nil)

	out, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS, SaveInput{
		RequiredTitleI18n: domain.I18nPatch{"kk": str("Қосымшаны жаңарту қажет")},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if out.RequiredTitleI18n["kk"] != "Қосымшаны жаңарту қажет" {
		t.Errorf("kk not written: %v", out.RequiredTitleI18n)
	}
	if out.RequiredTitleI18n["en"] != "Update required" {
		t.Errorf("en was lost by a kk-only patch: %v", out.RequiredTitleI18n)
	}
	if out.RequiredTitleI18n["ru"] != out.RequiredTitle {
		t.Errorf("the ru entry must mirror the base column: %q vs %q",
			out.RequiredTitleI18n["ru"], out.RequiredTitle)
	}
}

// TestSaveSwitchesForcingOff. An EMPTY threshold means "no threshold" — this is
// the emergency off switch and it must actually clear the row.
func TestSaveSwitchesForcingOff(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo, nil)

	out, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{MinSupportedVersion: str("")})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if out.MinSupportedVersion != "" {
		t.Fatalf("min_supported_version = %q, want empty", out.MinSupportedVersion)
	}
	d, err := u.Check(context.Background(), domain.PlatformIOS, "0.1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Action == domain.AppUpdateRequired {
		t.Error("clearing the forced floor did not stop the blocking screen")
	}
}

// TestSaveCreatesAMissingRow: a platform the seed never created must be
// configurable, not a 404.
func TestSaveCreatesAMissingRow(t *testing.T) {
	repo := newFakeRepo()
	u := NewUseCase(repo, nil)
	out, err := u.Save(context.Background(), superadmin(), domain.PlatformAndroid, SaveInput{
		StoreURL:              str("https://play.google.com/store/apps/details?id=kz.bookeat.app"),
		MinRecommendedVersion: str("1.9"),
		RecommendedTitle:      str("Доступно обновление"),
		RecommendedMessage:    str("Обновите приложение"),
	})
	if err != nil {
		t.Fatalf("Save on a missing row: %v", err)
	}
	if out.Platform != domain.PlatformAndroid {
		t.Errorf("platform = %q", out.Platform)
	}
	if repo.upserts != 1 {
		t.Errorf("upserts = %d, want 1", repo.upserts)
	}
}

// --- Save: validation -------------------------------------------------------

// TestSaveRefusesAnUnparsableThreshold. At read time such a value silently
// degrades to "no threshold", so the operator who flipped the switch would
// never learn it does nothing. Refuse it at the door instead.
func TestSaveRefusesAnUnparsableThreshold(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo, nil)
	for _, bad := range []string{"полтора", "1.5.x", "latest", "1..2", strings.Repeat("9", 40)} {
		_, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
			SaveInput{MinSupportedVersion: str(bad)})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Save(min_supported=%q) = %v, want ErrValidation", bad, err)
		}
	}
	if repo.upserts != 0 {
		t.Errorf("a refused save still wrote %d rows", repo.upserts)
	}
}

// TestSaveRefusesAForcedFloorAboveTheRecommendedOne: the recommended tier would
// be unreachable, which is a configuration mistake, not a policy.
func TestSaveRefusesAForcedFloorAboveTheRecommendedOne(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	_, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{MinSupportedVersion: str("2.0"), MinRecommendedVersion: str("1.9")})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Save = %v, want ErrValidation", err)
	}
	// Equal floors are fine: "require 2.0, and 2.0 is also what we recommend".
	if _, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{MinSupportedVersion: str("2.0"), MinRecommendedVersion: str("2.0")}); err != nil {
		t.Fatalf("equal floors must be allowed, got %v", err)
	}
}

// TestSaveRefusesAModeWithNoWordingOrNoStoreLink: a blank modal with a dead
// button is worse than no prompt.
func TestSaveRefusesAModeWithNoWordingOrNoStoreLink(t *testing.T) {
	repo := newFakeRepo()
	u := NewUseCase(repo, nil)

	_, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{MinSupportedVersion: str("1.5")})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("forcing with no wording = %v, want ErrValidation", err)
	}

	_, err = u.Save(context.Background(), superadmin(), domain.PlatformIOS, SaveInput{
		MinSupportedVersion: str("1.5"),
		RequiredTitle:       str("Нужно обновить"),
		RequiredMessage:     str("Обновите приложение"),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("forcing with no store URL = %v, want ErrValidation", err)
	}
}

// TestSaveRefusesABadStoreURL. The value ends up in a tap handler on a phone;
// a relative path or a javascript: scheme has no business there.
func TestSaveRefusesABadStoreURL(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	for _, bad := range []string{"apps.apple.com/app/id1", "/app", "javascript:alert(1)", "ftp://x/y"} {
		_, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS, SaveInput{StoreURL: str(bad)})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Save(store_url=%q) = %v, want ErrValidation", bad, err)
		}
	}
}

// TestSaveRefusesAnUnsupportedTranslationLanguage: a translation nothing can
// read back is worse than a missing one.
func TestSaveRefusesAnUnsupportedTranslationLanguage(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	_, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{RequiredTitleI18n: domain.I18nPatch{"zh": str("请更新")}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Save = %v, want ErrValidation", err)
	}
}

// TestSaveRefusesOverlongText: caught here with a field name instead of by
// Postgres as a 500 (value too long for type character varying).
func TestSaveRefusesOverlongText(t *testing.T) {
	u := NewUseCase(seeded(), nil)
	_, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{RequiredTitle: str(strings.Repeat("я", maxTitleLen+1))})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Save = %v, want ErrValidation", err)
	}
	// A title made of multi-byte runes that FITS must still be accepted:
	// varchar(N) counts characters, not bytes.
	if _, err := u.Save(context.Background(), superadmin(), domain.PlatformIOS,
		SaveInput{RequiredTitle: str(strings.Repeat("я", maxTitleLen))}); err != nil {
		t.Fatalf("a %d-character title was refused: %v", maxTitleLen, err)
	}
}

// TestSaveRefusesAnUnknownPlatform. The handler parses the path parameter, but
// the usecase is callable from elsewhere and must not create a row for a
// platform the CHECK constraint would reject anyway.
func TestSaveRefusesAnUnknownPlatform(t *testing.T) {
	repo := newFakeRepo()
	u := NewUseCase(repo, nil)
	for _, p := range []domain.DevicePlatform{"web", "windows", ""} {
		if _, err := u.Save(context.Background(), superadmin(), p, SaveInput{}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Save(platform=%q) = %v, want ErrValidation", p, err)
		}
	}
	if repo.upserts != 0 {
		t.Errorf("a refused save still wrote %d rows", repo.upserts)
	}
}
