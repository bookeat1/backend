// Command etl performs the one-time migration of identity data from a Supabase
// dump (loaded into the raw_supabase staging schema) into the clean users and
// user_credentials tables. It is idempotent — re-running upserts by id.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"

	"backend-core/internal/auth/phone"
	"backend-core/internal/bootstrap"
	"backend-core/internal/logger"
)

func main() {
	cfg, err := bootstrap.NewConfig()
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log := logger.New(cfg.App.LogLevel, cfg.App.LogFormat)
	db, err := bootstrap.NewSQLDB(cfg.DB.Postgres)
	if err != nil {
		log.Error("connect db", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	target := "users"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	var runErr error
	switch target {
	case "users":
		runErr = run(context.Background(), db, log)
	case "restaurants":
		runErr = runRestaurants(context.Background(), db, log)
	case "menu":
		runErr = runMenu(context.Background(), db, log)
	case "bookings":
		runErr = runBookings(context.Background(), db, cfg.Booking, log)
	default:
		log.Error("unknown etl target", slog.String("target", target))
		os.Exit(1)
	}
	if runErr != nil {
		log.Error("etl failed", slog.String("error", runErr.Error()))
		os.Exit(1)
	}
	log.Info("etl complete")
}

// run reads staged rows and upserts clean records. It joins auth users with
// profiles on id, preferring profile values where present.
func run(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	const q = `
		SELECT au.id::text,
		       au.email,
		       COALESCE(p.phone, au.raw_user_meta_data->>'phone')          AS phone,
		       COALESCE(p.full_name, au.raw_user_meta_data->>'full_name','') AS full_name,
		       COALESCE(p.role, 'user')                                     AS role,
		       p.avatar_url,
		       COALESCE(p.preferred_language, 'ru')                         AS preferred_language,
		       p.city,
		       au.encrypted_password
		FROM raw_supabase.users au
		LEFT JOIN raw_supabase.profiles p ON p.id = au.id`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()

	var migrated, withPassword int
	for rows.Next() {
		var (
			id, fullName, role, lang             string
			email, rawPhone, avatar, city, encPw sql.NullString
		)
		if err := rows.Scan(&id, &email, &rawPhone, &fullName, &role, &avatar, &lang, &city, &encPw); err != nil {
			return err
		}

		var phonePtr any
		if rawPhone.Valid {
			if n := phone.Normalize(rawPhone.String); n != "" {
				phonePtr = n
			}
		}

		const upsertUser = `
			INSERT INTO users (id, email, phone, full_name, role, avatar_url, preferred_language, city, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now(), now())
			ON CONFLICT (id) DO UPDATE SET
			  email=EXCLUDED.email, phone=EXCLUDED.phone, full_name=EXCLUDED.full_name,
			  role=EXCLUDED.role, avatar_url=EXCLUDED.avatar_url,
			  preferred_language=EXCLUDED.preferred_language, city=EXCLUDED.city,
			  updated_at=now()`
		if _, err := db.ExecContext(ctx, upsertUser,
			id, nullStr(email), phonePtr, fullName, role, nullStr(avatar), lang, nullStr(city),
		); err != nil {
			return err
		}

		if encPw.Valid && encPw.String != "" {
			const upsertCred = `
				INSERT INTO user_credentials (user_id, password_hash) VALUES ($1,$2)
				ON CONFLICT (user_id) DO UPDATE SET password_hash=EXCLUDED.password_hash`
			if _, err := db.ExecContext(ctx, upsertCred, id, encPw.String); err != nil {
				return err
			}
			withPassword++
		}
		migrated++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if migrated == 0 {
		return errors.New("no rows found in raw_supabase — is the dump loaded?")
	}
	log.Info("etl summary", slog.Int("users", migrated), slog.Int("with_password", withPassword))
	return nil
}

// nullStr converts a sql.NullString to any (nil when invalid) for parameters.
func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}
