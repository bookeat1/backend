# Спека домена: restaurants (Wave 1)

**Дата:** 2026-07-13
**Роадмап:** `2026-07-13-backend-migration-roadmap.md`
**Зависит от:** users (готово)
**Питает:** menu (Wave 2), bookings (Wave 3), orders (Wave 4), reviews/offers/social (Wave 5)

Каталог ресторанов — фундамент всего транзакционного ядра. Реализация переписывается
с нуля по паттерну users; данные переносятся идемпотентным ETL из `raw_supabase.*` с
сохранением UUID.

---

## 1. Состав домена (таблицы Supabase → сущности)

| Supabase table | Сущность backend-core | Заметки |
|---|---|---|
| `restaurants` | `Restaurant` | корень агрегата; i18n-поля, city/price_category как VARCHAR |
| `restaurant_categories` | `Category` | справочник (не привязан к ресторану), `restaurants.category_id` → FK |
| `restaurant_features` | `Feature` | 1—N от ресторана |
| `restaurant_images` | `Image` | 1—N, флаг `is_primary` |
| `restaurant_tags` | `Tag` | 1—N |
| `restaurant_social_links` | `SocialLink` | 1—N, `type` (instagram/…)+`url` |
| `restaurant_working_hours` | `WorkingHours` | 7 строк на ресторан (day_of_week) |
| `restaurant_time_slots` | `TimeSlot` | сетка бронирования (day_of_week, start/end) |
| `restaurant_tables` | `RestaurantTable` | физические столы, `capacity` |
| `restaurant_floor_plans` | `FloorPlan` | `layout_data jsonb` — непрозрачный passthrough |
| `restaurant_managers` | `RestaurantManager` | связь user↔restaurant, гейтит `/restaurant/*` |
| `restaurant_partnership_requests` | `PartnershipRequest` | публичная лид-форма без FK; отдельный мелкий ресурс |

`restaurant_surveys` **не входит** в этот домен — ссылается на `booking_id`, едет в Wave 3.

---

## 2. Схема (goose-миграция `000X_restaurants.sql`)

Стиль — как `0001_users_auth.sql`: `VARCHAR` для перечислимых полей, `jsonb` для
i18n, PK = оригинальный UUID, FK на уже перенесённые таблицы. Ключевые колонки:

```
restaurants
  id uuid PK
  category_id uuid REFERENCES restaurant_categories(id)   -- nullable
  name varchar, name_i18n jsonb
  description varchar, description_i18n jsonb
  cuisine_type varchar, cuisine_type_i18n jsonb
  address varchar, address_i18n jsonb
  opening_hours varchar, opening_hours_i18n jsonb
  city varchar NOT NULL                 -- ex-enum: almaty/astana/... (валидация в коде)
  price_category varchar NOT NULL        -- ex-enum: "₸" / "₸₸" / "₸₸₸"
  email varchar, phone varchar
  latitude double precision, longitude double precision   -- nullable
  kwaaka_restaurant_id varchar           -- внешний id, интеграция вне скоупа
  is_active bool NOT NULL DEFAULT true
  is_new / is_popular / is_premium bool  -- nullable флаги витрины
  hidden_from_home bool NOT NULL DEFAULT false
  display_order int                      -- nullable, сортировка витрины
  created_at / updated_at timestamptz

restaurant_categories (id PK, name, name_i18n, description, description_i18n, created_at)
restaurant_features   (id PK, restaurant_id FK, name, name_i18n, created_at)
restaurant_images     (id PK, restaurant_id FK, image_url, is_primary bool, created_at)
restaurant_tags       (id PK, restaurant_id FK, tag_name, tag_name_i18n, created_at)
restaurant_social_links (id PK, restaurant_id FK, type, url, created_at)
restaurant_working_hours (id PK, restaurant_id FK, day_of_week int, open_time, close_time, is_open bool, created_at, updated_at)
restaurant_time_slots (id PK, restaurant_id FK, day_of_week int, start_time, end_time, is_manually_disabled bool, created_at, updated_at)
restaurant_tables     (id PK, restaurant_id FK, name, capacity int, description, is_active bool, created_at, updated_at)
restaurant_floor_plans (id PK, restaurant_id FK, layout_data jsonb, created_at, updated_at)
restaurant_managers   (id PK, restaurant_id FK, user_id FK→users, created_by uuid, whatsapp_opt_in bool, whatsapp_phone varchar, created_at)
restaurant_partnership_requests (id PK, restaurant_name, contact_name, email, phone, address, cuisine_type, description, additional_info, status varchar DEFAULT 'pending', created_at, updated_at)
```

Индексы: FK-колонки `restaurant_id`; `restaurants(is_active, display_order, name)`
под витринный листинг; `restaurant_managers(user_id)` под гейтинг back office;
`restaurant_images(restaurant_id, is_primary)`.

`day_of_week`/`open_time`/`start_time` — валидируются в коде (0–6, HH:MM).

---

## 3. Domain-слой (`internal/domain/`, один файл на сущность)

- `restaurant.go` — `Restaurant`, `RestaurantRepository`, типы `City` и
  `PriceCategory` (named string + константы), геттеры возвращают `ErrNotFound`.
  i18n-поля — `map[string]string` (маршалятся в/из `jsonb`).
