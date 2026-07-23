# Спека: Wave 3 — домен bookings

**Дата:** 2026-07-21
**Статус:** СОГЛАСОВАН владельцем 2026-07-21
**Ветка:** `feature_bookings` (от `develop`)
**Зависимости:** Wave 0 (users, auth), Wave 1 (restaurants + tables + time_slots), Wave 2 (menu)

---

## 1. Цель

Перенести из Supabase транзакционное ядро бронирования: сами брони, привязку к
столам, предзаказ позиций меню, переписку по броне, чёрный список, антифрод-лог и
пост-визитный опрос. Плюс переписать (не копировать) движок доступности — сейчас
он живёт в Supabase RPC и на клиенте.

Границы: уведомления (Telegram/WhatsApp/FCM/APNS/SMS) остаются на Deno edge —
backend-core только пишет события в outbox, доставку выполняет существующий слой.
Платежи и депозиты — отдельная волна (см. §9).

---

## 2. Таблицы в скоупе

| Supabase | Строк на 21.07.2026 | Комментарий |
|---|---|---|
| `bookings` | 620 | ядро |
| `booking_tables` | 9 | many-to-many бронь ↔ стол |
| `booking_items` | 243 | предзаказ из меню |
| `booking_messages` | 30 | чат гость ↔ заведение |
| `booking_blacklist` | 0 | стоп-лист гостей |
| `booking_rate_log` | 73 | антифрод: частота броней |
| `restaurant_surveys` | 2 | опрос после визита |

Итого 7 таблиц + новые (см. §4): `booking_status_history`, `booking_outbox`.

---

## 3. Что меняем относительно Supabase (осознанные отличия)

1. **`booking_status` перестаёт быть DB-энумом.** По конвенции репозитория —
   `varchar` + типизированные константы в Go. Значения сохраняем как есть:
   `pending`, `confirmed`, `waitlist`, `cancelled`, `completed`, `arrived`.
2. **`bookings.table_id` (легаси, одиночный стол) не переносим как рабочее поле.**
   Единственный источник правды — `booking_tables`. На ETL одиночный `table_id`
   разворачивается в строку `booking_tables`. Так объединение столов работает без
   спецслучаев.
3. **`booking_items.item_price` → `item_price_minor bigint` (тиыны).** `numeric`
   для денег в Go читается неудобно и провоцирует float. Валюта пока одна (KZT),
   поле `currency varchar(3) NOT NULL DEFAULT 'KZT'` заводим сразу.
4. **Добавляем интервал брони.** Сейчас у брони есть только `booking_date`
   (момент старта) — длительность нигде не хранится, поэтому «занятость стола»
   вычислить нечем. Вводим `starts_at timestamptz` (= бывший `booking_date`) и
   `ends_at timestamptz`, вычисляемый из политики заведения.
5. **Таймзона.** В таблице `restaurants` сейчас нет таймзоны. Добавляем
   `timezone varchar NOT NULL DEFAULT 'Asia/Almaty'` и правила брони (см. §4.2).
   Хранение — всегда UTC `timestamptz`, вся календарная логика — в таймзоне
   заведения.
6. **История статусов** выносится в отдельную таблицу вместо перезаписи поля.
7. **Дедупликация полей отмены:** остаются `cancellation_reason_code`,
   `cancellation_reason`, `cancelled_at`, `cancelled_by` (новое — кто отменил:
   гость, заведение, система).
8. **Поля доставки уведомлений** (`late_notification_sent`, `reminder_60_sent_at`,
   `reminder_30_sent_at`, `user_notified_late_at`, `user_late_message`)
   переносятся как есть — их пишет edge-слой, ломать его сейчас нельзя.

---

## 4. Схема (миграция `0004_bookings.sql`)

### 4.1 bookings

