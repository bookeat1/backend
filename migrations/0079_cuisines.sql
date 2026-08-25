-- +goose Up

-- Кухня заведения становится справочником (ADR-022).
--
-- ЧТО БЫЛО СЛОМАНО, и что именно чинится здесь:
--
-- 1. Кухня жила свободной строкой `restaurants.cuisine_type`. 45 заведений дали
--    18 разных написаний, из них ЧЕТЫРЕ — попытка указать две кухни через
--    запятую в одном поле («Кафе, европейская», «Авторская, европейская»,
--    «Морепродукты, европейская», «Европейская, казахская»). Фильтр каталога
--    сравнивает строку точно (`r.cuisine_type = ANY($n)`), поэтому такое
--    заведение не находилось НИ ПО ОДНОЙ из перечисленных в нём кухонь.
--    Плюс регистр гуляет: «Европейская» и «европейская» — два разных фильтра.
--
-- 2. `user_cuisine_preferences` (миграция 0021) ссылается на
--    `restaurant_categories` — справочник ТИПОВ заведения (ресторан / кафе /
--    кофейня), а вовсе не кухонь. Тот справочник пуст (0 записей на бою), и
--    `restaurants.category_id` не проставлен ни у одного заведения. Прямое
--    следствие: сигнал ранжирования `cuisine_match` в
--    internal/domain/feed_ranking.go — 400 очков, самый весомый органический —
--    НЕ СРАБАТЫВАЛ НИКОГДА, потому что обеих сторон сравнения не существовало.
--    Главная лента ранжировалась вообще без персонализации, и заметить это было
--    нельзя: код есть, тесты есть, данных нет. Здесь таблица переезжает на
--    настоящий справочник кухонь. В ней 0 записей на бою, так что это по факту
--    смена колонки, а не миграция данных.
--
-- ЧЕГО ЭТА МИГРАЦИЯ НЕ ДЕЛАЕТ:
--
-- * `restaurants.cuisine_type` / `cuisine_type_i18n` НЕ трогаются и не
--   удаляются. Это контракт обратной совместимости с магазинными сборками
--   (1.4 в проде, 1.5 на проверке): клиент читает одну строку. Дальше она
--   становится ПРОИЗВОДНОЙ — пересобирается приложением как перечисление
--   названий кухонь через запятую при каждой смене набора. Здесь она остаётся
--   ровно такой, какой была, — в том числе поэтому Down полностью обратим.
-- * Спорные написания НЕ раскладываются автоматически. Раскладка составных
--   значений и «Кофейня» / «Японская (идзакая)» ждёт решения владельца; такие
--   заведения остаются БЕЗ связей в `restaurant_cuisines`, их исходная строка
--   `cuisine_type` цела и продолжает работать в выдаче и в фильтре.
--   Автоматический split по запятой запрещён осознанно: «Японская (идзакая)» —
--   одна кухня с уточнением, а не две.