- `restaurant_table.go`, `restaurant_timeslot.go`, `restaurant_working_hours.go`,
  `restaurant_manager.go`, `restaurant_media.go` (features/images/tags/social как
  мелкие связанные структуры — можно сгруппировать, чтобы не плодить файлы под
  тривиальные 1—N справочники), `restaurant_category.go`, `floor_plan.go`,
  `partnership_request.go`.
- Никакой бизнес-логики и фреймворков в domain (правило CLAUDE.md).

**Форма агрегата чтения.** `RestaurantRepository.GetByID` собирает
ресторан + images + features + tags + social_links (паритет вложенного
`select` в `useRestaurants`/`fetchRestaurantById`). Столы, слоты, рабочие часы,
floor plan, менеджеры — отдельными методами репозитория, не всегда в агрегате.

---

## 4. Usecase-слой (`internal/usecase/restaurants/`)

По settled-паттерну (`facade.go` + focused-файлы):

- **`Facade`** — публичное чтение и админ-CRUD ресторанов:
  - `ListActive(ctx)` — активные, сортировка `display_order` (nulls last) → `name`,
    с primary-image (паритет `useRestaurants`).
  - `GetByID(ctx, id)` — агрегат.
  - `Create/Update/SetActive/Delete` — админские мутации ресторана и его
    вложенных коллекций (images/features/tags/social/hours/slots/tables).
- **`managers.go` → `ManagerUseCase`** — назначить/снять/списать менеджеров
  ресторана; используется гейтингом `/restaurant/*`. Проверка «этот user
  управляет этим рестораном» — свободная функция над минимальным портом.
- **`availability.go` → `AvailabilityReadUseCase`** — только **чтение** сетки
  (time_slots + working_hours + tables) для отдачи фронту. Собственно
  *вычисление* доступности на дату (RPC `get_bookings_availability`,
  `get_booking_total_capacity`) относится к брони и **переносится в Wave 3**;
  здесь — только исходные данные. Слоты читаются и правятся вручную (через
  админ-CRUD, §5); RPC-регенерация (`regenerate_time_slots_for_restaurant`,
  `regenerate_all_time_slots`) в Wave 1 **не переносится**.
- **`ports.go`** — минимальные локальные порты: чтение `users` (проверка, что
  назначаемый менеджер существует), `domain.TxManager` для атомарных мутаций
  агрегата (ресторан + вложенные коллекции в одной транзакции).

---

## 5. Transport-слой (`internal/transport/rest/restaurants/`)

Публичные (без auth либо под `middleware.Auth`, но без роли):

- `GET /api/restaurants` — листинг активных с фильтрами (query-параметры):
  `city`, `category`, `is_popular`, `is_new`, текстовый поиск по имени; пагинация.
- `GET /api/restaurants/:id` — агрегат.
- `GET /api/restaurants/:id/menu` — **заглушка/404 до Wave 2** (документируется,
  реализуется в menu).
- `GET /api/restaurant-categories` — справочник.
- `POST /api/partnership-requests` — публичная лид-форма (rate-limit желателен).

Manager/admin-gated (`middleware.RequireRole`):

- `POST/PATCH/DELETE /api/admin/restaurants[...]` — CRUD + управление вложенными
  коллекциями (images/features/tags/social/hours/slots/tables/floor_plan).
- `GET/POST/DELETE /api/restaurants/:id/managers` — менеджеры (admin, либо
  менеджер того же ресторана).

Все ответы — `response.Envelope` (`OK`/`Created`/`Error`); все ошибки — через
`response.HandleError` (маппинг доменных sentinel'ов на 404/409/403/401/422/500);
после записи ошибки — немедленный `return`. DTO: `request.go`
(`validate:"..."` + `Validate()` + `ToDomain()`), `response.go` (`fromDomain()`).

---

## 6. ETL (`cmd/etl`, подкоманда `restaurants`)

Идемпотентный upsert-by-id из `raw_supabase.*` в чистую схему. Порядок с учётом FK:
`restaurant_categories` → `restaurants` → (features, images, tags, social_links,
working_hours, time_slots, tables, floor_plans, managers) → `partnership_requests`.

Маппинг:
- UUID `id` — verbatim (сохранение связей с users и будущими доменами).
- i18n-колонки (`*_i18n`) — `jsonb` verbatim.
- `city`, `price_category` — verbatim в VARCHAR (были DB-энумы).
- `restaurant_managers.user_id` — должен существовать в `users` (перенесён в
  Wave 0); строки-сироты логируются и пропускаются, не роняют прогон.
- nullable-поля (lat/long, флаги витрины, category_id) — переносятся как есть.

Повторный запуск безопасен; финальный прогон на cutover добирает дельту (см.
роадмап §5).

---

## 7. Definition of Done (Wave 1)

- Миграция применяется и откатывается (`make migrate-up` / `-down`).
- ETL идемпотентен на реальном дампе; повторный прогон не плодит дублей.
- Юнит-тесты (hand-written fakes) зелёные под `go test -short`.
- Интеграционные тесты репозиториев зелёные за `TEST_DATABASE_URL`.
- `GET /api/restaurants` и `GET /api/restaurants/:id` отдают данные, паритетные
  тому, что сейчас возвращают `useRestaurants`/`fetchRestaurantById`.
- `gofmt -w .` и `go vet ./...` чистые.
