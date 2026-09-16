-- +goose Up

-- ФУДИ-ПРОФИЛЬ ГОСТЯ (4-экранный визард мобилки, PR #222 bookeat1/frontend,
-- ветка feat/mobile-foodie-profile: кухни/диеты/аллергии/бюджет). До этой
-- миграции у визарда не было бэкенда вовсе — состояние жило только в
-- React Context и терялось при выходе из флоу.
--
-- ПОЧЕМУ НЕ user_cuisine_preferences / cuisines (migration 0079). Тот
-- справочник — UUID-таблица кухонь РЕСТОРАНОВ, которую ведёт платформа и на
-- которую ссылается venue.cuisines; сравнение с ней питает
-- FeedSignalCuisineMatch. Плитки визарда — СВОЙ статичный список
-- string-id ("kazakh", "asian", ...), заданный мобильным фронтендом
-- (foodie-profile-options.ts) и НЕ совпадающий с этим справочником ни по
-- набору значений, ни по типу ключа (нет UUID вовсе). Смешать их значило бы
-- либо завести в cuisines 15 строк, которые ни один venue никогда не
-- выберет, либо молча ронять половину выборов гостя, у которых нет
-- соответствия. Диеты и аллергии физически не могут лечь на cuisines —
-- это другая ось выбора, а не альтернативные кухни.
--
-- ФОРМА ХРАНЕНИЯ. cuisines/diets/allergies — множественный выбор без FK
-- на существующий справочник (значения статичные, версия списка живёт в
-- Go-константах internal/domain, а не в БД — см. правило CLAUDE.md
-- «VARCHAR для перечислений, проверка в приложении, не DB ENUM/CHECK»,
-- то же решение, что и у cuisine-плиток самого визарда). Три отдельные
-- таблицы вместо одной с массивами: у каждой оси свой набор допустимых
-- значений и своя мощность (cuisines ограничены пятью, diets держат
-- эксклюзивность no_diet, allergies без лимита) — отдельные строки читаются
-- и переписываются (DELETE all + INSERT) без разбора JSON/text[] на
-- стороне Go и без риска перепутать, какой массив к какой оси относится.
-- budget — РОВНО одно необязательное значение на гостя, поэтому лежит
-- колонкой прямо на users, как country_code/birth_date (migration 0021),
-- а не отдельной таблицей на один-единственный возможный ряд.
--
-- ЗАМЕНА ЦЕЛИКОМ. PUT /users/me/foodie-profile переписывает все четыре поля
-- одним запросом (весь визард сохраняется одним шагом на последнем экране),
-- поэтому Replace здесь — это DELETE всех строк гостя + INSERT нового
-- набора для каждой из трёх таблиц, как и Replace в usercuisine.
--
-- НОМЕР. На 2026-09-16 максимум по develop — 0108 (`0108_promo_codes.sql`);
-- 0109 не встречается ни в одном обнаруженном ворктри/ветке. Свободен.

SET lock_timeout = '3s';

ALTER TABLE users
    ADD COLUMN foodie_budget_tier varchar(16);

CREATE TABLE user_foodie_cuisines
(
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cuisine_id varchar(32) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, cuisine_id)
);

CREATE TABLE user_foodie_diets
(
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    diet_id    varchar(32) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, diet_id)
);

CREATE TABLE user_foodie_allergies
(
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    allergy_id  varchar(32) NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, allergy_id)
);

-- +goose Down

SET lock_timeout = '3s';

DROP TABLE IF EXISTS user_foodie_allergies;
DROP TABLE IF EXISTS user_foodie_diets;
DROP TABLE IF EXISTS user_foodie_cuisines;

ALTER TABLE users
    DROP COLUMN IF EXISTS foodie_budget_tier;