CREATE TABLE cuisines
(
    -- code — постоянный машинный ключ кухни (латиница, snake_case). Названия
    -- редактируются и переводятся, code — нет: по нему клиент подбирает вшитую
    -- картинку-фолбэк и по нему же приходят фильтры, не зависящие от языка.
    id            uuid PRIMARY KEY,
    code          varchar(64) NOT NULL,
    name          varchar     NOT NULL,
    name_i18n     jsonb,
    -- image_url — картинка кружка кухни в приложении. Хранится в справочнике
    -- (R2), чтобы новая кухня появлялась с фотографией БЕЗ релиза в магазин.
    image_url     varchar,
    display_order integer     NOT NULL DEFAULT 0,
    -- is_active = false: «скрыть». Удаления кухни у платформы нет — на неё
    -- могут ссылаться заведения и предпочтения гостей (обе связи RESTRICT).
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_cuisines_code ON cuisines (code);
-- Уникальность по НОРМАЛИЗОВАННОМУ названию — это и есть защита от повторения
-- истории: «Европейская» и «европейская» больше не могут сосуществовать.
CREATE UNIQUE INDEX uq_cuisines_name_normalized ON cuisines (lower(btrim(name)));
-- Под основную выборку справочника: активные, по порядку показа, затем по имени.
CREATE INDEX idx_cuisines_active_order ON cuisines (is_active, display_order, name);

-- Связь «у заведения много кухонь». Пара уникальна первичным ключом; position
-- задаёт порядок показа, первая кухня считается главной.
CREATE TABLE restaurant_cuisines
(
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    -- RESTRICT, а не CASCADE: удалить кухню, которой уже пользуются заведения,
    -- нельзя — её прячут через is_active.
    cuisine_id    uuid        NOT NULL REFERENCES cuisines (id) ON DELETE RESTRICT,
    position      integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (restaurant_id, cuisine_id)
);

-- «Кухни заведения по порядку» — PK покрывает только поиск по restaurant_id,
-- порядок берётся отсюда.
CREATE INDEX idx_restaurant_cuisines_rid_position ON restaurant_cuisines (restaurant_id, position);
-- «Заведения по кухне» — обратное направление фильтра каталога.
CREATE INDEX idx_restaurant_cuisines_cuisine ON restaurant_cuisines (cuisine_id);

-- Синонимы написания. Одна кухня — много написаний, которые ей соответствуют.
-- Alias хранится УЖЕ нормализованным (lower + btrim), нормализация одна и та же
-- в SQL и в Go (internal/domain.NormalizeCuisineKey).
--
-- Таблица закрывает три задачи сразу:
--   * фильтр от старого клиента, который шлёт название строкой («европейская»);
--   * распознавание значения, приехавшего из старой системы;
--   * перенос данных ниже: связь создаётся ТОЛЬКО там, где написание есть в
--     этой таблице. То есть таблица синонимов и есть утверждённая раскладка,
--     а всё, что в неё не попало, осознанно остаётся без кухни.
CREATE TABLE cuisine_aliases
(
    alias      varchar     PRIMARY KEY,
    cuisine_id uuid        NOT NULL REFERENCES cuisines (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_cuisine_aliases_cuisine ON cuisine_aliases (cuisine_id);

-- ---------------------------------------------------------------------------
-- Перенос данных. Идемпотентен целиком: каждая вставка ON CONFLICT DO NOTHING,
-- UPDATE трогает только ещё не заполненные переводы. Повторный прогон (в том
-- числе Down + Up) даёт тот же результат и ничего не дублирует.
-- ---------------------------------------------------------------------------

-- Двенадцать ОДНОЗНАЧНЫХ значений из боевой базы (замер 2026-08-24).
-- id зафиксированы (uuid v5 от «bookeat.cuisine.<code>»), чтобы повторный
-- прогон и любая другая среда получили те же самые идентификаторы.
INSERT INTO cuisines (id, code, name, display_order)
VALUES ('a02af5dd-9899-563c-997c-f07b8fde8aee', 'european', 'Европейская', 10),
       ('ccd2b778-439f-5532-bc31-08c0557c40e0', 'mediterranean', 'Средиземноморская', 20),
       ('8c00ea8d-ae06-5f16-b54f-dfd1e5fd3103', 'seafood', 'Морепродукты', 30),
       ('1770b802-586d-51cf-8349-272758c6ae9f', 'kazakh', 'Казахская', 40),
       ('53b95cba-6b78-59c8-919c-204d3489c140', 'pan_asian', 'Паназиатская', 50),
       ('fbabea3f-a2ca-57e1-a4ce-bccbb3748fec', 'italian', 'Итальянская', 60),
       ('bcd57d7d-600b-5e5c-8de3-d8b3e6b23605', 'french', 'Французская', 70),
       ('57717dc8-3a80-5502-8688-43ac77062a2f', 'georgian', 'Грузинская', 80),
       ('149cddbe-37d6-5f41-9a90-682183ac872e', 'turkish', 'Турецкая', 90),
       ('c6af3eef-46e2-5702-ad30-cdc2bda4b091', 'greek', 'Греческая', 100),
       ('b4fdc36e-ef07-5ac2-9601-23a457fbfa5a', 'oriental', 'Восточная', 110),
       ('89415987-d444-5bb3-af18-ba4ba0e8f6fd', 'vegan', 'Веганская', 120)
ON CONFLICT (id) DO NOTHING;

-- Переводы НЕ придумываются: они берутся из уже накопленного
-- `restaurants.cuisine_type_i18n` тех заведений, чья строка кухни точно
-- совпадает с названием справочника. Где переводов не было — остаётся NULL,
-- и API отдаёт русское название (domain.I18n.Resolve).
UPDATE cuisines c
SET name_i18n = src.i18n
FROM (SELECT DISTINCT ON (lower(btrim(r.cuisine_type)))
             lower(btrim(r.cuisine_type)) AS key,
             r.cuisine_type_i18n           AS i18n
      FROM restaurants r
      WHERE r.cuisine_type_i18n IS NOT NULL
        AND jsonb_typeof(r.cuisine_type_i18n) = 'object'
        AND r.cuisine_type_i18n <> '{}'::jsonb
      ORDER BY lower(btrim(r.cuisine_type)), r.created_at) src
WHERE lower(btrim(c.name)) = src.key
  AND c.name_i18n IS NULL;

-- Синонимы: собственное название и собственный код — всегда.
INSERT INTO cuisine_aliases (alias, cuisine_id)
SELECT lower(btrim(c.name)), c.id FROM cuisines c
ON CONFLICT (alias) DO NOTHING;
INSERT INTO cuisine_aliases (alias, cuisine_id)
SELECT lower(btrim(c.code)), c.id FROM cuisines c
ON CONFLICT (alias) DO NOTHING;

-- Связи заведение → кухня. Матчинг ТОЛЬКО по таблице синонимов, то есть только
-- по однозначным написаниям. Составные («Кафе, европейская») и небесспорные
-- («Кофейня», «Японская (идзакая)») в синонимах отсутствуют, поэтому остаются
-- без связи до решения владельца — их `cuisine_type` при этом цел.
INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position)
SELECT r.id, a.cuisine_id, 0
FROM restaurants r
         JOIN cuisine_aliases a ON a.alias = lower(btrim(r.cuisine_type))
ON CONFLICT (restaurant_id, cuisine_id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Любимые кухни гостя переезжают с ТИПОВ заведения на кухни (см. пункт 2 выше).
-- Делается по колонкам, а не пересозданием таблицы, чтобы миграция была
-- безопасна и на таблице с живыми строками.
-- ---------------------------------------------------------------------------
ALTER TABLE user_cuisine_preferences
    ADD COLUMN cuisine_id uuid REFERENCES cuisines (id) ON DELETE RESTRICT;

-- Перенести старые значения невозможно в принципе: category_id указывает на
-- ДРУГУЮ сущность (тип заведения), соответствия «тип → кухня» не существует.
-- На бою здесь 0 строк при 122 пользователях, терять нечего. Если строки вдруг
-- есть — они удаляются осознанно, потому что они и так ни на что не влияли:
-- ни одно заведение не имеет category_id, сравнивать их было не с чем.
DELETE FROM user_cuisine_preferences WHERE cuisine_id IS NULL;

-- DROP COLUMN снимает и старый первичный ключ (user_id, category_id).
ALTER TABLE user_cuisine_preferences
    DROP COLUMN category_id;
ALTER TABLE user_cuisine_preferences
    ALTER COLUMN cuisine_id SET NOT NULL,
    ADD PRIMARY KEY (user_id, cuisine_id);

-- +goose Down

-- Возврат предпочтений на справочник типов заведения — раньше, чем уедут
-- сами кухни: иначе не даст внешний ключ.
ALTER TABLE user_cuisine_preferences
    ADD COLUMN category_id uuid REFERENCES restaurant_categories (id) ON DELETE CASCADE;
DELETE FROM user_cuisine_preferences WHERE category_id IS NULL;
ALTER TABLE user_cuisine_preferences
    DROP COLUMN cuisine_id;
ALTER TABLE user_cuisine_preferences
    ALTER COLUMN category_id SET NOT NULL,
    ADD PRIMARY KEY (user_id, category_id);

DROP TABLE restaurant_cuisines;
DROP TABLE cuisine_aliases;
DROP TABLE cuisines;
