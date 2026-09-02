package cuisine

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
)

// aliasesOf reads the spellings the dictionary currently maps to one cuisine.
func aliasesOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT alias FROM cuisine_aliases WHERE cuisine_id = $1 ORDER BY alias`, id)
	if err != nil {
		t.Fatalf("read aliases: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan alias: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read aliases: %v", err)
	}
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCreateSeedsTheSameAliasesTheMigrationsDo pins the invariant the admin
// route broke: a cuisine is born with its name and its code in cuisine_aliases,
// the same pair migrations 0079/0080 wrote for the fifteen existing entries.
// Without it the catalog filter by the cuisine's own code matches nothing.
func TestCreateSeedsTheSameAliasesTheMigrationsDo(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	c := newCuisine("indian", "Индийская")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := sorted([]string{"индийская", "indian"})
	if got := sorted(aliasesOf(t, pool, c.ID)); !equalStrings(got, want) {
		t.Fatalf("aliases after create = %v, want %v", got, want)
	}
}

// TestAliasNormalizationMatchesTheSearchKey is the parity check between the two
// halves of the filter. The catalog compares
// domain.NormalizeCuisineKeys(?cuisine=...) against the stored alias, byte for
// byte — so a stored alias must be a FIXED POINT of that very function. Any
// other normalization here (SQL lower(btrim(...)) leaves inner double spaces,
// for one) produces a row that looks right in psql and never matches.
func TestAliasNormalizationMatchesTheSearchKey(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	// A name as sloppy as a human types it: padding, doubled inner space,
	// mixed case.
	const rawName = "  Ближневосточная   КУХНЯ  "
	c := newCuisine("middle_eastern", rawName)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := aliasesOf(t, pool, c.ID)
	if len(got) != 2 {
		t.Fatalf("aliases = %v, want two (name + code)", got)
	}
	for _, a := range got {
		if k := domain.NormalizeCuisineKey(a); k != a {
			t.Errorf("stored alias %q normalizes to %q; a search key can never equal it", a, k)
		}
	}
	want := sorted([]string{
		domain.NormalizeCuisineKey(rawName),
		domain.NormalizeCuisineKey("middle_eastern"),
	})
	if !equalStrings(sorted(got), want) {
		t.Fatalf("aliases = %v, want %v", sorted(got), want)
	}
}

// TestUpdateBackfillsMissingAliasesAndRepeatsCleanly covers two things that
// have to hold together:
//
//   - an entry created BEFORE this code exists with zero aliases (production
//     «Индийская», 2026-09-01). Editing it must repair it, so the fix reaches
//     old rows through the panel and not only through new ones;
//   - saving the same entry twice must not trip the alias primary key.
func TestUpdateBackfillsMissingAliasesAndRepeatsCleanly(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	// Insert the way the broken Create did: dictionary row only.
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO cuisines (id, code, name) VALUES ($1,'indian','Индийская')`, id); err != nil {
		t.Fatalf("seed aliasless cuisine: %v", err)
	}
	if got := aliasesOf(t, pool, id); len(got) != 0 {
		t.Fatalf("precondition: aliases = %v, want none", got)
	}

	c, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := repo.Update(ctx, c); err != nil {
			t.Fatalf("update #%d: %v", i+1, err)
		}
	}
	want := sorted([]string{"индийская", "indian"})
	if got := sorted(aliasesOf(t, pool, id)); !equalStrings(got, want) {
		t.Fatalf("aliases after two updates = %v, want exactly %v", got, want)
	}
}

// TestRenameAddsTheNewSpellingAndKeepsTheOldOne states the rename decision.
//
// cuisine_aliases is the set of spellings that MEAN this cuisine, not a
// rendering of its current name. So a rename ADDS and never removes: the old
// name still sits in the `cuisine_type` string of every venue saved before the
// rename and in the chips an already-installed app scraped out of the catalog,
// and both must keep resolving to the same dictionary entry. The same rule is
// what protects the hand-curated synonyms migration 0080 seeded.
func TestRenameAddsTheNewSpellingAndKeepsTheOldOne(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	c := newCuisine("indian", "Индийская")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.Name = "Индийская кухня"
	c.Code = "indian_cuisine"
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("rename: %v", err)
	}

	want := sorted([]string{"индийская", "indian", "индийская кухня", "indian_cuisine"})
	if got := sorted(aliasesOf(t, pool, c.ID)); !equalStrings(got, want) {
		t.Fatalf("aliases after rename = %v, want %v", got, want)
	}
}

// TestSpellingOwnedByAnotherCuisineIsRefused: two dictionary entries answering
// to one search key is an ambiguity the filter cannot resolve, so the write is
// refused rather than silently skipped (which would leave the new entry
// unfindable by its own name) or silently re-pointed (which would steal the key
// from the cuisine that already answers to it).
//
// The refusal must also leave NOTHING behind — the dictionary row and its
// aliases go in one statement, so the failed alias takes the cuisine with it.
func TestSpellingOwnedByAnotherCuisineIsRefused(t *testing.T) {
	repo, pool := freshRepo(t)
	ctx := context.Background()

	japanese := newCuisine("japanese", "Японская")
	if err := repo.Create(ctx, japanese); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The hand-curated synonym migration 0080 seeds.
	if _, err := pool.Exec(ctx,
		`INSERT INTO cuisine_aliases (alias, cuisine_id) VALUES ('японская (идзакая)', $1)`,
		japanese.ID); err != nil {
		t.Fatalf("seed manual alias: %v", err)
	}

	clash := newCuisine("izakaya", "Японская (идзакая)")
	if err := repo.Create(ctx, clash); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create with a taken spelling = %v, want ErrAlreadyExists", err)
	}
	if _, err := repo.GetByID(ctx, clash.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the refused cuisine survived the failed alias write (%v); "+
			"the dictionary row and its aliases must land or fail together", err)
	}
	// The existing mapping is untouched.
	if got := sorted(aliasesOf(t, pool, japanese.ID)); !equalStrings(got,
		sorted([]string{"японская", "japanese", "японская (идзакая)"})) {
		t.Errorf("aliases of the existing cuisine = %v, want them intact", got)
	}
}

// TestUpdateOfAMissingCuisineStillReportsNotFound guards the shape of Update
// after it grew a second statement: its command tag now counts ALIAS rows, so
// "does this id exist" has to come from the UPDATE's own RETURNING. Get this
// wrong and editing a deleted cuisine answers 200.
func TestUpdateOfAMissingCuisineStillReportsNotFound(t *testing.T) {
	repo, _ := freshRepo(t)
	ghost := newCuisine("ghost", "Призрачная")
	if err := repo.Update(context.Background(), ghost); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update of an unknown id = %v, want ErrNotFound", err)
	}
}
