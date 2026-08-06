-- Seed data for the mobile Home ("Explore") screen: cuisines, promotions,
-- articles. RU sample rows for staging/dev only — NOT a migration.
--
-- Run AFTER migration 0012_home_feeds.sql has been applied. Idempotent: it
-- clears the three tables first, so re-running gives a clean known set.
--
--   psql "$DATABASE_URL" -f seeds/home_feeds.sql
--
-- (or, against the app's configured DB:)
--   psql "$APP_DATABASE_URL" -f seeds/home_feeds.sql
--
-- Requires gen_random_uuid() (PostgreSQL 13+ core; earlier versions need the
-- pgcrypto extension). Promotions are tied to whatever active restaurants
-- already exist; if none exist they fall back to a NULL restaurant_id
-- (a global promotion), which the schema allows.

BEGIN;

TRUNCATE cuisines, promotions, articles;

-- Cuisines ------------------------------------------------------------------
INSERT INTO cuisines (id, name, image_url, sort, is_active) VALUES
    (gen_random_uuid(), 'Итальянская',  NULL, 10, true),
    (gen_random_uuid(), 'Казахская',    NULL, 20, true),
    (gen_random_uuid(), 'Пекарня',      NULL, 30, true),
    (gen_random_uuid(), 'Морепродукты', NULL, 40, true),
    (gen_random_uuid(), 'Азиатская',    NULL, 50, true),
    (gen_random_uuid(), 'Грузинская',   NULL, 60, true),
    (gen_random_uuid(), 'Кофейня',      NULL, 70, true);

-- Promotions ----------------------------------------------------------------
-- Tie the first two promotions to two distinct active restaurants when they
-- exist (deterministic ORDER BY created_at, id); leave the rest global.
WITH ranked AS (
    SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn
    FROM restaurants
    WHERE is_active = true
)
INSERT INTO promotions (id, restaurant_id, title, discount_label, starts_at, ends_at, image_url, is_active, sort)
VALUES
    (gen_random_uuid(),
     (SELECT id FROM ranked WHERE rn = 1),
     'Скидка 30% на всё меню', '-30%',
     now() - interval '1 day', now() + interval '30 days', NULL, true, 10),
    (gen_random_uuid(),
     (SELECT id FROM ranked WHERE rn = 2),
     'Минус 15% на первый заказ', '-15%',
     now() - interval '1 day', now() + interval '60 days', NULL, true, 20),
    (gen_random_uuid(),
     NULL,
     'Счастливые часы: кофе -20%', '-20%',
     now() - interval '1 day', now() + interval '14 days', NULL, true, 30);

-- Articles ------------------------------------------------------------------
INSERT INTO articles (id, title, author_label, cover_url, url, published_at, is_active, sort) VALUES
    (gen_random_uuid(), 'Куда сходить на неделе', 'Редакция BookEat', NULL,
     'https://book-eat.com/blog/where-to-go', now() - interval '2 days', true, 10),
    (gen_random_uuid(), 'От BookEat: 5 новых заведений Астаны', 'От BookEat', NULL,
     'https://book-eat.com/blog/new-venues-astana', now() - interval '5 days', true, 20),
    (gen_random_uuid(), 'Гид по морепродуктам в городе', 'Редакция BookEat', NULL,
     'https://book-eat.com/blog/seafood-guide', NULL, true, 30);

COMMIT;