```
id                        uuid PK                      -- UUID из Supabase
restaurant_id             uuid NOT NULL REFERENCES restaurants(id)
user_id                   uuid REFERENCES users(id)    -- NULL = гостевая бронь
name, phone, email        varchar NOT NULL
phone_normalized          varchar NOT NULL             -- E.164, нормализуется в usecase
guests                    integer NOT NULL CHECK (guests > 0)
starts_at                 timestamptz NOT NULL
ends_at                   timestamptz NOT NULL
status                    varchar NOT NULL DEFAULT 'pending'
source                    varchar NOT NULL DEFAULT 'app'   -- app | admin | phone | widget
notes                     varchar
promotion_id              uuid
event_id                  uuid
created_by_admin          boolean NOT NULL DEFAULT false
confirmed_at              timestamptz
arrived_at                timestamptz
cancelled_at              timestamptz
cancelled_by              varchar          -- guest | restaurant | system
cancellation_reason_code  varchar
cancellation_reason       varchar
late_notification_sent    boolean NOT NULL DEFAULT false
user_notified_late_at     timestamptz
user_late_message         varchar
reminder_60_sent_at       timestamptz
reminder_30_sent_at       timestamptz
original_booking_time_text varchar
created_at, updated_at    timestamptz NOT NULL DEFAULT now()
CHECK (ends_at > starts_at)
```

Индексы:
- `(restaurant_id, starts_at)` — календарь заведения, основной запрос кабинета
- `(user_id, starts_at DESC)` — «мои брони»
- `(status, starts_at)` — фоновые задачи (напоминания, автоотмена)
- `(phone_normalized)` — проверка по чёрному списку и антифрод

**Нормализация контактов.** `phone` сохраняется как ввёл гость,
`phone_normalized` — E.164 через существующий `internal/auth/phone`. Email
приводится к нижнему регистру. Без этого чёрный список не сработает: гость
введёт «+7 (777) 123-45-67», а в стоп-листе лежит «+77771234567».

### 4.2 Политика брони: два уровня (env → заведение)

Согласовано с владельцем: глобальные значения живут в env и меняются без миграций,
у заведения — необязательное переопределение.

**Уровень 1 — env (`bootstrap.Config`), значения по умолчанию для всех:**

```
BOOKING_DEFAULT_DURATION_MINUTES=90
BOOKING_DEFAULT_BUFFER_MINUTES=0
BOOKING_DEFAULT_LEAD_MINUTES=60
BOOKING_DEFAULT_HORIZON_DAYS=60
BOOKING_DEFAULT_CANCEL_DEADLINE_MINUTES=180
BOOKING_DEFAULT_CONFIRM_SLA_MINUTES=120
BOOKING_DEFAULT_MAX_GUESTS=20
BOOKING_DEFAULT_AUTO_CONFIRM=true
BOOKING_TIMEZONE_FALLBACK=Asia/Almaty
```

**Уровень 2 — расширение `restaurants`, все поля NULLABLE (NULL = взять из env):**

```
timezone                  varchar                        -- NULL → BOOKING_TIMEZONE_FALLBACK
booking_duration_minutes  integer
booking_buffer_minutes    integer
booking_lead_minutes      integer
booking_horizon_days      integer
cancel_deadline_minutes   integer
confirm_sla_minutes       integer
max_guests_per_booking    integer
auto_confirm              boolean
```

Разрешение политики — одна функция в `usecase/bookings`
(`resolvePolicy(restaurant, cfg) domain.BookingPolicy`), покрытая юнит-тестами на
все комбинации NULL / переопределено.

**Автоподтверждение по умолчанию ВКЛЮЧЕНО** (`BOOKING_DEFAULT_AUTO_CONFIRM=true`).
Причина: 21 бронь из 89 висела в `pending` без ответа заведения. Заведение может
отключить у себя (`restaurants.auto_confirm = false`).

**Овербукинг запрещён жёстко** — пересекающиеся брони на один стол невозможны на
уровне БД. Исключение: менеджер заведения может разместить бронь принудительно
(`force=true` на manager-эндпоинте), это пишется в `bookings.forced_placement
boolean NOT NULL DEFAULT false` и в историю статусов. Гостю такой возможности нет.

**Уточнение (ревью 2026-07-21):** `force=true` ТРЕБУЕТ явного `table_ids`; пустой
список → `domain.ErrValidation` (422). Форсированная бронь без строк в
`booking_tables` не видна ни движку доступности, ни exclusion-констрейнту — тот
же стол в тот же слот спокойно продаётся обычному гостю, и конфликт нигде не
фиксируется. То есть «принудительное размещение» без столов ломало ровно ту
гарантию, ради которой существует констрейнт. `force` означает «посади их вот за
эти столы вопреки занятости», а не «посади в никуда». То же правило действует в
`PATCH /bookings/{id}`.

