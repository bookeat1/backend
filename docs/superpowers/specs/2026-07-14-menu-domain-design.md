# Спека домена: menu (Wave 2)

**Дата:** 2026-07-14
**Роадмап:** `2026-07-13-backend-migration-roadmap.md`
**Зависит от:** users (готово), restaurants (Wave 1, ветка `feat/restaurants-domain`)
**Питает:** bookings (Wave 3 — booking_items → menu_items), orders (Wave 4)

Меню ресторана. Реализация переписывается с нуля по паттерну restaurants; данные
переносятся идемпотентным ETL из `raw_supabase.*` с сохранением UUID. Ветка Wave 2
(`feat/menu-domain`) строится поверх `feat/restaurants-domain`.

---

## 1. Состав домена (таблицы Supabase → сущности)

| Supabase table | Сущность backend-core | Заметки |
|---|---|---|
| `menu_items` | `MenuItem` | корень; принадлежит ресторану (FK); i18n-поля; `category`/`subcategory` — **текст**, не FK |
| `menu_item_tags` | `MenuItemTag` | 1—N от позиции; `tag` — простая строка (без i18n) |
| `menu_categories` | `MenuCategory` | справочник с иерархией (`parent_id`); **не FK-связан** с `menu_items` |

Ключевые факты из book-eat (проверено по коду):
- Consumer-меню грузится `menu_items.select('*').eq('restaurant_id', id)` с фильтром
  по `language` (`ru` ⇒ `language='ru' OR language IS NULL`; иначе `language=<lang>`),
  сортировка `display_order ASC, name`. Группировка по строковому `category`
  (fallback «Другое») делается **на клиенте** — сервер отдаёт плоский список.
- `menu_categories` — отдельный back-office справочник (иерархия parent→children,
  `display_order`). Связь с позициями только по названию категории (строка), **без FK**.
  Переносим как самостоятельную сущность; связь в FK **не реформируем** (сохраняем данные).
- `menu_items.language` — реальное поле мультиязычного меню (некоторые рестораны
  держат позиции в нескольких языковых вариантах строками). Переносим как nullable.

---

## 2. Схема (goose-миграция `000X_menu.sql`)

Стиль как `0002_restaurants.sql`: `VARCHAR` для перечислимого, `jsonb` для i18n,
PK = оригинальный UUID, FK на уже перенесённые таблицы.

```
menu_categories
  id uuid PK
  name varchar NOT NULL
  name_i18n jsonb
  parent_id uuid REFERENCES menu_categories(id) ON DELETE SET NULL   -- иерархия
  display_order int NOT NULL DEFAULT 0
  created_at timestamptz

menu_items
  id uuid PK
  restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE
  name varchar NOT NULL,            name_i18n jsonb
  description varchar NOT NULL DEFAULT '', description_i18n jsonb
  price numeric(12,2) NOT NULL DEFAULT 0    -- см. §решение по цене
  image_url varchar
  is_available boolean NOT NULL DEFAULT true
  category varchar,                 category_i18n jsonb      -- текстовая категория
  subcategory varchar,              subcategory_i18n jsonb   -- текстовая подкатегория
  portion_size varchar,             portion_size_i18n jsonb
  language varchar                  -- nullable: мультиязычные строки меню
  display_order int
  created_at timestamptz, updated_at timestamptz

menu_item_tags
  id uuid PK
  menu_item_id uuid NOT NULL REFERENCES menu_items(id) ON DELETE CASCADE
  tag varchar NOT NULL
  created_at timestamptz
  UNIQUE (menu_item_id, tag)        -- один тег на позицию не дублируется
```

Индексы: `menu_items(restaurant_id, display_order, name)` под меню-листинг;
`menu_items(restaurant_id, language)` под языковой фильтр; `menu_item_tags(menu_item_id)`;
`menu_categories(parent_id)`.

**Цена (решение):** БД `numeric(12,2)`; в домене и API — **decimal-строка**
(`"4500.00"`), без `float` и без внешних decimal-библиотек (правило «no private deps»).
Repo читает `price::text`; DTO отдаёт строку; клиент парсит. Валидация формата в
usecase (regex `^\d+(\.\d{1,2})?$`).

---

## 3. Domain-слой (`internal/domain/`, один файл на сущность)

- `menu_item.go` — `MenuItem` (Price `string`; i18n-поля `domain.I18n`; `Language *string`),
  `MenuItemRepository`; `MenuItemFilter{RestaurantID, Language *string}`.
- `menu_category.go` — `MenuCategory` (self-ref `ParentID *uuid.UUID`),
  `MenuCategoryRepository`.
- Теги — часть агрегата позиции (`MenuItem.Tags []MenuItemTag`), replace-collection
  по образцу restaurant media; отдельного репозитория-интерфейса можно не плодить —
  теги читаются/替换аются вместе с позицией (порт `MenuItemRepository` включает
  `ReplaceTags`).
- Никакой бизнес-логики/фреймворков в domain.

**Форма чтения.** `MenuItemRepository.ListByRestaurant(ctx, filter)` — плоский список
позиций с их тегами (паритет `select('*')`), языковой фильтр в SQL. `GetByID` —
одна позиция с тегами.

---

## 4. Usecase-слой (`internal/usecase/menu/`)

По settled-паттерну (`facade.go` + focused-файлы + `ports.go`):

