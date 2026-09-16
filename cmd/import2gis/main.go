// Command import2gis is a one-off, idempotent enrichment of the Алматы
// restaurants already in `restaurants` from a 2gis export
// (bookeat-selected-venues.json, produced out-of-band — see the JSON's own
// "source_url" per venue). It NEVER creates a restaurant: a venue with no
// confident name match is only reported, never inserted (see runbook /
// ai-tasks discussion for why — the export can duplicate or mis-scrape a
// branch, and a wrong INSERT is much harder to undo than a missed UPDATE).
//
// Merge policy (never destructive):
//   - address / phone / email: written ONLY when the current column is empty.
//     A non-empty column that disagrees (after light normalisation) is a
//     CONFLICT — reported, never overwritten. Damir resolves those by hand.
//   - website / instagram: same rule, but the "column" is the matching row of
//     restaurant_social_links (created if absent).
//   - opening_hours (free text) + restaurant_working_hours (the structured
//     weekly schedule used for bookability): only touched when the venue has
//     NO working_hours rows yet AND the 2gis hours string parses into a full,
//     unambiguous week (domain.ParseWorkingHoursText — "Ежедневно ..." and
//     day-range forms). 2gis's "Сегодня c HH:MM до HH:MM" ("today") lines are
//     NEVER written anywhere live: they say what the venue does TODAY, not
//     every day, and settling a permanent display string from that could tell
//     a guest the wrong closing time tomorrow. Those are reported as
//     unresolved so a human decides.
//   - venue features: 2gis's per-venue `feature_chips[].key` is matched
//     against a small, explicit table of the codes that mean the same thing
//     in our `venue_features` dictionary (chipCodeToFeatureCode below).
//     Matches are ADDED to restaurant_venue_features (never removed — a
//     feature the venue staff picked itself outranks a 2gis guess). A chip
//     with no dictionary equivalent is reported, never used to invent a new
//     dictionary row (venues pick from the dictionary, they don't grow it —
//     see migration 0082).
//   - average_check: written to price_min/price_max ONLY when both are
//     currently NULL (a point estimate: min=max=average_check). An existing
//     range that does not already contain the 2gis figure is a conflict.
//
// photo_urls and rating/review_count are read from the JSON but never
// written: they are 2gis's own CDN links and 2gis's own aggregates, not ours
// (see the task brief this command was written against).
//
// Idempotent: a second run changes nothing, since by then every field this
// tool is allowed to touch is already non-empty/non-null and matches, and
// every insertable link/feature/hours row already exists.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/bootstrap"
	"backend-core/internal/domain"
)