### 4.3 booking_tables — защита от двойной брони

```
id            uuid PK
booking_id    uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE
table_id      uuid NOT NULL REFERENCES restaurant_tables(id)
slot          tstzrange NOT NULL     -- ВКЛЮЧАЕТ буфер, см. ниже
active        boolean NOT NULL DEFAULT true
created_at    timestamptz NOT NULL DEFAULT now()

EXCLUDE USING gist (table_id WITH =, slot WITH &&) WHERE (active)
```

**Буфер входит в `slot`.** Иначе брони 10:00–12:00 и 12:00–14:00 не пересекутся
по `&&`, хотя столу нужна уборка между ними:

```
slot = tstzrange(
    starts_at - (buffer_minutes * interval '1 minute'),
    ends_at   + (buffer_minutes * interval '1 minute'),
    '[)'
)
```

`buffer_minutes` берётся из политики заведения на момент создания брони и
вычисляется **внутри той же транзакции**, что и вставка. Изменение политики
задним числом существующие брони не переписывает.

**Синхронизация `active`.** Полагаться на то, что usecase не забудет снять флаг,
нельзя. `active` поддерживает триггер `AFTER UPDATE OF status ON bookings`:
статус в (`pending`, `confirmed`, `arrived`) → `active = true`, иначе `false`.
Триггер — единственный писатель этого поля.

`active` — признак, что бронь в статусе, занимающем стол
(`pending`, `confirmed`, `arrived`). Отменённые и завершённые стол не держат.
Требуется расширение `btree_gist` (ставится в этой же миграции).

**Это ключевое архитектурное решение волны:** двойная бронь ловится ограничением
БД, а не проверкой в коде. Проверка в коде проигрывает гонке при одновременных
запросах; ограничение — нет.

### 4.4 Остальные таблицы

- `booking_items` — `booking_id`, `menu_item_id` (nullable, позиция может быть
  удалена), денормализованные `item_name`, `item_price_minor`, `currency`,
  `quantity CHECK (> 0)`, `status varchar`, `comment`. Индекс `(booking_id)`.
  Цена фиксируется на момент брони и не переписывается при изменении меню — при
  реализации платежей брать именно её.
- `booking_messages` — `sender_type varchar` (guest | restaurant | system),
  `sender_id`, `message`, `is_read`, `read_at timestamptz` (для метрики скорости
  ответа заведения). Индекс `(booking_id, created_at)`.
- `booking_blacklist` — `user_id` / `phone_normalized` / `email`, `reason`,
  `created_by`, `is_active`. Частичный уникальный индекс по
  `(restaurant_id, phone_normalized) WHERE is_active`.
  **Изменение:** добавляем `restaurant_id` (nullable) — стоп-лист должен быть
  и глобальным, и на уровне заведения. Сейчас он только глобальный.
- `booking_rate_log` — как есть, для антифрода.
- `restaurant_surveys` — как есть; рейтинги `CHECK (BETWEEN 1 AND 5)`, `nps
  CHECK (BETWEEN 0 AND 10)`, уникальность `(booking_id)`.
- `booking_status_history` — `booking_id`, `from_status`, `to_status`,
  `actor_type` (guest | manager | admin | system), `actor_id`, `reason`,
  `created_at`. Пишется в той же транзакции, что и смена статуса.
- `booking_outbox` — `id`, `booking_id`, `event_type`, `payload jsonb`,
  `created_at`, `published_at`. Записывается транзакционно вместе с изменением
  брони; читается воркером и отдаётся в edge-слой уведомлений.

---

## 5. Машина состояний

```
pending ──confirm──▶ confirmed ──arrive──▶ arrived ──complete──▶ completed
   │                     │                    │
   └──cancel──▶ cancelled ◀──cancel───────────┘
pending ──waitlist──▶ waitlist ──confirm──▶ confirmed
```

Правила:
- Переходы описаны таблицей допустимых переходов в `domain`; недопустимый переход
  → `domain.ErrInvalidStatus` → HTTP 422.
