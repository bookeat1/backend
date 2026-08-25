-- +goose Up

-- Город становится справочником (ADR-023, дополнение от 2026-08-25).
--
-- ЧТО БЫЛО, и что именно чинится здесь:
--
-- 1. Единственным списком городов были ДВЕ КОНСТАНТЫ В КОДЕ
--    (internal/domain/restaurant.go: CityAstana / CityAlmaty). Таблицы `cities`
--    не существовало — миграции 0061 и 0078 прямо это оговаривают («Not a FK:
--    there is no cities table»), поэтому город везде лежит как varchar и ни на
--    что не ссылается. Добавить город = релиз бэкенда. Перевести название на
--    казахский/английский было негде вообще.
--
-- 2. `restaurants.city` — свободная строка без единой проверки на уровне базы.
--    Из старой системы (legacysync.Sink.UpsertRestaurant и разовый cmd/etl)
--    туда при ВСТАВКЕ может лечь любое написание, и заведение просто перестаёт
--    находиться фильтром каталога (`r.city = $n` — точное сравнение строк).
--    На бою на 2026-08-25 таких значений нет: «Алматы» 43, «Астана» 2, пустых
--    нет, — но это везение, а не гарантия.
--
-- ЧЕГО ЭТА МИГРАЦИЯ НЕ ДЕЛАЕТ:
--
-- * `restaurants.city` НЕ переписывается и НЕ удаляется — ровно как
--    `cuisine_type` в 0079. Это контракт обратной совместимости: сборка в
--    магазине читает город строкой и шлёт эту же строку в `?city=`. Колонка
--    остаётся NOT NULL и остаётся тем, по чему реально фильтрует каталог.
-- * Город НЕ становится обязательной ссылкой. `city_id` — nullable, см.
--    подробное обоснование у самой колонки ниже.

SET lock_timeout = '3s';

-- city_key — ЕДИНСТВЕННАЯ нормализация написания города, общая для уникального
-- индекса, таблицы синонимов, переноса данных и триггера ниже. Ровно то же
-- самое делает internal/domain.NormalizeCityKey в Go: обрезать края, схлопнуть
-- внутренние пробелы, привести к нижнему регистру. Две реализации одной
-- нормализации разъезжаются рано или поздно, поэтому SQL-сторона тут одна
-- функция, а не повторённое в пяти местах выражение.
-- +goose StatementBegin
CREATE FUNCTION city_key(v text) RETURNS text AS
$$
SELECT lower(btrim(regexp_replace(coalesce(v, ''), '\s+', ' ', 'g')));
$$ LANGUAGE sql IMMUTABLE;
-- +goose StatementEnd

