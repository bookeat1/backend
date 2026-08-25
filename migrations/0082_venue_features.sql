-- +goose Up

-- Удобства заведения становятся справочником — тем же устройством, что кухни
-- (0079/0080) и города (0081). Раскладка спорных формулировок утверждена
-- владельцем 2026-08-25, список пересничан с ЖИВОЙ старой базы в тот же день
-- (29 строк / 23 формулировки / 10 заведений — новых написаний не появилось).
--
-- ЧТО СЛОМАНО СЕЙЧАС:
--
-- 1. Фильтр «Удобства» на экране поиска существует ТОЛЬКО в интерфейсе.
--    Значения лежат в `UiOnlyFacets` (apps/mobile/src/hooks/useSearch.ts) и в
--    запрос к серверу не уходят вовсе. Гость выбирает «Намазхана» — выдача не
--    меняется. Это хуже отсутствующего фильтра: приложение делает вид, что
--    отфильтровало.
-- 2. Данных под фильтр нет: `restaurant_features` на бою и на тесте — 0 строк.
-- 3. В старой системе те же особенности лежат свободным текстом, и в одном поле
--    свалены три разные оси: удобство («Терраса»), КУХНЯ («Восточная кухня»,
--    «Национальная кухня») и даже район («Коктобе»).
--
-- ЧТО ЭТА МИГРАЦИЯ ДЕЛАЕТ С `restaurant_features`:
--
-- УДАЛЯЕТ. Таблица пуста в обеих средах (проверено 2026-08-25), её единственным
-- писателем был ручной `cmd/etl` (в этой же работе он перестаёт её писать),
-- кабинет её не заполняет, приложение её не показывает. Держать рядом две
-- таблицы «особенности заведения» — свободнотекстовую и справочную — значит
-- воспроизвести ровно ту путаницу, ради которой всё затевалось.
-- Поле `features[]` в ответе `GET /restaurants/:id` СОХРАНЯЕТСЯ: тот же формат
-- (id / name / name_i18n), только теперь собирается из справочника.
-- Перед удалением содержимое таблицы (если оно вдруг есть в чьей-то среде)
-- переносится в связи по таблице синонимов, а нераспознанное печатается
-- в NOTICE — молча ничего не пропадает. Down пересоздаёт таблицу.

