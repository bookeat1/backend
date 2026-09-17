-- +goose Up

-- ФУДИ-ПРОФИЛЬ КАК СПРАВОЧНИК ПЛАТФОРМЫ (spec
-- foodie-profile-admin-dictionaries-20260916.md, BE-1). До этой миграции все
-- 36 вариантов визарда (15 кухонь, 10 диет, 8 аллергий, 3 бюджета) были
-- вшиты в Go-константы (internal/domain/foodie_profile.go) и в мобильный
-- фронтенд (packages/i18n) — добавить `korean` в подбор или переименовать
-- плитку могло только инженер, правкой кода и релизом.
--
-- ОДНА ТАБЛИЦА НА ЧЕТЫРЕ ВИДА. `kind` — varchar, не Postgres ENUM (CLAUDE.md:
-- «VARCHAR для перечислений, проверка в приложении»), проверяется в usecase.
-- Общая форма (название на трёх языках, картинка, порядок, is_active) везде
-- одна; `description`/`price_label`/`price_category` нужны только `budget`,
-- поэтому лежат nullable-колонками на общей таблице, а не в отдельной —
-- заводить четвёртую таблицу ради трёх колонок, которые не пустуют только у
-- 3 строк из 36, добавило бы JOIN каждому читателю без выгоды.
--
-- code НЕИЗМЕНЯЕМ и совпадает с тем, что уже лежит в `user_foodie_*.{cuisine,
-- diet,allergy}_id` и `users.foodie_budget_tier` (migration 0109) — ссылка
-- держится строкой, а не FK: составной ключ (kind, code) в FK не выразить
-- (то же решение, что в шапке 0109), а «удаления нет, только скрыть» снимает
-- саму необходимость целостности на уровне БД.
--
-- 🔴1 = A (Дамир, 2026-09-16): связь «плитка кухни → кухни заведений»
-- редактируется в админке, поэтому здесь же заводится
-- `foodie_option_cuisines` — обычная many-to-many, RESTRICT на cuisines (её
-- саму нельзя удалить, пока на неё есть связь; «скрыть» кухню можно — связь
-- переживает это, см. критерий 3.11 спеки).
--
-- 🔴2 = A: диеты остаются с правилами в коде (`dietAxes`,
-- internal/domain/taste_match.go) — эта миграция трогает только СПИСОК
-- вариантов диет (что показать гостю), не формулу очков.
--
-- ЧТО НЕ ТРОГАЕТСЯ: user_foodie_cuisines/diets/allergies, users.
-- foodie_budget_tier — те же колонки, те же значения кода. Go-константы
-- FoodieCuisineIDs/DietIDs/AllergyIDs/BudgetTierIDs и их Valid* уходят из
-- домена отдельным коммитом (BE-3), после того как проверка PUT
-- /users/me/foodie-profile переедет на чтение этой таблицы.
--
-- НОМЕР. На 2026-09-16 максимум по develop — 0109 (`0109_foodie_profile.sql`,
-- PR #135, уже в develop); 0110 не встречается ни в одном обнаруженном
-- ворктри/ветке (feat/media-custom-domain, feat/marathon-remainder тоже
-- проверены). Свободен.

SET lock_timeout = '3s';

CREATE TABLE foodie_options
(
    id                 uuid PRIMARY KEY,
    -- kind: cuisine / diet / allergy / budget. Проверка в usecase, не CHECK —
    -- тот же принцип, что и у code ниже.
    kind               varchar(16)  NOT NULL,
    -- code — постоянный машинный ключ, тот самый id, что визард шлёт в
    -- PUT /users/me/foodie-profile. ≤32 (бюджет пишется в users.
    -- foodie_budget_tier varchar(16), поэтому 16 символов — фактический
    -- потолок у budget, но колонка общая и не сужается по kind).
    code               varchar(32)  NOT NULL,
    name               varchar      NOT NULL,
    name_i18n          jsonb,
    -- image_url — картинка плитки. R2, POST /admin/media/images. NULL =
    -- заглушка (сегодняшнее поведение для 9 из 15 кухонь и всех диет/
    -- аллергий, см. спека 3.3) — не сломанная плитка.
    image_url          varchar,
    -- description/price_label — используются мобилкой только у budget
    -- (спека §5); у cuisine/diet/allergy остаются NULL.
    description        varchar,
    description_i18n   jsonb,
    price_label        varchar,
    price_label_i18n   jsonb,
    -- price_category — только у budget, значения как restaurants.
    -- price_category ('₸'/'₸₸'/'₸₸₸'); проверка допустимости в usecase.
    price_category     varchar(8),
    display_order      integer      NOT NULL DEFAULT 0,
    -- is_active = false: «скрыть». Жёсткого удаления в API нет (критерий 8) —
    -- на код может ссылаться user_foodie_* без FK, а скрытый вариант должен
    -- быть виден тому, кто его скрыл, чтобы его можно было вернуть.
    is_active          boolean      NOT NULL DEFAULT true,
    created_at         timestamptz  NOT NULL DEFAULT now(),
    updated_at         timestamptz  NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_foodie_options_kind_code ON foodie_options (kind, code);
-- Уникальность по нормализованному названию В ПРЕДЕЛАХ вида — «Корейская» и
-- «корейская» не могут сосуществовать как две кухни, но «seafood» разрешён
-- одновременно как кухня и как аллергия (критерий 3.12/6.12 спеки).
CREATE UNIQUE INDEX uq_foodie_options_kind_name_normalized ON foodie_options (kind, lower(btrim(name)));
-- Под основную выборку: вид, активность, порядок показа, затем имя —
-- ровно то, что требует критерий 4 (GET /foodie-profile/options).
CREATE INDEX idx_foodie_options_kind_active_order ON foodie_options (kind, is_active, display_order, name);

-- Связь «плитка кухни → кухни заведений» (🔴1 = A). Только для kind='cuisine'
-- — проверка в usecase, не CHECK (нужен был бы подзапрос на другую таблицу,
-- Postgres CHECK такого не умеет без триггера, а триггер здесь лишний вес).
CREATE TABLE foodie_option_cuisines
(
    option_id  uuid NOT NULL REFERENCES foodie_options (id) ON DELETE CASCADE,
    -- RESTRICT: скрыть кухню в справочнике можно, а удалить, пока на неё
    -- ссылается плитка (или заведение), нельзя — жёсткого удаления в API
    -- кухонь и так нет, RESTRICT здесь для симметрии с restaurant_cuisines.
    cuisine_id uuid NOT NULL REFERENCES cuisines (id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (option_id, cuisine_id)
);
CREATE INDEX idx_foodie_option_cuisines_cuisine ON foodie_option_cuisines (cuisine_id);

-- ---------------------------------------------------------------------------
-- Сид: ровно 36 вариантов, совпадающих с сегодняшними константами
-- internal/domain/foodie_profile.go. Названия ru/kk/en взяты один в один из
-- packages/i18n/src/{ru,kk,en}.ts (`onboarding.foodieProfile.*.options`,
-- сверено глазами 2026-09-16) — ничего не придумано, ничего не изменилось на
-- экране гостя в день выката. id — uuid v5 от
-- «bookeat.foodie_option.<kind>.<code>» (та же схема NewSHA1(NameSpaceDNS,
-- ...), что и «bookeat.cuisine.<code>» в migration 0079), поэтому повторный
-- прогон (в том числе Down + Up) не дублирует и не падает — ON CONFLICT (id)
-- DO NOTHING на каждой вставке, как в 0079/0080.
-- ---------------------------------------------------------------------------

-- Кухни (display_order = позиция в CUISINE_OPTIONS × 10).
INSERT INTO foodie_options (id, kind, code, name, name_i18n, display_order)
VALUES ('d95774ed-983c-556a-abcb-4bb718f1df94', 'cuisine', 'kazakh', 'Казахская', '{"kk":"Қазақ","en":"Kazakh"}', 10),
       ('c7d3555d-a02f-5654-8014-b34ed6181c6e', 'cuisine', 'asian', 'Азиатская', '{"kk":"Азиялық","en":"Asian"}', 20),
       ('48cff942-4809-5972-8be3-157f10bb5401', 'cuisine', 'european', 'Европейская', '{"kk":"Еуропалық","en":"European"}', 30),
       ('6cb7392c-a05a-5a12-81a4-94c5f3d29eab', 'cuisine', 'japanese', 'Японская', '{"kk":"Жапон","en":"Japanese"}', 40),
       ('f4e2386a-112f-5deb-9435-571d9fa3a54a', 'cuisine', 'italian', 'Итальянская', '{"kk":"Итальян","en":"Italian"}', 50),
       ('f12c7e57-81b0-5f6c-8af0-f32882f26b33', 'cuisine', 'korean', 'Корейская', '{"kk":"Кәріс","en":"Korean"}', 60),
       ('ad1363d4-45d7-5823-adae-266620cd0e85', 'cuisine', 'seafood', 'Морепродукты', '{"kk":"Теңіз өнімдері","en":"Seafood"}', 70),
       ('d30b28d3-e6cb-590b-a448-d0daae560ff3', 'cuisine', 'meat', 'Мясо', '{"kk":"Ет","en":"Meat"}', 80),
       ('e2209952-bc95-5737-87b8-e38d6e2f3a27', 'cuisine', 'vegan', 'Веганская', '{"kk":"Веган","en":"Vegan"}', 90),
       ('ef2984f5-b8c9-5e89-a9f9-fe59fe92f24a', 'cuisine', 'desserts', 'Десерты', '{"kk":"Десерттер","en":"Desserts"}', 100),
       ('13130d38-d368-540d-9899-f4ee57b7efdb', 'cuisine', 'coffee', 'Кофе', '{"kk":"Кофе","en":"Coffee"}', 110),
       ('fd58bcdb-1e2b-54f3-958a-fc9dac8bbbf9', 'cuisine', 'healthy', 'Здоровая еда', '{"kk":"Пайдалы тағам","en":"Healthy food"}', 120),
       ('f6f34a44-437d-54f4-8d6a-a7369064a34e', 'cuisine', 'fastfood', 'Фастфуд', '{"kk":"Фастфуд","en":"Fast food"}', 130),
       ('a8233ef9-dd3f-596f-993c-f5195dbbb4ca', 'cuisine', 'spicy', 'Острая', '{"kk":"Ащы","en":"Spicy"}', 140),
       ('b46a9b8f-b744-5c86-a31f-4f4c87b25147', 'cuisine', 'bbq', 'Барбекю', '{"kk":"Барбекю","en":"BBQ"}', 150)
ON CONFLICT (id) DO NOTHING;

-- Диеты (DIET_OPTIONS × 10).
INSERT INTO foodie_options (id, kind, code, name, name_i18n, display_order)
VALUES ('e9a74d12-4a7a-57c2-baee-408ffa786ad2', 'diet', 'no_diet', 'Без диеты', '{"kk":"Диетасыз","en":"No diet"}', 10),
       ('c0ccb6fc-dd33-5b94-8000-107aeaf4e36d', 'diet', 'vegan', 'Веган', '{"kk":"Веган","en":"Vegan"}', 20),
       ('f8b3657c-9177-5906-b611-16678ba72537', 'diet', 'pescetarian', 'Пескетарианец', '{"kk":"Пескетариан","en":"Pescetarian"}', 30),
       ('4efa5845-7680-5bd3-bd3a-9e7075bac16f', 'diet', 'halal', 'Халяль', '{"kk":"Халал","en":"Halal"}', 40),
       ('cd852adf-2603-5d98-b143-c8fd0d98f2e3', 'diet', 'kosher', 'Кошерное', '{"kk":"Кошер","en":"Kosher"}', 50),
       ('c7e444fb-03b0-57c4-a1d9-7ff7e3450ee9', 'diet', 'keto', 'Кето', '{"kk":"Кето","en":"Keto"}', 60),
       ('55c08aac-2e20-5a25-8e7c-cb3b2a97a7f4', 'diet', 'low_carb', 'Лоу-карб', '{"kk":"Аз көмірсулы","en":"Low-carb"}', 70),
       ('4afe9921-14ba-5cf9-b1c2-a9bd3e247660', 'diet', 'paleo', 'Палео', '{"kk":"Палео","en":"Paleo"}', 80),
       ('96fad211-b3e9-5d4b-b7bb-f0cbaaeefd55', 'diet', 'no_lactose', 'Без лактозы', '{"kk":"Лактозасыз","en":"Lactose-free"}', 90),
       ('9f2ab8d7-2655-5ed5-a2a0-2e54cfbc9cb8', 'diet', 'no_gluten', 'Без глютена', '{"kk":"Глютенсіз","en":"Gluten-free"}', 100)
ON CONFLICT (id) DO NOTHING;

-- Аллергии (ALLERGY_OPTIONS × 10).
INSERT INTO foodie_options (id, kind, code, name, name_i18n, display_order)
VALUES ('d4fb60f9-5232-5bdd-942f-f77356339d3e', 'allergy', 'nuts', 'Орехи', '{"kk":"Жаңғақ","en":"Nuts"}', 10),
       ('ede1f797-76ee-5f4c-aaa2-ae8eecaf92a4', 'allergy', 'dairy', 'Молочные', '{"kk":"Сүт өнімдері","en":"Dairy"}', 20),
       ('d0293183-c6af-5416-a59c-521b7a23324e', 'allergy', 'eggs', 'Яйца', '{"kk":"Жұмыртқа","en":"Eggs"}', 30),
       ('e5935b5b-0c0d-54c5-be6a-263f68312a7e', 'allergy', 'seafood', 'Морепродукты', '{"kk":"Теңіз өнімдері","en":"Seafood"}', 40),
       ('0a9985ce-ff75-518c-8ea4-fc4244c2df05', 'allergy', 'soy', 'Соя', '{"kk":"Соя","en":"Soy"}', 50),
       ('e3e8a6d4-6d30-548c-b5f0-2beb78eb1ffb', 'allergy', 'wheat', 'Пшеница', '{"kk":"Бидай","en":"Wheat"}', 60),
       ('a879b0d5-db38-5f68-922d-d8c6e031f95d', 'allergy', 'shellfish', 'Моллюски', '{"kk":"Моллюскалар","en":"Shellfish"}', 70),
       ('3006959c-956f-58e3-a965-80903336fe45', 'allergy', 'sesame', 'Кунжут', '{"kk":"Күнжіт","en":"Sesame"}', 80)
ON CONFLICT (id) DO NOTHING;

-- Бюджет (BUDGET_TIERS × 10) — единственный вид с description/price_label/
-- price_category. price_category как в §5.2/критерии 2 спеки: budget ↔ ₸,
-- mid ↔ ₸₸, premium ↔ ₸₸₸ (restaurants.price_category scale).
INSERT INTO foodie_options (
    id, kind, code, name, name_i18n,
    description, description_i18n, price_label, price_label_i18n, price_category, display_order
)
VALUES
    ('16b1d55f-6c83-5a9c-a1bb-1d39f98724b6', 'budget', 'budget', 'Бюджетный',
     '{"kk":"Үнемді","en":"Budget"}',
     'Кофейни, fast casual, завтраки и быстрые встречи.',
     '{"kk":"Кофеханалар, fast casual, таңғы ас және жылдам кездесулер.","en":"Cafés, fast casual, breakfasts and quick meetups."}',
     'до 5 000 ₸',
     '{"kk":"5 000 ₸ дейін","en":"up to ₸5,000"}',
     '₸', 10),
    ('200685c2-3266-558d-8e12-38cd53eed788', 'budget', 'mid', 'Средний',
     '{"kk":"Орташа","en":"Mid-range"}',
     'Основной диапазон для ресторанов, ужинов и встреч.',
     '{"kk":"Мейрамханалар, кештер мен кездесулер үшін негізгі диапазон.","en":"The main range for restaurants, dinners and meetups."}',
     '5 000–15 000 ₸',
     '{"kk":"5 000–15 000 ₸","en":"₸5,000–15,000"}',
     '₸₸', 20),
    ('dc4241a1-92f7-5395-95db-bd082e050099', 'budget', 'premium', 'Премиум',
     '{"kk":"Премиум","en":"Premium"}',
     'Fine dining, авторская кухня и более камерный сервис.',
     '{"kk":"Fine dining, авторлық ас мәзірі және неғұрлым жеке қызмет.","en":"Fine dining, signature cuisine and a more intimate service."}',
     'от 15 000 ₸',
     '{"kk":"15 000 ₸ бастап","en":"from ₸15,000"}',
     '₸₸₸', 30)
ON CONFLICT (id) DO NOTHING;

-- Связи «плитка кухни → кухни заведений» (🔴1 = A), ровно
-- internal/domain/taste_match.go's FoodieCuisineDictionaryCodes на
-- 2026-09-16. Матчинг ПО КОДУ кухни через JOIN — код, отсутствующий в
-- cuisines на этой среде (сегодня это `indian`: справочник кухонь его не
-- содержит), молча пропускается, как того требует критерий 2 спеки, а не
-- падает и не создаёт кухню сам.
INSERT INTO foodie_option_cuisines (option_id, cuisine_id)
SELECT o.id, c.id
FROM (VALUES ('kazakh', 'kazakh'),
             ('asian', 'pan_asian'),
             ('asian', 'japanese'),
             ('asian', 'indian'),
             ('european', 'european'),
             ('european', 'french'),
             ('european', 'mediterranean'),
             ('european', 'greek'),
             ('japanese', 'japanese'),
             ('italian', 'italian'),
             ('seafood', 'seafood'),
             ('vegan', 'vegan')) AS m(tile_code, cuisine_code)
         JOIN foodie_options o ON o.kind = 'cuisine' AND o.code = m.tile_code
         JOIN cuisines c ON c.code = m.cuisine_code
ON CONFLICT (option_id, cuisine_id) DO NOTHING;

-- +goose Down

SET lock_timeout = '3s';

DROP TABLE IF EXISTS foodie_option_cuisines;
DROP TABLE IF EXISTS foodie_options;
