package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

// runMenu upserts the menu catalog from raw_supabase into the clean schema.
// Idempotent (upsert by id); FK order: categories → items (JOIN restaurants) →
// tags (JOIN items). Duplicate (menu_item_id, tag) pairs are skipped.
func runMenu(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	steps := []struct {
		name string
		sql  string
	}{
		{"menu_categories", `
			INSERT INTO menu_categories (id, name, name_i18n, parent_id, display_order, created_at)
			SELECT id, name, name_i18n, parent_id, COALESCE(display_order, 0), COALESCE(created_at, now())
			FROM raw_supabase.menu_categories
			ON CONFLICT (id) DO UPDATE SET
			  name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n,
			  parent_id=EXCLUDED.parent_id, display_order=EXCLUDED.display_order`},
		{"menu_items", `
			INSERT INTO menu_items (id, restaurant_id, name, name_i18n, description, description_i18n,
			  price, image_url, is_available, category, category_i18n, subcategory, subcategory_i18n,
			  portion_size, portion_size_i18n, language, display_order, created_at, updated_at)
			SELECT mi.id, mi.restaurant_id, mi.name, mi.name_i18n, COALESCE(mi.description,''), mi.description_i18n,
			  GREATEST(COALESCE(mi.price, 0), 0), mi.image_url, COALESCE(mi.is_available, true), mi.category, mi.category_i18n,
			  mi.subcategory, mi.subcategory_i18n, mi.portion_size, mi.portion_size_i18n, mi.language,
			  mi.display_order, COALESCE(mi.created_at, now()), COALESCE(mi.updated_at, now())
			FROM raw_supabase.menu_items mi
			JOIN restaurants r ON r.id = mi.restaurant_id
			ON CONFLICT (id) DO UPDATE SET
			  name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n, description=EXCLUDED.description,
			  description_i18n=EXCLUDED.description_i18n, price=EXCLUDED.price, image_url=EXCLUDED.image_url,
			  is_available=EXCLUDED.is_available, category=EXCLUDED.category, category_i18n=EXCLUDED.category_i18n,
			  subcategory=EXCLUDED.subcategory, subcategory_i18n=EXCLUDED.subcategory_i18n,
			  portion_size=EXCLUDED.portion_size, portion_size_i18n=EXCLUDED.portion_size_i18n,
			  language=EXCLUDED.language, display_order=EXCLUDED.display_order, updated_at=EXCLUDED.updated_at`},
		{"menu_item_tags", `
			INSERT INTO menu_item_tags (id, menu_item_id, tag, created_at)
			SELECT t.id, t.menu_item_id, t.tag, COALESCE(t.created_at, now())
			FROM raw_supabase.menu_item_tags t
			JOIN menu_items mi ON mi.id = t.menu_item_id
			ON CONFLICT (menu_item_id, tag) DO NOTHING`},
	}

	var totalItems int
	for _, s := range steps {
		res, err := db.ExecContext(ctx, s.sql)
		if err != nil {
			return errors.New("etl step " + s.name + ": " + err.Error())
		}
		n, _ := res.RowsAffected()
		if s.name == "menu_items" {
			totalItems = int(n)
		}
		log.Info("etl step done", slog.String("step", s.name), slog.Int64("rows", n))
	}
	if totalItems == 0 {
		return errors.New("no menu_items found in raw_supabase — is the dump loaded?")
	}
	log.Info("menu etl complete", slog.Int("menu_items", totalItems))
	return nil
}