CREATE TABLE venue_features
(
    -- code — постоянный машинный ключ (латиница, snake_case). Названия
    -- редактируются и переводятся, code — нет: по нему приходит фильтр,
    -- не зависящий от языка, и по нему клиент подбирает иконку.
    id            uuid PRIMARY KEY,
    code          varchar(64) NOT NULL,
    name          varchar     NOT NULL,
    name_i18n     jsonb,
    display_order integer     NOT NULL DEFAULT 0,
    -- is_active = false: «скрыть». Удаления нет — на удобство ссылаются
    -- заведения (связь RESTRICT).
    is_active     boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_venue_features_code ON venue_features (code);
-- Уникальность по НОРМАЛИЗОВАННОМУ названию — защита от «Терраса» / «терраса»
-- как двух разных записей. Проверка в Go запрещена: гонка двух админов.
CREATE UNIQUE INDEX uq_venue_features_name_normalized ON venue_features (lower(btrim(name)));
CREATE INDEX idx_venue_features_active_order ON venue_features (is_active, display_order, name);

CREATE TABLE restaurant_venue_features
(
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    -- RESTRICT: удобство, которым уже пользуются заведения, не удаляют — прячут.
    feature_id    uuid        NOT NULL REFERENCES venue_features (id) ON DELETE RESTRICT,
    position      integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (restaurant_id, feature_id)
);

CREATE INDEX idx_restaurant_venue_features_rid_position ON restaurant_venue_features (restaurant_id, position);
-- Обратное направление — «заведения по удобству», это и есть фильтр каталога.
-- Фильтр И-семантики делает по одному EXISTS на каждое выбранное удобство,
-- поэтому индекс по feature_id обязателен, а не «желателен».
CREATE INDEX idx_restaurant_venue_features_feature ON restaurant_venue_features (feature_id);

-- Синонимы написания: одно удобство — много формулировок, которые ему
-- соответствуют. Alias хранится УЖЕ нормализованным (lower + btrim),
-- нормализация одна и та же в SQL и в Go (domain.NormalizeFeatureKey).
CREATE TABLE venue_feature_aliases
(
    alias      varchar     PRIMARY KEY,
    feature_id uuid        NOT NULL REFERENCES venue_features (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_venue_feature_aliases_feature ON venue_feature_aliases (feature_id);

-- ---------------------------------------------------------------------------
-- Наполнение. Идемпотентно целиком: каждая вставка ON CONFLICT DO NOTHING.
-- id зафиксированы (uuid v5 от «bookeat.feature.<code>») — тот же приём, что в
-- 0079: повторный прогон и любая другая среда дают те же идентификаторы.
-- ---------------------------------------------------------------------------

-- Переводы НЕ придуманы: ru/en/kk для первых восьми взяты из утверждённого
-- словаря приложения (packages/i18n/src/{ru,en,kk}.ts, ключ
-- search.filters.amenities); для «Терраса» и «Живая музыка» тот же текст лежит
-- и в старой базе. Там, где источника перевода нет, стоит NULL — API отдаст
-- русское название (domain.I18n.Resolve), а не выдуманный перевод.
INSERT INTO venue_features (id, code, name, name_i18n, display_order)
VALUES ('688fc12d-b985-5eff-9197-f6afc2750750', 'terrace', 'Терраса',
        '{"ru":"Терраса","en":"Terrace","kk":"Терраса"}'::jsonb, 10),
       ('b1ea1043-5c2a-52a6-b9c7-ff551779fb30', 'wifi', 'Wi-Fi',
        '{"ru":"Wi-Fi","en":"Wi-Fi","kk":"Wi-Fi"}'::jsonb, 20),
       ('8f53ac23-305e-5873-b8e1-b7a1a1e4e439', 'parking', 'Парковка',
        '{"ru":"Парковка","en":"Parking","kk":"Тұрақ"}'::jsonb, 30),
       ('fc825bed-bb74-5fb3-a4e8-44b8430b1950', 'halal', 'Халал',
        '{"ru":"Халал","en":"Halal","kk":"Халал"}'::jsonb, 40),
       ('2c1cfa3c-d675-5b98-bd22-60ca3e48835b', 'prayer_room', 'Намазхана',
        '{"ru":"Намазхана","en":"Prayer room","kk":"Намазхана"}'::jsonb, 50),
       -- «Детские стульчики», а не «Можно с детьми»: с детьми пускают почти
       -- везде, отличает заведение именно наличие стульчика (решение владельца).
       ('98e8dd59-7f0e-5dcc-878c-2ed81aa6dcab', 'kids_chairs', 'Детские стульчики',
        '{"ru":"Детские стульчики","en":"High chairs","kk":"Балалар орындығы"}'::jsonb, 60),
       -- «Без детей», а не «18+»: 18+ читается как возрастное ограничение по
       -- закону (алкоголь, контент), а гость ищет тихий вечер. Формулировка
       -- описывает атмосферу, а не правовой статус заведения.
       ('aca81ca9-7ac9-5b99-84cd-0991f8f10dd5', 'child_free', 'Без детей', NULL, 70),
       ('8dac4162-8d3f-5e86-b232-c6ea6c4a08bb', 'pets', 'Можно с питомцами',
        '{"ru":"Можно с питомцами","en":"Pet-friendly","kk":"Үй жануарларына рұқсат"}'::jsonb, 80),
       ('ab785310-4e06-53bc-af23-746f50ca75fa', 'live_music', 'Живая музыка',
        '{"ru":"Живая музыка","en":"Live music","kk":"Тірі музыка"}'::jsonb, 90),
       ('b71f4993-dd9d-58dd-bc5f-cf6f17fee30e', 'hookah', 'Кальян', NULL, 100),
       ('fa5ac96a-4e21-5216-90a2-43e4335aed9d', 'vip_rooms', 'Отдельные кабинеты', NULL, 110),
       ('d31f85f0-0098-5346-b448-7f0920fd7a5c', 'business_lunch', 'Бизнес-ланч', NULL, 120),
       ('aedf3106-317b-59ae-9328-7760a65ce71a', 'breakfast', 'Завтраки', NULL, 130),
       ('84996eaf-d02e-58dd-9ea4-9dc9a961399a', 'takeaway', 'Заказ на вынос', NULL, 140),
       ('d8f12bbf-3509-588c-baba-f27e3ba91814', 'view', 'Панорамный вид', NULL, 150),
       ('45eb60b9-267d-5962-9410-72697b0b241e', 'sports_broadcasts', 'Спортивные трансляции', NULL, 160),
       ('df850685-4535-5f6d-86d2-b4394ade7e6d', 'wine_list', 'Винная карта', NULL, 170),
       ('cbf6564c-a84f-5391-9471-27c00561b0ef', 'vegetarian_menu', 'Вегетарианское меню', NULL, 180),
       ('b0d772d6-8923-55a4-b23b-c6a00b4c8583', 'gluten_free_menu', 'Безглютеновое меню', NULL, 190)
ON CONFLICT (id) DO NOTHING;

-- Синонимы: собственное название и собственный код — всегда.
INSERT INTO venue_feature_aliases (alias, feature_id)
SELECT lower(btrim(f.name)), f.id FROM venue_features f
ON CONFLICT (alias) DO NOTHING;
INSERT INTO venue_feature_aliases (alias, feature_id)
SELECT lower(btrim(f.code)), f.id FROM venue_features f
ON CONFLICT (alias) DO NOTHING;

-- Утверждённая раскладка формулировок старой системы: НАСТОЯЩИЕ синонимы,
-- то есть другое написание того же самого. Их шесть, и все шесть реально
-- встречаются в старой базе. Синонимов «на будущее» здесь нет намеренно:
-- таблица синонимов — это утверждённая раскладка, а не догадки о том, как
-- когда-нибудь может написать менеджер.
--
-- Не заведены намеренно (это не синонимы, а другая ось классификации или шум):
--   «Восточная кухня», «Национальная кухня»      → КУХНЯ (справочник 0079)
--   «Рыбная витрина», «Устричная витрина»        → КУХНЯ (морепродукты)
--   «Коктобе»                                     → район/локация
--   «Колоритный интерьер», «Необычный интерьер»  → маркетинговый эпитет
--   «Профессиональный звук»                       → для арендатора зала, не для гостя
--   «Киновечера», «Чайные церемонии»             → события, им место в афише
--   «Постное меню»                                → см. ниже, связь заведена явно
INSERT INTO venue_feature_aliases (alias, feature_id)
VALUES ('бизнес ланч',               'd31f85f0-0098-5346-b448-7f0920fd7a5c'),
       ('английский завтрак',        'aedf3106-317b-59ae-9328-7760a65ce71a'),
       ('вип кабинки',               'fa5ac96a-4e21-5216-90a2-43e4335aed9d'),
       ('панорамный вид на горы',    'd8f12bbf-3509-588c-baba-f27e3ba91814'),
       ('вид на предгорные пейзажи', 'd8f12bbf-3509-588c-baba-f27e3ba91814'),
       ('lounge',                    'b71f4993-dd9d-58dd-bc5f-cf6f17fee30e')
ON CONFLICT (alias) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Перенос данных из старой системы.
--
-- Источник — НЕ эта база: `restaurant_features` здесь пуста, а 29 записей лежат
-- в Supabase. Поэтому раскладка вбита ЯВНЫМ СПИСКОМ (тот же приём, что в 0080),
-- по идентификаторам заведений — они совпадают в обеих системах, все 10
-- заведений проверены на бою 2026-08-25 и активны. Автоматики по свободному
-- тексту здесь нет и быть не должно.
--
-- 19 связей из 29 записей у 8 заведений. Chaihana Palau и INZHU остаются без
-- удобств: обе их «особенности» — это кухня и маркетинг.
--
-- Отдельно «Постное меню» → «Вегетарианское меню» у Aiza Miras. Связь заведена
-- ЯВНО и НЕ через синоним: постное меню и вегетарианское — не два написания
-- одного и того же (в постном допустима рыба). Владелец решил свести их в одно
-- удобство, но записывать «постное меню» синонимом значило бы утверждать
-- тождество, которого нет, и распознавать так любое будущее написание.
-- Тот же принцип, по которому в 0080 «Кафе, европейская» не стала синонимом.
INSERT INTO restaurant_venue_features (restaurant_id, feature_id, position)
SELECT v.rid::uuid, f.id, v.pos
FROM (VALUES
    -- 1100 Karaoke: Терраса, Вип кабинки (+ «Профессиональный звук» — мусор)
    ('21d70e1c-4a0d-43ee-b40e-e2205f4ba310', 'terrace', 0),
    ('21d70e1c-4a0d-43ee-b40e-e2205f4ba310', 'vip_rooms', 1),
    -- Abay: Вид на предгорные пейзажи (+ интерьер и «Национальная кухня» — не сюда)
    ('bdce796d-bf6a-41fa-b1ad-5076aa1ede38', 'view', 0),
    -- Aiza Esentai: Wi-Fi, Терраса, Бизнес ланч
    ('937b075f-c8b0-43e7-97a7-4d8f45c2b96a', 'wifi', 0),
    ('937b075f-c8b0-43e7-97a7-4d8f45c2b96a', 'terrace', 1),
    ('937b075f-c8b0-43e7-97a7-4d8f45c2b96a', 'business_lunch', 2),
    -- Aiza Miras: Wi-Fi, Английский завтрак, Заказ на вынос, Постное меню
    ('660fd375-ac57-4c0b-b461-9424cc3133d9', 'wifi', 0),
    ('660fd375-ac57-4c0b-b461-9424cc3133d9', 'breakfast', 1),
    ('660fd375-ac57-4c0b-b461-9424cc3133d9', 'takeaway', 2),
    ('660fd375-ac57-4c0b-b461-9424cc3133d9', 'vegetarian_menu', 3),
    -- Guinness Pub: Живая музыка, Спортивные трансляции
    ('2de7a222-8b5f-4926-b831-c5da789fb711', 'live_music', 0),
    ('2de7a222-8b5f-4926-b831-c5da789fb711', 'sports_broadcasts', 1),
    -- Hooqa Room: Lounge → Кальян, Живая музыка (+ киновечера и чайные церемонии — события)
    ('4282dd37-5b49-4a4d-8c3c-79f4423a5e7e', 'hookah', 0),
    ('4282dd37-5b49-4a4d-8c3c-79f4423a5e7e', 'live_music', 1),
    -- Koktobe Terrace: Терраса, Панорамный вид на горы (+ «Коктобе» — район)
    ('9f7ce1c4-606a-49f7-a17e-a22d20ea157d', 'terrace', 0),
    ('9f7ce1c4-606a-49f7-a17e-a22d20ea157d', 'view', 1),
    -- Mongol Bar Мирас: Терраса, Wi-Fi, Винная карта
    ('653782ce-5ed7-4e75-9575-bb2165368ecb', 'terrace', 0),
    ('653782ce-5ed7-4e75-9575-bb2165368ecb', 'wifi', 1),
    ('653782ce-5ed7-4e75-9575-bb2165368ecb', 'wine_list', 2)
) AS v(rid, code, pos)
         JOIN venue_features f ON f.code = v.code
-- Заведение могло не доехать в эту базу (тест, чистая среда) — тогда связи
-- просто не будет, миграция не падает.
         JOIN restaurants r ON r.id = v.rid::uuid
ON CONFLICT (restaurant_id, feature_id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Свободнотекстовая `restaurant_features` закрывается.
--
-- Сначала переносим то, что в ней лежит (в наших средах — ничего, но миграция
-- не имеет права рассчитывать на конкретную среду), потом печатаем всё, что не
-- распознали, и только потом удаляем таблицу.
-- ---------------------------------------------------------------------------
INSERT INTO restaurant_venue_features (restaurant_id, feature_id, position)
SELECT rf.restaurant_id, a.feature_id, 0
FROM restaurant_features rf
         JOIN venue_feature_aliases a ON a.alias = lower(btrim(rf.name))
ON CONFLICT (restaurant_id, feature_id) DO NOTHING;

-- +goose StatementBegin
DO
$$
    DECLARE
        unmapped text;
    BEGIN
        SELECT string_agg(DISTINCT rf.name, ', ')
        INTO unmapped
        FROM restaurant_features rf
                 LEFT JOIN venue_feature_aliases a ON a.alias = lower(btrim(rf.name))
        WHERE a.alias IS NULL;
        IF unmapped IS NOT NULL THEN
            RAISE NOTICE 'venue_features: свободнотекстовые особенности НЕ распознаны и удаляются вместе с таблицей: %', unmapped;
        END IF;
    END
$$;
-- +goose StatementEnd

DROP TABLE restaurant_features;

-- +goose Down

-- Свободнотекстовая таблица возвращается ровно в том виде, в каком была
-- (схема из миграции 0002). Данных в ней на момент удаления не было — вернуть
-- нечего, и это единственная причина, по которой удаление вообще допустимо.
CREATE TABLE restaurant_features
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid              NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    name          varchar           NOT NULL,
    name_i18n     jsonb,
    created_at    timestamptz       NOT NULL DEFAULT now()
);
CREATE INDEX idx_restaurant_features_rid ON restaurant_features (restaurant_id);

DROP TABLE restaurant_venue_features;
DROP TABLE venue_feature_aliases;
DROP TABLE venue_features;