- `completed` выставляет фоновая задача через `ends_at + grace`, если статус был
  `arrived`. Если гость не отмечен пришедшим — статус `no_show` (**новое
  значение**, в Supabase его нет; нужно для платной брони и статистики).
- **Уточнение (ревью 2026-07-21):** `no_show` достижим ТОЛЬКО из `confirmed`.
  Неявка — это нарушенное обещание, которое заведение приняло; бронь, которую
  заведение так и не подтвердило, нарушить нечего. Переход `pending → no_show`
  убран: он позволял менеджеру пометить неявку по заявке, на которую сам же не
  ответил, и испортить гостю историю. Следствие: `pending`/`waitlist`, у которых
  `ends_at + grace` уже прошёл, закрывает воркер — в `cancelled` с
  `actor_type = system`, `cancelled_by = system` и причиной «venue never
  responded». Без этой стадии такая бронь была бы бессмертной.
- Отмена гостем разрешена до `starts_at - cancel_deadline`; после — только
  заведением. Дедлайн берётся из политики заведения.
- Каждый переход выполняет три операции — `UPDATE bookings`, `INSERT
  booking_status_history`, `INSERT booking_outbox` — строго внутри одного
  `domain.TxManager.WithinTx(...)`. Отдельный интеграционный тест проверяет, что
  при ошибке в середине откатываются все три.
- **Автоподтверждение и SLA.** Фоновый воркер (`cmd/worker`, тик раз в минуту)
  забирает брони со `status = 'pending' AND created_at < now() - confirm_sla`:
  если у заведения `auto_confirm = true` — переводит в `confirmed`, иначе шлёт
  эскалацию в outbox. Тот же воркер закрывает `completed`/`no_show` после
  `ends_at + grace` и отменяет «брошенные» `pending`/`waitlist` (стадия идёт
  первой, чтобы просроченная заявка не была сначала автоподтверждена, а потом
  записана гостю в неявку). Воркер идемпотентен и безопасен при параллельном
  запуске (`SELECT ... FOR UPDATE SKIP LOCKED`).
- **Уточнение (ревью 2026-07-21):** `ClaimDue` сортирует по той же колонке, по
  которой делает отсечку (`created_at` для confirm-SLA, `ends_at` для закрытия).
  Раньше отсечка шла по `created_at`, а `ORDER BY` — по `starts_at`: при батче
  меньше числа кандидатов реально просроченная бронь с далёким `starts_at`
  вытеснялась свежими и не обрабатывалась никогда. Колонка выбирается из
  закрытого набора `domain.ClaimColumn`, в SQL не подставляется ничего
  произвольного; индексы — `0006_bookings_claim_indexes.sql`.

---

## 6. Движок доступности

`GET /api/restaurants/{id}/availability?date=YYYY-MM-DD&guests=N`

Алгоритм (весь в usecase, не в SQL-RPC):
1. Календарный день разворачивается в таймзоне заведения.
2. Берутся `restaurant_working_hours` и `restaurant_time_slots` для дня недели,
   исключаются `is_manually_disabled`.
3. Из `restaurant_tables` (только `is_active`) берутся столы с
   `capacity >= guests`; при `guests > max(capacity)` — рассматриваются комбинации
   столов (жадный подбор, не более 3 столов).
4. Вычитаются занятые интервалы из `booking_tables` (активные статусы) с учётом
   `booking_buffer_minutes`.
5. Отсекаются слоты ближе `booking_lead_minutes` и дальше `booking_horizon_days`.
6. Ответ: список слотов с флагом доступности и количеством свободных столов.

Запрос дня в таймзоне заведения — по полуоткрытому интервалу, а не по приведению
типа (иначе индекс не используется):

```sql
WHERE restaurant_id = $1
  AND starts_at >= $2   -- начало дня в TZ заведения, приведённое к UTC
  AND starts_at <  $3   -- начало следующего дня
```

Границы суток вычисляются в Go через `time.LoadLocation(restaurant.timezone)`.
Отдельный тест — на переход летнего времени (Almaty его не имеет, но заведения
в других TZ появятся).

Эндпоинт публичный (без авторизации) — витрине он нужен до логина.

---

## 7. Эндпоинты и доступ

