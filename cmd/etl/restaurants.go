package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
)

// runRestaurants upserts the restaurant catalog from raw_supabase into the clean
// schema. Idempotent (upsert by id); FK order: categories → restaurants → children.
func runRestaurants(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	steps := []struct {
		name string
		sql  string
	}{
		{"categories", `
			INSERT INTO restaurant_categories (id, name, name_i18n, description, description_i18n, created_at)
			SELECT id, name, name_i18n, description, description_i18n, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_categories
			ON CONFLICT (id) DO UPDATE SET
			  name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n,
			  description=EXCLUDED.description, description_i18n=EXCLUDED.description_i18n`},
		{"restaurants", `
			INSERT INTO restaurants (id, category_id, name, name_i18n, description, description_i18n,
			  cuisine_type, cuisine_type_i18n, address, address_i18n, opening_hours, opening_hours_i18n,
			  city, price_category, email, phone, latitude, longitude, kwaaka_restaurant_id,
			  is_active, is_new, is_popular, is_premium, hidden_from_home, display_order, created_at, updated_at)
			SELECT id, category_id, name, name_i18n, COALESCE(description,''), description_i18n,
			  COALESCE(cuisine_type,''), cuisine_type_i18n, COALESCE(address,''), address_i18n,
			  COALESCE(opening_hours,''), opening_hours_i18n, city::text, price_category::text,
			  COALESCE(email,''), COALESCE(phone,''), latitude, longitude, kwaaka_restaurant_id,
			  COALESCE(is_active,true), is_new, is_popular, is_premium, COALESCE(hidden_from_home,false),
			  display_order, COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurants
			ON CONFLICT (id) DO UPDATE SET
			  category_id=EXCLUDED.category_id, name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n,
			  description=EXCLUDED.description, description_i18n=EXCLUDED.description_i18n,
			  cuisine_type=EXCLUDED.cuisine_type, cuisine_type_i18n=EXCLUDED.cuisine_type_i18n,
			  address=EXCLUDED.address, address_i18n=EXCLUDED.address_i18n,
			  opening_hours=EXCLUDED.opening_hours, opening_hours_i18n=EXCLUDED.opening_hours_i18n,
			  city=EXCLUDED.city, price_category=EXCLUDED.price_category, email=EXCLUDED.email,
			  phone=EXCLUDED.phone, latitude=EXCLUDED.latitude, longitude=EXCLUDED.longitude,
			  kwaaka_restaurant_id=EXCLUDED.kwaaka_restaurant_id, is_active=EXCLUDED.is_active,
			  is_new=EXCLUDED.is_new, is_popular=EXCLUDED.is_popular, is_premium=EXCLUDED.is_premium,
			  hidden_from_home=EXCLUDED.hidden_from_home, display_order=EXCLUDED.display_order,
			  updated_at=EXCLUDED.updated_at`},
		{"features", `
			INSERT INTO restaurant_features (id, restaurant_id, name, name_i18n, created_at)
			SELECT id, restaurant_id, name, name_i18n, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_features
			ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n`},
		{"images", `
			INSERT INTO restaurant_images (id, restaurant_id, image_url, is_primary, created_at)
			SELECT id, restaurant_id, image_url, COALESCE(is_primary,false), COALESCE(created_at, now())
			FROM raw_supabase.restaurant_images
			ON CONFLICT (id) DO UPDATE SET image_url=EXCLUDED.image_url, is_primary=EXCLUDED.is_primary`},
		{"tags", `
			INSERT INTO restaurant_tags (id, restaurant_id, tag_name, tag_name_i18n, created_at)
			SELECT id, restaurant_id, tag_name, tag_name_i18n, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_tags
			ON CONFLICT (id) DO UPDATE SET tag_name=EXCLUDED.tag_name, tag_name_i18n=EXCLUDED.tag_name_i18n`},
		{"social_links", `
			INSERT INTO restaurant_social_links (id, restaurant_id, type, url, created_at)
			SELECT id, restaurant_id, type, url, COALESCE(created_at, now())
			FROM raw_supabase.restaurant_social_links
			ON CONFLICT (id) DO UPDATE SET type=EXCLUDED.type, url=EXCLUDED.url`},
		{"working_hours", `
			INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open, created_at, updated_at)
			SELECT id, restaurant_id, day_of_week, open_time::text, close_time::text, COALESCE(is_open,true),
			  COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_working_hours
			ON CONFLICT (id) DO UPDATE SET day_of_week=EXCLUDED.day_of_week, open_time=EXCLUDED.open_time,
			  close_time=EXCLUDED.close_time, is_open=EXCLUDED.is_open, updated_at=EXCLUDED.updated_at`},
		{"time_slots", `
			INSERT INTO restaurant_time_slots (id, restaurant_id, day_of_week, start_time, end_time, is_manually_disabled, created_at, updated_at)
			SELECT id, restaurant_id, day_of_week, start_time::text, end_time::text, COALESCE(is_manually_disabled,false),
			  COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_time_slots
			ON CONFLICT (id) DO UPDATE SET day_of_week=EXCLUDED.day_of_week, start_time=EXCLUDED.start_time,
			  end_time=EXCLUDED.end_time, is_manually_disabled=EXCLUDED.is_manually_disabled, updated_at=EXCLUDED.updated_at`},
		{"tables", `
			INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, description, is_active, created_at, updated_at)
			SELECT id, restaurant_id, name, COALESCE(capacity,0), description, COALESCE(is_active,true),
			  COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_tables
			ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, capacity=EXCLUDED.capacity,
			  description=EXCLUDED.description, is_active=EXCLUDED.is_active, updated_at=EXCLUDED.updated_at`},
		{"floor_plans", `
			INSERT INTO restaurant_floor_plans (id, restaurant_id, layout_data, created_at, updated_at)
			SELECT id, restaurant_id, layout_data, COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_floor_plans
			ON CONFLICT (id) DO UPDATE SET layout_data=EXCLUDED.layout_data, updated_at=EXCLUDED.updated_at`},
		{"managers", `
			INSERT INTO restaurant_managers (id, restaurant_id, user_id, created_by, whatsapp_opt_in, whatsapp_phone, created_at)
			SELECT m.id, m.restaurant_id, m.user_id, m.created_by, COALESCE(m.whatsapp_opt_in,false), m.whatsapp_phone, COALESCE(m.created_at, now())
			FROM raw_supabase.restaurant_managers m
			JOIN users u ON u.id = m.user_id
			ON CONFLICT (id) DO UPDATE SET whatsapp_opt_in=EXCLUDED.whatsapp_opt_in, whatsapp_phone=EXCLUDED.whatsapp_phone`},
		{"partnership_requests", `
			INSERT INTO restaurant_partnership_requests (id, restaurant_name, contact_name, email, phone, address, cuisine_type, description, additional_info, status, created_at, updated_at)
			SELECT id, restaurant_name, contact_name, email, phone, COALESCE(address,''), cuisine_type, description, additional_info,
			  COALESCE(status,'pending'), COALESCE(created_at, now()), COALESCE(updated_at, now())
			FROM raw_supabase.restaurant_partnership_requests
			ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, updated_at=EXCLUDED.updated_at`},
	}

	var totalRestaurants int
	for _, s := range steps {
		res, err := db.ExecContext(ctx, s.sql)
		if err != nil {
			return errors.New("etl step " + s.name + ": " + err.Error())
		}
		n, _ := res.RowsAffected()
		if s.name == "restaurants" {
			totalRestaurants = int(n)
		}
		log.Info("etl step done", slog.String("step", s.name), slog.Int64("rows", n))
	}
	if totalRestaurants == 0 {
		return errors.New("no restaurants found in raw_supabase — is the dump loaded?")
	}
	log.Info("restaurants etl complete", slog.Int("restaurants", totalRestaurants))
	return nil
}