- **`Facade`**:
  - `ListByRestaurant(ctx, restaurantID, lang *string)` — публичное чтение (языковой
    фильтр как в MenuSheet: `ru`/nil ⇒ ru-или-null).
  - `Get(ctx, itemID)`.
  - `Categories(ctx)` — справочник (иерархия parent→children строится на клиенте либо
    отдаётся плоско; отдаём плоско, паритет back-office запроса).
  - `Create/Update/Delete(ctx, actor, restaurantID, ...)` — мутации позиции с тегами
    (replace-collection в TxManager), с проверкой принадлежности (см. §6 авторизация).
  - `SetAvailable(ctx, actor, restaurantID, itemID, bool)` — стоп-лист toggle.
  - Categories CRUD (admin/manager).
- **`ports.go`** — минимальный порт `managerChecker` (обёртка над
  `restaurants.ManagerUseCase.Manages`) для проверки прав; `domain.TxManager`.
- Валидация: `name` непустой, `price` соответствует формату, `restaurant_id` существует.

---

## 5. Transport-слой (`internal/transport/rest/menu/`)

Публичные (unauthenticated, группа `api`):
- `GET /api/v1/restaurants/:restaurantId/menu` — плоский список позиций;
  `?lang=ru|kk|en` (по умолчанию ru). Обёртка `response.Envelope`.
- `GET /api/v1/menu-categories` — справочник категорий.

Per-restaurant-gated (см. §6):
- `POST   /api/v1/restaurants/:restaurantId/menu-items`
- `PATCH  /api/v1/restaurants/:restaurantId/menu-items/:itemId`
- `DELETE /api/v1/restaurants/:restaurantId/menu-items/:itemId`
- `PATCH  /api/v1/restaurants/:restaurantId/menu-items/:itemId/availability`

Admin-only (справочник глобальный, не привязан к ресторану):
- `POST/PATCH/DELETE /api/v1/menu-categories` — `RequireRole(admin)`.

DTO: `request.go` (`Validate()`+`ToInput()`), `response.go` (`fromDomain()`). Все
ответы — `response.Envelope`; ошибки — `response.HandleError`; `return` сразу после
ошибки. `PATCH` позиции — **opt-in семантика** (как исправлено в Wave 1: менять только
переданные поля; теги заменять только если ключ передан).

---

## 6. Авторизация: per-restaurant гейтинг (+ закрытие Wave 1 follow-up)

Решение: мутации меню и ресторанов доступны админам **и менеджерам своего ресторана**.

**Новый `middleware.RequireRestaurantManager(managers)`** (`internal/transport/rest/middleware/`):
- читает `AuthUser`; `admin` → `Next()`; `restaurant`-роль → `Manages(userID, :restaurantId)`
  из пути → `Next()` при `true`, иначе `403`; прочее → `403`; нет auth → `401`.
- зависит от порта `Manages(ctx, userID, restaurantID) (bool, error)`
  (реализован `restaurants.ManagerUseCase`, прокидывается через `deps`).

**IDOR-защита в usecase.** Middleware проверяет только `:restaurantId` из пути.
Facade-методы позиции дополнительно проверяют `item.RestaurantID == restaurantId`
(из пути) и возвращают `domain.ErrForbidden`/`ErrNotFound` при несовпадении — иначе
менеджер ресторана A мог бы через свой `:restaurantIdA` править `itemId` ресторана B.
Т.е. авторизация в двух слоях: роль+Manages (middleware), принадлежность (usecase).

**Закрытие Wave 1 follow-up.** Применяем тот же `RequireRestaurantManager` к
мутациям ресторанов (`app.go`): группа admin-only заменяется на per-restaurant для
роутов вида `/restaurants/:restaurantId/...` (PATCH/DELETE ресторана, managers). Роуты
без `:restaurantId` (создание нового ресторана `POST /restaurants`, справочники)
остаются `RequireRole(admin)`. `ManagerUseCase.Manages` наконец подключается.

**Тесты middleware:** admin проходит · менеджер своего ресторана проходит · менеджер
чужого → 403 · не-менеджер → 403 · без auth → 401. Плюс IDOR-тест в usecase/handler.

---

## 7. ETL (`cmd/etl`, подкоманда `menu`)

Идемпотентный upsert-by-id из `raw_supabase.*`. Порядок по FK:
`menu_categories` → `menu_items` (JOIN `restaurants` — сироты по `restaurant_id`
логируются и пропускаются) → `menu_item_tags` (JOIN `menu_items`).

Маппинг:
- UUID `id` — verbatim; i18n-колонки — `jsonb` verbatim.
- `price` — `numeric(12,2)` (из source number); в чистую схему как есть.
- `language`, `category`/`subcategory`(+i18n), `portion_size`(+i18n) — verbatim.
- `menu_item_tags.tag` — verbatim; дубли `(menu_item_id, tag)` схлопываются
  (`ON CONFLICT (menu_item_id, tag) DO NOTHING`, чтобы UNIQUE не ронял шаг — урок
  Wave 1 по managers).
- Ошибка, если 0 `menu_items` найдено.

---

## 8. Definition of Done (Wave 2)

- Миграция применяется и откатывается.
- ETL идемпотентен на реальном дампе; повторный прогон без дублей.
- Юнит-тесты (fakes) зелёные под `go test -short`; интеграционные — за `TEST_DATABASE_URL`.
- `GET /restaurants/:id/menu` отдаёт данные, паритетные `MenuSheet`/`fetchMenuByLanguage`
  (языковой фильтр, сортировка, теги).
- Per-restaurant авторизация работает (middleware + IDOR-тесты зелёные); мутации
  ресторанов Wave 1 переведены на per-restaurant.
- `gofmt -w .` и `go vet ./...` чистые.

> Решения по развилкам (подтверждены): `menu_categories` наружу — плоским списком
> (клиент строит parent→children); языковой фильтр по умолчанию `ru` (ru-или-null);
> теги — только replace-collection в составе позиции, без отдельных эндпоинтов.