func main() {
	dataPath := flag.String("data", "", "path to the 2gis venues JSON export (read-only)")
	apply := flag.Bool("apply", false, "actually write changes; default is a dry run that only prints the report")
	flag.Parse()
	if *dataPath == "" {
		fmt.Fprintln(os.Stderr, "usage: import2gis --data <path-to-json> [--apply]")
		os.Exit(2)
	}

	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	db, err := bootstrap.NewSQLDB(cfg.DB.Postgres)
	if err != nil {
		slog.Error("connect db", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	export, err := loadExport(*dataPath)
	if err != nil {
		slog.Error("load 2gis export", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ctx := context.Background()
	candidates, err := loadCandidates(ctx, db, "Алматы")
	if err != nil {
		slog.Error("load candidate restaurants", slog.String("error", err.Error()))
		os.Exit(1)
	}
	featureCodes, err := loadFeatureCodes(ctx, db)
	if err != nil {
		slog.Error("load venue_features", slog.String("error", err.Error()))
		os.Exit(1)
	}

	rep := &report{}
	for _, v := range export.Venues {
		match, ambiguous := matchVenue(v, candidates)
		if match == nil {
			rep.addNotFound(v, ambiguous)
			continue
		}
		plan, err := buildPlan(ctx, db, *match, v, featureCodes)
		if err != nil {
			slog.Error("build plan", slog.String("venue", v.Name), slog.String("error", err.Error()))
			os.Exit(1)
		}
		rep.addMatch(v, *match, plan)
		if *apply && plan.hasWrites() {
			if err := applyPlan(ctx, db, match.ID, plan); err != nil {
				slog.Error("apply plan", slog.String("restaurant_id", match.ID.String()), slog.String("error", err.Error()))
				os.Exit(1)
			}
		}
	}

	rep.print(*apply)
}

// ---- JSON export shape -----------------------------------------------------

type export struct {
	City   string   `json:"city"`
	Venues []venue2 `json:"venues"`
}

type featureChip struct {
	Group string `json:"group"`
	Key   string `json:"key"`
	Label string `json:"label"`
}

type venue2 struct {
	Name         string        `json:"name"`
	MatchedName  string        `json:"matched_name"`
	Address      string        `json:"address"`
	Phone        string        `json:"phone"`
	Email        string        `json:"email"`
	Website      string        `json:"website"`
	Instagram    string        `json:"instagram"`
	Hours        []string      `json:"hours"`
	FeatureChips []featureChip `json:"feature_chips"`
	AverageCheck int           `json:"average_check"`
	SourceURL    string        `json:"source_url"`
}

func loadExport(path string) (*export, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e export
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ---- DB-side candidate restaurants -----------------------------------------

type restaurantRow struct {
	ID           uuid.UUID
	Name         string
	Address      string
	Phone        string
	Email        string
	OpeningHours string
	PriceMin     *int
	PriceMax     *int
	HasHours     bool // true if restaurant_working_hours has any row already
}

func loadCandidates(ctx context.Context, db *sql.DB, city string) ([]restaurantRow, error) {
	const q = `
		SELECT r.id, r.name, r.address, r.phone, r.email, r.opening_hours, r.price_min, r.price_max,
		       EXISTS (SELECT 1 FROM restaurant_working_hours w WHERE w.restaurant_id = r.id) AS has_hours
		FROM restaurants r
		WHERE r.city = $1`
	rows, err := db.QueryContext(ctx, q, city)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []restaurantRow
	for rows.Next() {
		var r restaurantRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Address, &r.Phone, &r.Email, &r.OpeningHours, &r.PriceMin, &r.PriceMax, &r.HasHours); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadFeatureCodes(ctx context.Context, db *sql.DB) (map[string]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, code FROM venue_features`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			return nil, err
		}
		out[code] = id
	}
	return out, rows.Err()
}

// chipCodeToFeatureCode maps a 2gis feature_chips[].key to the venue_features
// dictionary code that means the same thing (checked by hand against the
// TEST dictionary, migration 0082 — see the package doc). A chip key absent
// from this table has no confident equivalent and is left unmapped on
// purpose: the dictionary is platform-owned, this importer does not grow it.
var chipCodeToFeatureCode = map[string]string{
	"wifi":           "wifi",
	"terrace":        "terrace",
	"breakfast":      "breakfast",
	"wine_list":      "wine_list",
	"pet_friendly":   "pets",
	"panoramic_view": "view",
	"takeaway":       "takeaway",
}

// ---- fuzzy name matching ----------------------------------------------------

var normNonWord = regexp.MustCompile(`[^a-zа-я0-9 ]`)
var normSpaces = regexp.MustCompile(`\s+`)

// hyphenFold folds the dash/hyphen look-alikes 2gis labels use (e.g. U+2011
// NON-BREAKING HYPHEN in "Wi‑Fi") to a plain '-' before any other
// normalisation, same spirit as domain.punctReplacer for working-hours text.
var hyphenFold = strings.NewReplacer("‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-")

func normalizeName(s string) string {
	s = hyphenFold.Replace(strings.ToLower(s))
	s = normNonWord.ReplaceAllString(s, " ")
	return strings.TrimSpace(normSpaces.ReplaceAllString(s, " "))
}

func noSpace(s string) string { return strings.ReplaceAll(s, " ", "") }

var addrStopwords = map[string]bool{
	"улица": true, "ул": true, "мкр": true, "микрорайон": true,
	"проспект": true, "пр": true, "этаж": true, "дом": true, "д": true,
	"алматы": true, "трц": true, "филиал": true, "филиала": true, "филиалов": true,
}

func addressTokens(s string) map[string]bool {
	n := normalizeName(s)
	out := map[string]bool{}
	for _, tok := range strings.Fields(n) {
		if len(tok) < 2 || addrStopwords[tok] {
			continue
		}
		out[tok] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Fields(s) {
		out[t] = true
	}
	return out
}

// nameScore scores how well two venue names match, 0..1. Exact/contained
// normalised forms score highest; anything else falls back to word-set
// Jaccard, which is enough for reordered words in the SAME script (e.g. "Kok
// Tobe Terrace" vs "Koktobe Terrace" is caught by the no-space check below,
// not this).
func nameScore(a, b string) float64 {
	na, nb := normalizeName(a), normalizeName(b)
	if na == nb {
		return 1.0
	}
	if noSpace(na) == noSpace(nb) {
		return 0.95
	}
	if strings.Contains(na, nb) || strings.Contains(nb, na) {
		return 0.9
	}
	return jaccard(tokenSet(na), tokenSet(nb))
}

// matchResult is a scored candidate.
type matchResult struct {
	row   restaurantRow
	score float64
}

// matchVenue finds the best restaurantRow for a 2gis venue. It returns
// (nil, false) when nothing scores high enough, and (nil, true) when two or
// more candidates are too close to call — both are "not found" for the
// caller, the bool only changes the report wording.
//
// Address similarity is folded in ON TOP of the name score (not instead of
// it): a few venues in this export share a generic chain name across several
// TEST branches (three "Ocean Basket" entries) or are written in different
// scripts between 2gis and our own data ("Караоке 1100" vs "1100 Karaoke",
// which share only their street address and the digits "1100" — no string
// similarity between "караоке" and "karaoke" exists in any script-agnostic
// metric). Combining both signals is what makes those resolvable without
// hand-coding restaurant ids into this file.
func matchVenue(v venue2, candidates []restaurantRow) (*restaurantRow, bool) {
	vAddr := addressTokens(v.Address)
	var scored []matchResult
	for _, c := range candidates {
		ns := nameScore(v.Name, c.Name)
		as := jaccard(vAddr, addressTokens(c.Address))
		combined := ns
		if as > combined {
			// Address-led fallback: lets a cross-script or chain-generic name
			// still resolve when the address makes the target unambiguous.
			combined = 0.5*ns + 0.5*as
		}
		if combined >= 0.45 {
			scored = append(scored, matchResult{c, combined})
		}
	}
	if len(scored) == 0 {
		return nil, false
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	if len(scored) > 1 && scored[0].score-scored[1].score < 0.1 {
		return nil, true // too close to call, e.g. duplicate-named TEST rows
	}
	return &scored[0].row, false
}

// ---- plan: what would change ------------------------------------------------

type fieldChange struct {
	field string
	from  string
	to    string
}

type plan struct {
	updates            []fieldChange // written verbatim to `restaurants`
	conflicts          []fieldChange // reported only, never written
	newSocialLinks     []domain.SocialLink
	newFeatureCodes    []string
	unmappedFeatures   []string
	hoursNote          string // set when hours exist in 2gis but were not written
	priceMin, priceMax *int
}

func (p *plan) hasWrites() bool {
	return len(p.updates) > 0 || len(p.newSocialLinks) > 0 || len(p.newFeatureCodes) > 0 || (p.priceMin != nil && p.priceMax != nil)
}

func normPhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Kazakhstan numbers are sometimes stored with a leading 8 instead of 7 —
	// fold both to the same 10 significant digits so "+7 700..." and
	// "8 700..." compare equal instead of raising a false conflict.
	if len(out) == 11 && (out[0] == '7' || out[0] == '8') {
		out = out[1:]
	}
	return out
}

func buildPlan(ctx context.Context, db *sql.DB, row restaurantRow, v venue2, featureCodes map[string]uuid.UUID) (*plan, error) {
	p := &plan{}

	mergeText := func(field, current, incoming string) {
		incoming = strings.TrimSpace(incoming)
		if incoming == "" {
			return
		}
		if strings.TrimSpace(current) == "" {
			p.updates = append(p.updates, fieldChange{field, current, incoming})
			return
		}
		if strings.TrimSpace(current) != incoming {
			p.conflicts = append(p.conflicts, fieldChange{field, current, incoming})
		}
	}

	// address / email: plain text compare.
	mergeText("address", row.Address, v.Address)
	mergeText("email", row.Email, v.Email)

	// phone: compare on digits only so formatting differences don't cause a
	// false conflict, but store the 2gis value's own formatting when writing.
	if incoming := strings.TrimSpace(v.Phone); incoming != "" {
		if strings.TrimSpace(row.Phone) == "" {
			p.updates = append(p.updates, fieldChange{"phone", row.Phone, incoming})
		} else if normPhone(row.Phone) != normPhone(incoming) {
			p.conflicts = append(p.conflicts, fieldChange{"phone", row.Phone, incoming})
		}
	}

	// social links (website / instagram)
	existing, err := listSocialLinks(ctx, db, row.ID)
	if err != nil {
		return nil, err
	}
	mergeLink := func(typ, incoming string) {
		incoming = strings.TrimSpace(incoming)
		if incoming == "" {
			return
		}
		if cur, ok := existing[typ]; ok {
			if cur != incoming {
				p.conflicts = append(p.conflicts, fieldChange{typ, cur, incoming})
			}
			return
		}
		p.newSocialLinks = append(p.newSocialLinks, domain.SocialLink{RestaurantID: row.ID, Type: typ, URL: incoming})
	}
	mergeLink("website", v.Website)
	mergeLink("instagram", v.Instagram)

	// hours: only "Ежедневно ..." / day-range forms are ever written, and
	// only into a restaurant with NO existing schedule at all.
	if len(v.Hours) > 0 && strings.TrimSpace(v.Hours[0]) != "" {
		raw := v.Hours[0]
		if _, err := domain.ParseWorkingHoursText(raw); err == nil {
			if !row.HasHours {
				p.updates = append(p.updates, fieldChange{"working_hours (structured)", "(none)", raw})
				mergeText("opening_hours", row.OpeningHours, raw)
			} else {
				p.hoursNote = fmt.Sprintf("2gis: %q parses cleanly but restaurant already has a working_hours schedule — left alone", raw)
			}
		} else {
			p.hoursNote = fmt.Sprintf("2gis hours %q not written: 2gis 'today' format only tells us today's hours, not the whole week — needs a human to confirm the real weekly schedule", raw)
		}
	}

	// features
	for _, chip := range v.FeatureChips {
		code, ok := chipCodeToFeatureCode[chip.Key]
		if !ok {
			p.unmappedFeatures = append(p.unmappedFeatures, chip.Label)
			continue
		}
		if _, ok := featureCodes[code]; !ok {
			continue // dictionary changed since chipCodeToFeatureCode was written
		}
		var has bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM restaurant_venue_features WHERE restaurant_id=$1 AND feature_id=$2)`,
			row.ID, featureCodes[code]).Scan(&has); err != nil {
			return nil, err
		}
		if !has {
			p.newFeatureCodes = append(p.newFeatureCodes, code)
		}
	}

	// average check
	if v.AverageCheck > 0 {
		if row.PriceMin == nil && row.PriceMax == nil {
			val := v.AverageCheck
			p.priceMin, p.priceMax = &val, &val
		} else if row.PriceMin != nil && row.PriceMax != nil && (v.AverageCheck < *row.PriceMin || v.AverageCheck > *row.PriceMax) {
			p.conflicts = append(p.conflicts, fieldChange{
				"average_check",
				fmt.Sprintf("%d-%d", *row.PriceMin, *row.PriceMax),
				fmt.Sprintf("%d", v.AverageCheck),
			})
		}
	}

	return p, nil
}

func listSocialLinks(ctx context.Context, db *sql.DB, restaurantID uuid.UUID) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, url FROM restaurant_social_links WHERE restaurant_id=$1`, restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var typ, url string
		if err := rows.Scan(&typ, &url); err != nil {
			return nil, err
		}
		out[typ] = url
	}
	return out, rows.Err()
}

// ---- apply ------------------------------------------------------------------

func applyPlan(ctx context.Context, db *sql.DB, restaurantID uuid.UUID, p *plan) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, u := range p.updates {
		var col string
		switch u.field {
		case "address":
			col = "address"
		case "email":
			col = "email"
		case "phone":
			col = "phone"
		case "opening_hours":
			col = "opening_hours"
		default:
			continue // "working_hours (structured)" is applied below, not a `restaurants` column
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE restaurants SET %s=$1, updated_at=now() WHERE id=$2`, col), u.to, restaurantID); err != nil {
			return fmt.Errorf("update %s: %w", col, err)
		}
	}

	if p.priceMin != nil && p.priceMax != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE restaurants SET price_min=$1, price_max=$2, updated_at=now() WHERE id=$3`, *p.priceMin, *p.priceMax, restaurantID); err != nil {
			return fmt.Errorf("update price range: %w", err)
		}
	}

	for _, l := range p.newSocialLinks {
		id := uuid.New()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO restaurant_social_links (id, restaurant_id, type, url) VALUES ($1,$2,$3,$4)`,
			id, restaurantID, l.Type, l.URL); err != nil {
			return fmt.Errorf("insert social link %s: %w", l.Type, err)
		}
	}

	for _, u := range p.updates {
		if u.field != "working_hours (structured)" {
			continue
		}
		parsed, err := domain.ParseWorkingHoursText(u.to)
		if err != nil {
			return fmt.Errorf("re-parse hours at apply time: %w", err)
		}
		for _, wh := range parsed.ToWorkingHours(restaurantID) {
			id := uuid.New()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
				 VALUES ($1,$2,$3,$4,$5,$6)`,
				id, restaurantID, wh.DayOfWeek, wh.OpenTime, wh.CloseTime, wh.IsOpen); err != nil {
				return fmt.Errorf("insert working_hours: %w", err)
			}
		}
	}

	featureCodes, err := loadFeatureCodesTx(ctx, tx)
	if err != nil {
		return err
	}
	for _, code := range p.newFeatureCodes {
		fid, ok := featureCodes[code]
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO restaurant_venue_features (restaurant_id, feature_id) VALUES ($1,$2)
			 ON CONFLICT (restaurant_id, feature_id) DO NOTHING`,
			restaurantID, fid); err != nil {
			return fmt.Errorf("insert restaurant_venue_features %s: %w", code, err)
		}
	}

	return tx.Commit()
}