CREATE TABLE cities
(
    -- code — постоянный машинный ключ города (латиница, snake_case).
    -- Название редактируется и переводится, code — нет: по нему приходит
    -- фильтр, не зависящий от языка, и по нему клиент подбирает свои
    -- локальные ассеты.
    id            uuid PRIMARY KEY,
    code          varchar(64) NOT NULL,
    name          varchar     NOT NULL,
    name_i18n     jsonb,
    display_order integer     NOT NULL DEFAULT 0,
    -- is_active = false: «скрыть». Удаления города у платформы нет — на него
    -- ссылаются заведения (FK RESTRICT), а его название лежит строкой в
    -- restaurants.city у живых строк.
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_cities_code ON cities (code);
-- Уникальность по НОРМАЛИЗОВАННОМУ названию: «Алматы» и «алматы» не могут
-- сосуществовать — иначе повторится история кухонь, где регистр разводил один
-- и тот же город на два разных значения фильтра.
CREATE UNIQUE INDEX uq_cities_name_normalized ON cities (city_key(name));
-- Под основную выборку справочника: активные, по порядку показа, затем по имени.
CREATE INDEX idx_cities_active_order ON cities (is_active, display_order, name);

-- Синонимы написания города. Одна и та же таблица закрывает три задачи:
--   * фильтр от СТАРОГО клиента, который шлёт название строкой («Алматы»);
--   * фильтр от нового клиента, который шлёт код («almaty»);
--   * распознавание значения, приехавшего из старой системы при вставке.
-- Alias хранится УЖЕ нормализованным — через city_key() выше, тот же ключ, что
-- строит internal/domain.NormalizeCityKey на стороне Go.
CREATE TABLE city_aliases
(
    alias      varchar     PRIMARY KEY,
    city_id    uuid        NOT NULL REFERENCES cities (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_city_aliases_city ON city_aliases (city_id);

-- ---------------------------------------------------------------------------
-- Перенос данных. Идемпотентен целиком: каждая вставка ON CONFLICT DO NOTHING,
-- UPDATE только там, где связи ещё нет. Повторный прогон (в том числе
-- Down + Up) даёт тот же результат и ничего не дублирует.
-- ---------------------------------------------------------------------------

-- Два города из констант в коде. id зафиксированы (uuid v5 в namespace DNS от
-- «bookeat.city.<code>» — та же схема, что у кухонь в 0079), чтобы повторный
-- прогон и ЛЮБАЯ среда (тест, стейдж, прод) получили одни и те же
-- идентификаторы: иначе city_id из дампа не совпал бы между контурами.
--
-- display_order повторяет порядок domain.Cities() — Астана, потом Алматы.
-- Это не эстетика: GET /cities отдаёт названия в этом порядке, и старая
-- сборка в магазине показывает их списком как есть.
INSERT INTO cities (id, code, name, name_i18n, display_order)
VALUES ('452c6951-5bde-5a1b-b1b4-8a4c938ae456', 'astana', 'Астана',
        '{"kk": "Астана", "en": "Astana"}'::jsonb, 10),
       ('f157fb6e-7c0a-51d8-9526-37870bc306bf', 'almaty', 'Алматы',
        '{"kk": "Алматы", "en": "Almaty"}'::jsonb, 20)
ON CONFLICT (id) DO NOTHING;

-- Синонимы: собственное название и собственный код — всегда.
INSERT INTO city_aliases (alias, city_id)
SELECT city_key(c.name), c.id FROM cities c
ON CONFLICT (alias) DO NOTHING;
INSERT INTO city_aliases (alias, city_id)
SELECT city_key(c.code), c.id FROM cities c
ON CONFLICT (alias) DO NOTHING;

-- Исторические и латинские написания. Это не догадки: Астана в 2019-2022
-- официально называлась Нур-Султан, Алматы до 1993 — Алма-Ата, и обе формы
-- реально встречаются в старых базах и в поисковых запросах. Синоним НИЧЕГО
-- не переименовывает — он только позволяет узнать город по чужой строке.
INSERT INTO city_aliases (alias, city_id)
SELECT v.alias, c.id
FROM (VALUES ('нур-султан', 'astana'),
             ('нур султан', 'astana'),
             ('nur-sultan', 'astana'),
             ('nursultan', 'astana'),
             ('алма-ата', 'almaty'),
             ('алма ата', 'almaty'),
             ('alma-ata', 'almaty')) AS v(alias, code)
         JOIN cities c ON c.code = v.code
ON CONFLICT (alias) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Связь заведения со справочником.
-- ---------------------------------------------------------------------------

-- city_id NULLABLE, и это решение, а не недосмотр:
--
--   * В таблицу restaurants ведут ДВЕ двери из старой системы —
--     legacysync.Sink.UpsertRestaurant и ручной cmd/etl (ADR-023). Обе при
--     ВСТАВКЕ пишут город строкой и про справочник не знают. NOT NULL уронил
--     бы им INSERT, а «пусть падает, потом починим» на живой синхронизации
--     означает молча остановившийся приток новых заведений.
--   * Неизвестный город обязан быть представим. NOT NULL заставил бы либо
--     заводить город автоматически (мусор в справочнике), либо отклонять
--     заведение целиком (потеря данных). NULL здесь читается однозначно:
--     «строка города есть, в справочнике такого нет» — это очередь на разбор,
--     а не поломка.
--
-- ON DELETE RESTRICT: город, на который ссылаются заведения, удалить нельзя —
-- его прячут через is_active.
ALTER TABLE restaurants
    ADD COLUMN city_id uuid REFERENCES cities (id) ON DELETE RESTRICT;

-- Обратное направление фильтра: «заведения этого города».
CREATE INDEX idx_restaurants_city_id ON restaurants (city_id);

-- ---------------------------------------------------------------------------
-- Триггер: строка и ссылка не расходятся, кто бы ни писал.
--
-- Он существует ровно потому, что дверей в таблицу больше одной и закрыть их
-- по очереди в коде уже не получилось однажды (ADR-023, «две двери»). Триггер
-- закрывает их все сразу и не требует, чтобы старый импортёр вообще знал о
-- справочнике.
--
-- Правила, по приоритету:
--   INSERT   — city_id не задан: резолвим из строки по синонимам (это и есть
--              путь старого импортёра). Узнали город — строка ПРИВОДИТСЯ к
--              названию из справочника. Это не косметика: каталог фильтрует
--              точным сравнением строк, и заведение, приехавшее как
--              «Нур-Султан», иначе было бы привязано к Астане, но не нашлось
--              бы по фильтру «Астана». Не узнали — строка остаётся как есть,
--              придумывать за старую систему нечего.
--   UPDATE   — сменили city_id: он главный, строка подтягивается за ним
--              (так работает переименование города в кабинете).
--              Сменили только строку: главная строка, ссылка перерезолвится
--              (в том числе в NULL, если город неизвестен), а узнанная строка
--              приводится к каноническому написанию — так PATCH
--              /restaurants/:id с полем city продолжает работать как раньше,
--              но «алматы» больше не заводит второе написание в каталог.
--              Не меняли ничего, а ссылки нет: пробуем дорезолвить. Это
--              самолечение строк, чей город завели в справочнике уже потом.
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION restaurants_sync_city() RETURNS trigger AS
$$
DECLARE
    resolved uuid;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.city_id IS NULL THEN
            SELECT a.city_id INTO NEW.city_id
            FROM city_aliases a
            WHERE a.alias = city_key(NEW.city);
        END IF;
        IF NEW.city_id IS NOT NULL THEN
            SELECT c.name INTO NEW.city FROM cities c WHERE c.id = NEW.city_id;
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.city_id IS DISTINCT FROM OLD.city_id THEN
        IF NEW.city_id IS NOT NULL THEN
            SELECT c.name INTO NEW.city FROM cities c WHERE c.id = NEW.city_id;
        END IF;
    ELSIF NEW.city IS DISTINCT FROM OLD.city THEN
        SELECT a.city_id INTO resolved
        FROM city_aliases a
        WHERE a.alias = city_key(NEW.city);
        NEW.city_id := resolved;
        IF resolved IS NOT NULL THEN
            SELECT c.name INTO NEW.city FROM cities c WHERE c.id = resolved;
        END IF;
    ELSIF NEW.city_id IS NULL THEN
        SELECT a.city_id INTO NEW.city_id
        FROM city_aliases a
        WHERE a.alias = city_key(NEW.city);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_restaurants_sync_city
    BEFORE INSERT OR UPDATE
    ON restaurants
    FOR EACH ROW
EXECUTE FUNCTION restaurants_sync_city();

-- Проставить ссылку существующим заведениям. Делается ПОСЛЕ создания триггера
-- намеренно: тот же UPDATE проводит строку города через триггер, и заведение,
-- записанное как «  алматы » или «Нур-Султан», получает и ссылку, и
-- каноническое написание. Иначе оно было бы привязано к городу, но не нашлось
-- бы фильтром каталога, который сравнивает строки точно.
UPDATE restaurants r
SET city_id = a.city_id
FROM city_aliases a
WHERE r.city_id IS NULL
  AND a.alias = city_key(r.city);

-- +goose Down

DROP TRIGGER IF EXISTS trg_restaurants_sync_city ON restaurants;
DROP FUNCTION IF EXISTS restaurants_sync_city();
-- Строка города не восстанавливается и не чистится: её эта миграция никогда
-- не переписывала, поэтому откат возвращает схему ровно в исходное состояние.
ALTER TABLE restaurants
    DROP COLUMN city_id;
DROP TABLE city_aliases;
DROP TABLE cities;
DROP FUNCTION IF EXISTS city_key(text);
