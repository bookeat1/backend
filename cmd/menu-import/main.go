// Command menu-import's entry point: wires flags, config, DB and the plan
// built in plan.go into an upsert run (or a dry-run report). See doc.go for
// the full contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/bootstrap"
	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/menu"
	"backend-core/internal/logger"
)

func main() {
	filePath := flag.String("file", "", "path to a parsed menu JSON file (required)")
	restaurantIDStr := flag.String("restaurant-id", "", "restaurant UUID (mutually exclusive with -restaurant-name)")
	restaurantName := flag.String("restaurant-name", "", "exact restaurant name, matched case-insensitively (mutually exclusive with -restaurant-id)")
	dryRun := flag.Bool("dry-run", false, "print counts only, write nothing")
	flag.Parse()

	log := logger.New("info", "text")

	if err := run(context.Background(), log, *filePath, *restaurantIDStr, *restaurantName, *dryRun); err != nil {
		log.Error("menu-import failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, filePath, restaurantIDStr, restaurantName string, dryRun bool) error {
	if filePath == "" {
		return errors.New("-file is required")
	}
	if (restaurantIDStr == "") == (restaurantName == "") {
		return errors.New("exactly one of -restaurant-id / -restaurant-name is required")
	}

	cfg, err := bootstrap.NewConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pool, err := bootstrap.NewDB(cfg.DB.Postgres)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	restaurantID, name, err := resolveRestaurant(ctx, pool, restaurantIDStr, restaurantName)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", filePath, err)
	}
	parsed, err := ParseMenuFile(data)
	if err != nil {
		return fmt.Errorf("%s: %w", filePath, err)
	}

	items := menu.New(pool)
	categories := menu.NewCategories(pool)

	existing, err := items.ListByRestaurant(ctx, domain.MenuItemFilter{RestaurantID: restaurantID})
	if err != nil {
		return fmt.Errorf("load existing menu for %s: %w", name, err)
	}

	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		return fmt.Errorf("build plan for %s: %w", name, err)
	}
	for i := range plan.ToInsert {
		plan.ToInsert[i].RestaurantID = restaurantID
	}

	existingCats, err := categories.List(ctx)
	if err != nil {
		return fmt.Errorf("load menu categories: %w", err)
	}
	missingCats := missingCategories(existingCats, plan.Sections)

	log.Info("plan built",
		slog.String("restaurant", name),
		slog.String("restaurant_id", restaurantID.String()),
		slog.String("file", filePath),
		slog.Int("file_items", len(parsed)),
		slog.Int("to_insert", len(plan.ToInsert)),
		slog.Int("to_update", len(plan.ToUpdate)),
		slog.Int("unchanged", plan.Unchanged),
		slog.Int("sections_seen", len(plan.Sections)),
		slog.Int("sections_to_create", len(missingCats)),
	)
	fmt.Printf("restaurant: %s (%s)\nfile: %s (%d items)\nto insert: %d\nto update: %d\nunchanged: %d\nsections seen: %d\nsections to create: %s\n",
		name, restaurantID, filePath, len(parsed),
		len(plan.ToInsert), len(plan.ToUpdate), plan.Unchanged,
		len(plan.Sections), strings.Join(missingCats, ", "))

	if dryRun {
		fmt.Println("dry-run: no changes written")
		return nil
	}

	for _, catName := range missingCats {
		c := domain.MenuCategory{Name: catName}
		if err := categories.Create(ctx, &c); err != nil {
			return fmt.Errorf("create category %q: %w", catName, err)
		}
	}
	for i := range plan.ToInsert {
		m := plan.ToInsert[i]
		if err := items.Create(ctx, &m); err != nil {
			return fmt.Errorf("insert item %q: %w", m.Name, err)
		}
	}
	for i := range plan.ToUpdate {
		m := plan.ToUpdate[i]
		if err := items.Update(ctx, &m); err != nil {
			return fmt.Errorf("update item %q (%s): %w", m.Name, m.ID, err)
		}
	}

	log.Info("menu-import applied",
		slog.String("restaurant", name),
		slog.Int("inserted", len(plan.ToInsert)),
		slog.Int("updated", len(plan.ToUpdate)),
		slog.Int("categories_created", len(missingCats)),
	)
	fmt.Println("done: written to the database")
	return nil
}

// missingCategories returns the sections not already present (trimmed,
// case-insensitive) among existingCats, in the order they first appear in
// sections.
func missingCategories(existingCats []domain.MenuCategory, sections []string) []string {
	have := make(map[string]bool, len(existingCats))
	for _, c := range existingCats {
		have[NormalizeName(c.Name)] = true
	}
	var missing []string
	for _, s := range sections {
		if !have[NormalizeName(s)] {
			missing = append(missing, s)
			have[NormalizeName(s)] = true // dedupe within sections itself
		}
	}
	return missing
}

// dbQuerier is the minimal subset of *pgxpool.Pool this command needs to
// resolve a restaurant by id/name — declared locally so this file depends on
// a shape, not the concrete pgx type.
type dbQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// resolveRestaurant returns the id to import into. -restaurant-id is parsed
// and trusted as-is (id existence is verified by the ListByRestaurant/insert
// calls failing loudly); -restaurant-name is matched case-insensitively,
// trimmed, and must resolve to EXACTLY one active-or-not restaurant — an
// ambiguous or absent name is a hard error, never a guess.
func resolveRestaurant(ctx context.Context, pool dbQuerier, idStr, name string) (uuid.UUID, string, error) {
	if idStr != "" {
		id, err := uuid.Parse(idStr)
		if err != nil {
			return uuid.UUID{}, "", fmt.Errorf("invalid -restaurant-id %q: %w", idStr, err)
		}
		var got string
		err = pool.QueryRow(ctx, `SELECT name FROM restaurants WHERE id = $1`, id).Scan(&got)
		if err != nil {
			return uuid.UUID{}, "", fmt.Errorf("restaurant %s: %w", id, err)
		}
		return id, got, nil
	}

	trimmed := strings.TrimSpace(name)
	rows, err := pool.Query(ctx, `SELECT id, name FROM restaurants WHERE trim(lower(name)) = trim(lower($1))`, trimmed)
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("look up restaurant %q: %w", trimmed, err)
	}
	defer rows.Close()

	var (
		id      uuid.UUID
		got     string
		matches int
	)
	for rows.Next() {
		if err := rows.Scan(&id, &got); err != nil {
			return uuid.UUID{}, "", fmt.Errorf("scan restaurant row: %w", err)
		}
		matches++
	}
	if err := rows.Err(); err != nil {
		return uuid.UUID{}, "", fmt.Errorf("look up restaurant %q: %w", trimmed, err)
	}
	if matches == 0 {
		return uuid.UUID{}, "", fmt.Errorf("no restaurant named %q", trimmed)
	}
	if matches > 1 {
		return uuid.UUID{}, "", fmt.Errorf("restaurant name %q is ambiguous (%d matches), use -restaurant-id", trimmed, matches)
	}
	return id, got, nil
}