func loadFeatureCodesTx(ctx context.Context, tx *sql.Tx) (map[string]uuid.UUID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, code FROM venue_features`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			return nil, err
		}
		out[code] = id
	}
	return out, rows.Err()
}

// ---- report ------------------------------------------------------------------

type venueReport struct {
	venue     venue2
	matched   *restaurantRow
	ambiguous bool
	plan      *plan
}

type report struct {
	rows []venueReport
}

func (r *report) addNotFound(v venue2, ambiguous bool) {
	r.rows = append(r.rows, venueReport{venue: v, ambiguous: ambiguous})
}

func (r *report) addMatch(v venue2, row restaurantRow, p *plan) {
	r.rows = append(r.rows, venueReport{venue: v, matched: &row, plan: p})
}

func (r *report) print(applied bool) {
	updated, notFound, conflicts := 0, 0, 0
	mode := "DRY RUN (no writes) — pass --apply to write"
	if applied {
		mode = "APPLIED"
	}
	fmt.Printf("=== 2gis import report — %s ===\n\n", mode)
	for _, row := range r.rows {
		if row.matched == nil {
			notFound++
			label := "не найдено"
			if row.ambiguous {
				label = "не найдено (неоднозначно — несколько похожих кандидатов)"
			}
			fmt.Printf("- %s: %s\n", row.venue.Name, label)
			continue
		}
		p := row.plan
		if len(p.updates) > 0 || p.priceMin != nil || len(p.newSocialLinks) > 0 || len(p.newFeatureCodes) > 0 {
			updated++
		}
		fmt.Printf("- %s -> restaurant_id=%s (%s)\n", row.venue.Name, row.matched.ID, row.matched.Name)
		for _, u := range p.updates {
			fmt.Printf("    UPDATE %-28s %q -> %q\n", u.field, u.from, u.to)
		}
		if p.priceMin != nil {
			fmt.Printf("    UPDATE %-28s (none) -> %d-%d\n", "price_min/price_max", *p.priceMin, *p.priceMax)
		}
		for _, l := range p.newSocialLinks {
			fmt.Printf("    UPDATE %-28s (none) -> %q\n", l.Type, l.URL)
		}
		for _, c := range p.newFeatureCodes {
			fmt.Printf("    ADD FEATURE %s\n", c)
		}
		for _, u := range p.unmappedFeatures {
			fmt.Printf("    (info) unmapped 2gis feature, no dictionary code: %q\n", u)
		}
		if p.hoursNote != "" {
			fmt.Printf("    (note) %s\n", p.hoursNote)
		}
		for _, c := range p.conflicts {
			conflicts++
			fmt.Printf("    CONFLICT %-25s ours=%q 2gis=%q — SKIPPED, needs a decision\n", c.field, c.from, c.to)
		}
	}
	fmt.Printf("\n--- summary ---\nvenues in export: %d\nmatched with at least one change: %d\nnot found/ambiguous: %d\nconflicts (fields skipped): %d\n",
		len(r.rows), updated, notFound, conflicts)
}
