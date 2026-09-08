-- +goose Up

-- Доводка справочника кухонь: раскладка СПОРНЫХ написаний.
--
-- Миграция 0079 сознательно оставила без кухонь всё, что нельзя было разложить
-- автоматически: составные значения («Авторская, европейская»), тип заведения,
-- записанный в поле кухни («Кофейня», «Винный бар»), и кухню с уточнением в
-- скобках («Японская (идзакая)»). Автосплит по запятой был запрещён именно
-- потому, что он ошибается на последнем случае.
--
-- Раскладку ниже утвердил владелец 2026-08-25. Она разовая и явная: список
-- `approved` — единственный источник истины, никакой автоматики.
--
-- ПОЧЕМУ ЗДЕСЬ ПОЯВИЛСЯ «Винный бар», которого не было в замере 24 августа.
-- Значение завели ПОСЛЕ замера: заведение Agora wine and deli обновлено
-- 2026-08-24 19:17 UTC, замер снят в 18:36 UTC. Это не промах инвентаризации,
-- а живое редактирование каталога — тем же вечером в кабинете правили ещё
-- пять заведений. Пока кухня остаётся свободной строкой, между этой миграцией
-- и выкатом может появиться новое написание, поэтому в конце файла стоит
-- отчёт (RAISE NOTICE), а не молчание.

-- Две новые кухни. id детерминированные (uuid v5 от «bookeat.cuisine.<code>»),
-- как и двенадцать из 0079.
INSERT INTO cuisines (id, code, name, display_order)
VALUES ('44271db8-9be4-5a8f-9751-be89b91afcf1', 'authors', 'Авторская', 130),
       ('9de2530f-a6c3-587e-baeb-718c739319ec', 'japanese', 'Японская', 140)
ON CONFLICT (id) DO NOTHING;

-- Переводы — только из уже накопленных данных, как в 0079. Ни одного
-- придуманного перевода.
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

-- Синонимы для новых кухонь: собственное название и код.
INSERT INTO cuisine_aliases (alias, cuisine_id)
SELECT lower(btrim(c.name)), c.id FROM cuisines c
ON CONFLICT (alias) DO NOTHING;
INSERT INTO cuisine_aliases (alias, cuisine_id)
SELECT lower(btrim(c.code)), c.id FROM cuisines c
ON CONFLICT (alias) DO NOTHING;

-- «Японская (идзакая)» — НАСТОЯЩИЙ синоним: это одна кухня, уточнение в
-- скобках отбрасывается. Поэтому написание уезжает в таблицу синонимов и
-- будет распознано, если приедет снова.
--
-- «Кафе, европейская» синонимом НЕ становится, хотя тоже даёт одну кухню:
-- это не другое написание «Европейской», а составное значение, из которого
-- выброшен тип заведения. Оно разложено ниже, в явном списке.
INSERT INTO cuisine_aliases (alias, cuisine_id)
SELECT 'японская (идзакая)', id FROM cuisines WHERE code = 'japanese'
ON CONFLICT (alias) DO NOTHING;

-- Утверждённая раскладка спорных написаний.
--
-- Читается так: заведение, у которого cuisine_type нормализуется в `legacy`,
-- получает кухню `code` на позиции `position` (0 — главная).
--
-- Чего в списке НЕТ и почему:
--   «Кофейня»     — тип заведения, не кухня. Кухня не проставляется.
--   «Винный бар»  — то же самое.
--   «Кафе» из «Кафе, европейская» — то же самое, поэтому у этого заведения
--                   остаётся одна кухня «Европейская», а не две.
-- Все три — решение владельца от 2026-08-25, а не пропуск.
INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position)
SELECT r.id, c.id, a.position
FROM (VALUES ('авторская, европейская', 'authors', 0),
             ('авторская, европейская', 'european', 1),
             ('европейская, казахская', 'european', 0),
             ('европейская, казахская', 'kazakh', 1),
             ('морепродукты, европейская', 'seafood', 0),
             ('морепродукты, европейская', 'european', 1),
             ('кафе, европейская', 'european', 0),
             ('японская (идзакая)', 'japanese', 0)) AS a(legacy, code, position)
         JOIN cuisines c ON c.code = a.code
         JOIN restaurants r ON lower(btrim(r.cuisine_type)) = a.legacy
ON CONFLICT (restaurant_id, cuisine_id) DO NOTHING;

-- Тот же идемпотентный проход по синонимам, что и в 0079: теперь он подхватит
-- всё, что стало распознаваемым благодаря двум новым кухням.
INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position)
SELECT r.id, a.cuisine_id, 0
FROM restaurants r
         JOIN cuisine_aliases a ON a.alias = lower(btrim(r.cuisine_type))
ON CONFLICT (restaurant_id, cuisine_id) DO NOTHING;

-- Отчёт, а не тишина. Заведение без кухонь после этой миграции — это либо одно
-- из трёх осознанно оставленных, либо НОВОЕ написание, появившееся уже после
-- утверждения раскладки. Второе должно быть видно в логе выката сразу, а не
-- обнаруживаться через неделю по пустому фильтру.
-- +goose StatementBegin
DO $$
DECLARE
    unresolved text;
BEGIN
    SELECT string_agg(DISTINCT r.cuisine_type, ' | ' ORDER BY r.cuisine_type)
    INTO unresolved
    FROM restaurants r
    WHERE NOT EXISTS (SELECT 1 FROM restaurant_cuisines rc WHERE rc.restaurant_id = r.id)
      AND lower(btrim(r.cuisine_type)) NOT IN ('кофейня', 'винный бар', '');

    IF unresolved IS NOT NULL THEN
        RAISE NOTICE 'НЕРАСПОЗНАННЫЕ НАПИСАНИЯ КУХНИ (заведения остались без кухонь): %', unresolved;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- Снимаем ровно то, что создала эта миграция: связи спорных написаний и две
-- новые кухни. Связи, созданные 0079 по однозначным значениям, не трогаются —
-- их условие (совпадение написания со списком ниже) не выполняется.
DELETE FROM restaurant_cuisines rc
USING restaurants r
WHERE rc.restaurant_id = r.id
  AND lower(btrim(r.cuisine_type)) IN ('авторская, европейская',
                                       'европейская, казахская',
                                       'морепродукты, европейская',
                                       'кафе, европейская',
                                       'японская (идзакая)');

DELETE FROM cuisine_aliases WHERE alias = 'японская (идзакая)';
-- Синонимы двух кухонь уедут каскадом вместе с самими кухнями.
DELETE FROM cuisines WHERE code IN ('authors', 'japanese');
