# Роадмап: полный переезд backend'а из book-eat-app в backend-core

**Дата:** 2026-07-13
**Статус:** согласован (дизайн), детальные спеки доменов — отдельно
**Область:** транзакционное ядро BookEat. Auth и users уже перенесены.

---

## 1. Цель и принципы

Перенести серверную логику BookEat из Supabase (Postgres + RLS + Deno edge
functions), к которому фронт `book-eat-app` сейчас обращается напрямую через
Supabase JS client, в самостоятельный Go-сервис `backend-core` (Clean/Hexagonal).

Принципы:

- **Данные сохраняем, реализацию — переписываем.** Схема в backend-core
  проектируется заново (чистая, `VARCHAR` вместо DB-энумов, без RLS —
  авторизация в коде). Данные переносятся идемпотентным ETL из дампа Supabase.
- **UUID сохраняются.** Первичные ключи (`id`) копируются из Supabase как есть,
  чтобы связи между доменами и внешние ссылки не ломались.
- **Один домен — одна волна — одна спека.** Каждый домен проходит полный цикл
  spec → plan → реализация независимо. Волна зависит только от уже перенесённых
  волн, поэтому её ETL и эндпоинты тестируются в изоляции.
- **Big-bang cutover.** Supabase остаётся системой записи (system of record) на
  всём протяжении переезда; фронт продолжает писать в Supabase. backend-core
  строится до полного паритета параллельно и включается одним переключением в
  самом конце.

---

## 2. Область: что переносим, что нет

### В скоупе (транзакционное ядро)

| Волна | Домен | Основные таблицы |
|---|---|---|
| 0 (готово) | **auth** | user_credentials, otp_codes, refresh_tokens, JWKS |
| 0 (готово) | **users** | users (ex-`profiles` + `auth.users`) |
| 1 | **restaurants** | restaurants (+ categories, features, images, tags, social_links, working_hours, floor_plans), restaurant_tables, restaurant_time_slots, restaurant_managers |
| 2 | **menu** | menu_categories, menu_items, menu_item_tags |
| 3 | **bookings** | bookings, booking_tables, booking_items, booking_messages, booking_blacklist, restaurant_surveys, availability-RPC |
| 4 | **orders** | restaurant_orders (+ line items через booking_items/меню) |
| 5a | **reviews** | reviews, review_responses |
| 5b | **offers** | partner_offers, offer_redemptions, promotions, user_promo_codes |
| 5c | **social** | favorites, saved_items, user_friends, friend_messages, split_bills, split_bill_participants |

### Вне скоупа (пока остаётся в Supabase / edge / отдельном сервисе)

- **Loyalty** — user_loyalty, loyalty_transactions/rewards/challenges/achievements/redemptions,
  user_bonuses, **referral_codes/uses, gift_cards**, tier-RPC. Отложено.
- **Notifications / messaging** — ~22 Deno edge-функций: FCM, APNS, Telegram,
  Twilio/WhatsApp, push_campaigns, device_tokens, OTP-доставка. Остаётся на edge
  (Deno-рантайм, внешние интеграции).
- **Analytics / ML** — churn_predictions, cohort_analysis, revenue_cohorts,
  user_engagement_metrics, acquisition_channels, user_activities,
  booking_funnel_events. BI/оффлайн, не транзакционное ядро.
- **Gastroguide / Content** — articles, gastroguide_* (routes/points/categories/
  events), stories, highlights. Контентное, редко меняется.
- **Kwaaka POS** — сторонняя интеграция заказов/POS (`src/integrations/kwaaka`).

> Примечание: `restaurants.kwaaka_restaurant_id` переносится как обычная
> nullable-колонка (внешний идентификатор), сама интеграция — вне скоупа.

---

## 3. Граф зависимостей и порядок волн

Зависимость направлена от домена к тому, что должно уже существовать. Всё
зависит от `users` (готово).

```
Wave 0 (DONE):  auth · users
Wave 1:         restaurants        ← users
Wave 2:         menu               ← restaurants
Wave 3:         bookings           ← restaurants, menu (booking_items → menu_items)
Wave 4:         orders             ← restaurants, menu, users
──────── после Wave 4 домены взаимно независимы, можно параллелить ────────
Wave 5a:        reviews            ← restaurants, bookings
Wave 5b:        offers             ← restaurants, users
Wave 5c:        social             ← restaurants, bookings (split_bills)
```

- **Волны 1–4 — строгая цепочка** (транзакционный «хребет»): меню принадлежит
  ресторану, бронь ссылается на ресторан/стол/слот и на позиции меню, заказ —
  на ресторан и меню.