Публичные:
- `GET /api/restaurants/{id}/availability`

Гость (авторизован):
- `POST /api/bookings` — создание. Обязателен заголовок `Idempotency-Key`.
- `GET /api/bookings` — свои брони, пагинация, фильтр по статусу
- `GET /api/bookings/{id}` — своя бронь
- `POST /api/bookings/{id}/cancel`
- `GET|POST /api/bookings/{id}/messages`
- `POST /api/bookings/{id}/survey`

Менеджер заведения (`RequireRestaurantManager`):
- `GET /api/restaurants/{id}/bookings` — календарь, фильтры по дате и статусу
- `POST /api/restaurants/{id}/bookings` — ручная бронь (`source=admin`)
- `POST /api/bookings/{id}/confirm|reject|arrive|complete|no-show`
- `PATCH /api/bookings/{id}` — перенос времени, смена столов, число гостей
- `GET|POST /api/restaurants/{id}/blacklist`

Админ:
- всё вышеперечисленное без привязки к конкретному заведению

**Требование к тестам:** на каждый manager-эндпоинт — тест «менеджер чужого
заведения получает 403». В Supabase это делала RLS; теперь это код, и он должен
быть покрыт.

---

## 8. ETL

Подкоманда `go run ./cmd/etl/main.go bookings`, читает `raw_supabase.*`,
upsert-by-id, идемпотентна, безопасна к повторному запуску.

Особые случаи:
- `booking_date` → `starts_at`; `ends_at` = `starts_at + booking_duration_minutes`
  заведения (для исторических броней длительность неизвестна — берём политику).
- `bookings.table_id` → строка в `booking_tables` (если стол ещё существует).
- `item_price numeric` → `item_price_minor bigint` (× 100, округление банковское).
- Статус `completed`/`cancelled`/… переносится как есть; `no_show` в истории не
  проставляется задним числом — нет данных, домысливать не будем.
- Брони с несуществующим `restaurant_id`/`table_id` — пропускаются с логом, а не
  падают.
- `promotion_id` и `event_id` переносятся без FK: промо или событие могут быть
  удалены, бронь обязана пережить это как исторический факт. Явно оговорено
  комментарием в миграции.
- Исторические брони получают `ends_at = starts_at + текущая политика
  заведения` — реальная длительность визита в Supabase не хранилась. Для уже
  завершённых броней этого достаточно; на статистику по будущим броням не влияет.

---

## 9. Что осознанно НЕ входит в эту волну

- Платежи, депозиты, невозвратная бронь, возвраты. Это отдельная волна
  `payments` — схема брони к ней готова (`booking_id` + отдельная таблица
  платежей), но код не пишем.
- Реальная отправка уведомлений — только запись в `booking_outbox`.
- Лояльность (начисление баллов за визит).
- Перенос фронта на новый API.

---

## 10. Definition of Done

1. `0004_bookings.sql` применяется и откатывается (`goose up` / `down`).
2. `go build`, `go vet`, `gofmt` — чисто.
3. `go test -short ./...` — зелёные юнит-тесты на фейках.
4. Тест на буфер: брони 10:00–12:00 и 12:00–14:00 при `buffer = 15` на одном
   столе — вторая отклоняется.
5. Интеграционные тесты на реальном Postgres (`TEST_DATABASE_URL`), включая
   тест на гонку: два одновременных `POST /api/bookings` на один стол и слот —
   ровно один успех, второй получает 409.
6. Тесты авторизации: чужой менеджер → 403, чужой гость → 404/403. Доступ к
   данным заведения проверяется явно в начале каждого usecase — неявного allow
   быть не должно.
7. ETL прогнан на дампе дважды подряд — результат идентичен.
8. Ревью: code-reviewer + Codex, замечания закрыты.
9. PR `feature_bookings` → `develop`.

---

## 11. Решения владельца

Все закрыты владельцем 2026-07-21:

1. Длительность брони — 2 часа, управление через env + переопределение на заведении. ✅
2. Автоподтверждение — включено по умолчанию. ✅
3. Дедлайн отмены гостем — 3 часа, так же через env. ✅
4. Овербукинг — жёсткий запрет; ручное принудительное размещение доступно только
   менеджеру заведения с флагом `force`. ✅
