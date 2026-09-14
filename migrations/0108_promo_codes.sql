-- +goose Up

-- ПРОМОКОДЫ ДЛЯ СУЩЕСТВУЮЩИХ ГОСТЕЙ (spec promo-codes-existing-guests-20260912,
-- план promo-codes-implementation-plan-20260912, задача B1; решение ADR-047).
--
-- ЗАЧЕМ. Гость вводит код руками на шаге подтверждения брони; код — это ВТОРОЙ
-- способ проставить броне уже существующий bookings.promotion_id, а не второе
-- поле того же назначения. Скидки у кода нет вообще (владелец, 12.09.2026):
-- сегодня это метка участия в кампании, по которой потом выдают мерч.
--
-- ПОЧЕМУ ОТДЕЛЬНАЯ ТАБЛИЦА, А НЕ КОЛОНКА В promos. У кода и у акции разные
-- жизненные циклы: карточка акции может висеть на витрине, пока приём кодов
-- выключен (status='paused'), и наоборот. Плюс у кода своё окно действия и свои
-- лимиты, которых у акции нет. Слить их в одну строку — значит навсегда связать
-- «видно гостю» и «принимаем код».
--
-- ПОЧЕМУ ЗДЕСЬ НЕТ КОЛОНКИ-СЧЁТЧИКА АКТИВАЦИЙ (ADR-047). Единственный источник
-- правды о расходе лимита — сами брони:
--     select count(distinct user_id) from bookings
--      where promo_code_id = $1 and status not in ('cancelled','no_show');
-- Так «отмена и no-show освобождают место» получается САМО, без пути
-- декремента, который живёт в четырёх местах (отмена гостем, отмена персоналом,
-- воркер processExpired → no_show, внешние резервации) и рано или поздно молча
-- разойдётся с реальностью. Расход защищён `SELECT ... FROM promo_codes WHERE
-- id = $1 FOR UPDATE` внутри той же транзакции, что вставка брони; порядок
-- блокировок promo_code → venue односторонний и обязательный (иначе дедлок).
-- Обратный ход дешёвый: счётчик добавляется одной миграцией, подсчёт остаётся
-- сверкой.
--
-- ЭТА МИГРАЦИЯ НЕ СЕЕТ ДАННЫХ — марафонский код MARATHON26 заводит суперадмин
-- через экран кабинета (осознанное решение tech-lead, план §1.1): значение
-- max_uses_total почти наверняка поменяется до кампании, а данные в миграции
-- правятся только второй миграцией или руками на бою.
--
-- НОМЕР. На 2026-09-12 максимум по ВСЕМ веткам origin/* — 0107
-- (`0107_almaty_marathon_promo.sql`, есть на develop/main); проверено
-- `git ls-tree -r --name-only <branch> -- migrations/` по каждой удалённой
-- ветке. 0108 свободен.

SET lock_timeout = '3s';

CREATE TABLE promo_codes
(
    id                uuid PRIMARY KEY,

    -- Нормализованный код: верхний регистр, без пробелов и дефисов
    -- (domain.NormalizePromoCode). UNIQUE — «код есть» это одна строка, а не
    -- поиск по вариантам написания; регистр и разделители снимаются ДО записи,
    -- поэтому обычного уникального индекса достаточно и citext не нужен.
    -- CHECK повторяет domain.promoCodeRe: это страховка от строки, вставленной
    -- мимо приложения (руками в psql, будущим импортом), а не замена валидации
    -- в Go — поле едет в path-параметре предпроверки GET /promo-codes/:code.
    code              varchar(32) NOT NULL UNIQUE
        CONSTRAINT promo_codes_code_normalized CHECK (code ~ '^[A-Z0-9]{3,32}$'),

    -- Что код значит. Именно это значение уезжает в bookings.promotion_id.
    -- ON DELETE RESTRICT, а не CASCADE и не SET NULL: код без акции
    -- бессмыслен (названия, условий и обложки он не дублирует — гость видит
    -- title/terms акции), поэтому удаление акции, на которую ссылается живой
    -- код, должно ОТКАЗАТЬ, а не тихо осиротить или снести код. Отказ Postgres
    -- маппится в понятную ошибку в postgres/promo (задача B5).
    promotion_id      uuid        NOT NULL REFERENCES promos (id) ON DELETE RESTRICT,

    -- Собственное окно приёма кода, НЕ окно акции. Акция может идти дольше, чем
    -- принимается код, и наоборот; обе проверки делаются при активации
    -- (живость акции — существующий validatePromotion).
    starts_at         timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,

    -- NULL = без общего лимита. Считается РАЗНЫМИ гостями
    -- (count(distinct user_id) по броням), а не бронями: «сколько человек
    -- участвуют в кампании». 0 запрещён — «код, которым нельзя
    -- воспользоваться» выражается статусом paused, а не лимитом в ноль.
    max_uses_total    int
        CONSTRAINT promo_codes_max_uses_total_positive CHECK (max_uses_total >= 1),
    -- Сколько раз ОДИН гость может применить код. По умолчанию 1.
    max_uses_per_user int         NOT NULL DEFAULT 1
        CONSTRAINT promo_codes_max_uses_per_user_positive CHECK (max_uses_per_user >= 1),

    -- draft → active ⇄ paused → archived (archived терминальный). VARCHAR +
    -- CHECK, а не CREATE TYPE ... AS ENUM (правило схемы): переходы
    -- валидируются в Go, domain.PromoCodeStatus.Valid/CanTransitionTo, как у
    -- promos.status (0032).
    status            varchar(16) NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'paused', 'archived')),

    -- Кто завёл код. НАМЕРЕННО БЕЗ FK на users — та же причина, что у
    -- platform_pages.updated_by (0105) и payment_providers: десятки чужих
    -- интеграционных пакетов делают `testdb.Truncate(t, pool, ..., "users")`,
    -- а `TRUNCATE ... CASCADE` чистит ЛЮБУЮ таблицу с FK на усекаемую и НЕ
    -- смотрит на ON DELETE SET NULL (conventions/bookeat-backend.md,
    -- «TEST-DB POLLUTION TRAP»). Ценность здесь — атрибуция, а не ссылочная
    -- целостность.
    --
    -- ЧЕСТНАЯ ОГОВОРКА: отсутствие ЭТОГО FK не делает таблицу неуязвимой.
    -- promo_codes обязана ссылаться на promos, а promos.feed_reviewed_by уже
    -- ссылается на users, и TRUNCATE ... CASCADE идёт по цепочке транзитивно —
    -- `testdb.Truncate(..., "users")` уносит promos и вместе с ними коды.
    -- Проверено тестом TestCreatedByCarriesNoForeignKey. Практический вывод:
    -- promo_codes НЕ справочник, засеянных строк, которые должны пережить весь
    -- прогон, здесь быть не должно — каждый тест заводит свои коды.
    created_by        uuid,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT promo_codes_window CHECK (expires_at > starts_at)
);

COMMENT ON TABLE promo_codes IS
    'Промокоды-метки участия: код → существующая акция promos, со своим окном, '
        'лимитами и статусом. Скидки у кода нет. Счётчика активаций нет '
        'намеренно — расход считается по броням под FOR UPDATE (ADR-047).';
COMMENT ON COLUMN promo_codes.max_uses_total IS
    'NULL = без лимита. Считается РАЗНЫМИ гостями: count(distinct user_id) по '
        'броням с этим promo_code_id и статусом не cancelled/no_show.';
COMMENT ON COLUMN promo_codes.status IS
    'draft / active / paused / archived. Активация возможна ТОЛЬКО в active и '
        'внутри [starts_at, expires_at), при живой акции.';

-- Листинг кодов акции и проверка «на эту акцию уже есть коды» перед её
-- удалением. Без индекса FK RESTRICT заставляет Postgres сканировать таблицу
-- при каждом удалении акции.
CREATE INDEX idx_promo_codes_promotion ON promo_codes (promotion_id);

-- Поиск активных кодов в админском листинге, отсортированном по свежести.
CREATE INDEX idx_promo_codes_status_created ON promo_codes (status, created_at DESC);

-- БРОНИ: код, по которому бронь создана.
--
-- promo_code_id БЕЗ FK — ровно как promotion_id и event_id (0004, комментарий
-- там же и в domain/booking.go): бронь обязана пережить архивацию и удаление
-- кода как исторический факт. promo_code — строковый снимок на момент брони,
-- чтобы админка и выгрузка показывали код без join'а и после удаления строки
-- кода; тип и CHECK НЕ навешиваются жёстче, чем varchar(32), потому что это
-- снимок прошлого, а не действующее значение (правило нормализации может
-- поменяться, старые брони переписывать нельзя).
--
-- Оба поля пишутся ТОЛЬКО в INSERT брони: «код на момент брони» неизменяем по
-- определению, и в UPDATE репозитория броней они не перечисляются (ADR-047).
ALTER TABLE bookings
    ADD COLUMN promo_code_id uuid,
    ADD COLUMN promo_code    varchar(32);

COMMENT ON COLUMN bookings.promo_code_id IS
    'Код, по которому создана бронь. Без FK намеренно (как promotion_id): бронь '
        'переживает удаление кода. Пишется только при создании.';
COMMENT ON COLUMN bookings.promo_code IS
    'Строка кода на момент брони — снимок, чтобы показать код без join''а и '
        'после удаления строки promo_codes.';

-- Обязательное условие ADR-047: оба подсчёта расхода лимита
-- (count(distinct user_id) и count(*) по одному гостю) идут по promo_code_id
-- внутри транзакции создания брони. Без этого индекса каждая бронь с кодом
-- сканирует растущую таблицу броней. Частичный — потому что подавляющее
-- большинство броней кода не несёт.
CREATE INDEX idx_bookings_promo_code ON bookings (promo_code_id)
    WHERE promo_code_id IS NOT NULL;

-- +goose Down

-- Симметричный откат. Колонки броней дропаются вместе с индексом (DROP COLUMN
-- уносит зависимые индексы сам, явный DROP INDEX оставлен для читаемости и на
-- случай частично применённого наката). ОТКАТ ТЕРЯЕТ привязку броней к кодам —
-- это осознанно и симметрично 0105: висящие колонки, которые откатившийся код
-- больше не пишет, а следующий накат прочитал бы как «эти брони участвуют»,
-- хуже честной потери. promotion_id у таких броней остаётся на месте, то есть
-- участие в кампании не теряется — теряется только «пришёл по коду».
SET lock_timeout = '3s';

DROP INDEX IF EXISTS idx_bookings_promo_code;

ALTER TABLE bookings
    DROP COLUMN IF EXISTS promo_code,
    DROP COLUMN IF EXISTS promo_code_id;

DROP TABLE IF EXISTS promo_codes;