- **Волны 5a–5c — параллельны** между собой: каждая зависит только от 1–4, но не
  друг от друга. Их можно вести одновременно (или строго линейно, если удобнее
  вести ровно один домен за раз — на выбор исполнителя).

### Пограничные таблицы

- `restaurant_surveys` — ссылается на `booking_id`, поэтому едет в **Wave 3
  (bookings)**, а не в restaurants.
- `restaurant_partnership_requests` — самостоятельная лид-форма без FK (заявка на
  партнёрство). Маленький публичный эндпоинт; переносится как отдельный мелкий
  ресурс в Wave 1 либо откладывается — решается в спеке restaurants.
- `dinner_invitations`, `event_registrations` — низкий трафик; при переносе
  привязываются к ближайшей волне (bookings/social), иначе явно помечаются как
  «defer», а не додумываются.

---

## 4. Рецепт миграции одного домена (повторяемый чек-лист)

Каждая волна повторяет паттерн, уже отработанный на users. Спека домена детализирует
шаги, но структура всегда одна:

1. **Чистая схема.** Одна goose-миграция на домен: `VARCHAR` для перечислимых
   полей (валидация в коде, никаких `CREATE TYPE ... AS ENUM`), FK на уже
   перенесённые таблицы, PK = оригинальный Supabase UUID. i18n-поля (`*_i18n`)
   переносятся как `jsonb`.
2. **Domain-слой.** Один файл на сущность в `internal/domain/`: структура,
   repository-интерфейс, типизированные константы статусов/ролей.
3. **Usecase.** Экспортируемый `Facade` (базовый CRUD/чтение) + отдельные файлы с
   focused-интерфейсами для сложных операций; зависимости — позиционными
   аргументами в `NewFacade(...)`; минимальные локальные порты в `ports.go`
   (домен не импортирует чужой конкретный репозиторий).
4. **Transport.** Gin-хендлеры, `request.go` (DTO + `Validate()` + `ToDomain()`),
   `response.go` (DTO + `fromDomain()`). Все ответы — через `response.Envelope`,
   все ошибки — через `response.HandleError`.
5. **Идемпотентный ETL.** Расширение `cmd/etl` (подкоманда на домен), читающее из
   staging-схемы `raw_supabase.*`, upsert-by-id. Т.к. фронт пишет в Supabase до
   самого cutover, **каждый ETL проектируется под повторный запуск** — финальный
   прогон на cutover добирает дельту.
6. **Тесты.** Hand-written fakes (юнит, проходят под `go test -short`) +
   Postgres-интеграция за `TEST_DATABASE_URL` (`infrastructure/postgres/testdb`).

Definition of Done волны: миграция применяется и откатывается; ETL идемпотентен на
реальном дампе; юнит + интеграционные тесты зелёные; эндпоинты отдают паритетные
данные фронтовым хукам; `gofmt` + `go vet` чистые.

---

## 5. Стратегия данных и cutover (big-bang)

- **System of record до конца — Supabase.** Фронт `book-eat-app` продолжает
  ходить в Supabase JS client всё время переезда. backend-core пишется до полного
  паритета в параллель и в проде не обслуживает трафик до финала.
- **Staging-дамп.** Дамп Supabase грузится в схему `raw_supabase` (как уже
  сделано для users: `auth.users` → `raw_supabase.users`, `public.profiles` →
  `raw_supabase.profiles`). ETL-и доменов читают оттуда.
- **Повторяемость.** Каждый доменный ETL — upsert-by-id, безопасен к повторному
  прогону. По ходу разработки гоняем на снапшотах; на cutover — один финальный
  прогон всех ETL против свежего дампа, чтобы забрать всю накопленную дельту.
- **Переключение.** После финального ETL фронт перенаправляется с Supabase JS
  client на REST API backend-core. Это отдельная работа на стороне
  `book-eat-app` (замена слоя доступа к данным в хуках `src/hooks/*`), не входит в
  доменные спеки backend-core, но упоминается как условие завершения.
- **i18n.** Локализуемые поля остаются `jsonb` формы `{ ru, kk, en, ... }`.
  Логика фолбэка (`localize()`) может остаться на клиенте (читает ту же форму)
  либо переехать на сервер — решается при cutover, не блокирует доменные волны.
- **Авторизация.** RLS-политики Supabase заменяются на `middleware.Auth` +
  `middleware.RequireRole` в backend-core. Каждая доменная спека фиксирует, какие
  эндпоинты публичны, какие user-gated, какие manager/admin-gated.

---

## 6. Что дальше

1. **Wave 1 — restaurants:** детальная спека в
   `2026-07-13-restaurants-domain-design.md` (этот же каталог). Готова к переходу
   в implementation plan.
2. Последующие волны получают свою спеку по мере готовности предыдущей.
