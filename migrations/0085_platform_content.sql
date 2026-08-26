-- +goose Up

-- Акции и афиши БЕЗ заведения — «витрина платформы».
--
-- ЧТО БЫЛО. И `events` (0031), и `promos` (0032) держат `restaurant_id` как
-- NOT NULL: контент на платформе мог существовать только от имени конкретного
-- заведения. Город акции при этом не хранился нигде — он выводился из
-- заведения через JOIN, а значит для акции без заведения города не было бы
-- вовсе. Событиям свой город уже завели в 0084, ровно с этим прицелом.
--
-- ЧТО ДЕЛАЕТ ЭТА МИГРАЦИЯ — четыре независимых вещи:
--   1. снимает NOT NULL с `events.restaurant_id` и `promos.restaurant_id`;
--   2. даёт `promos` тот же НЕОБЯЗАТЕЛЬНЫЙ город-переопределение, что 0084 дал
--      событиям (колонки + справочник + триггер), а не вторую его форму;
--   3. запрещает состояния, которые «витрина» сделала бы выразимыми, а
--      остальная система не выдержала бы: платное событие без заведения;
--   4. добавляет событию КНОПКУ действия: подпись + необязательная внешняя
--      ссылка.
--
-- НОМЕР. Прод стоит на 83, 0084 занят веткой feat/events-city (событию — свой
-- город), поверх которой сделана эта работа. Свободный следующий — 0085.
--
-- ЧЕГО ЗДЕСЬ НЕТ. Переноса данных: все существующие строки сохраняют своё
-- `restaurant_id` и получают `city IS NULL`, что для строки с заведением
-- означает ровно то же, чем город был раньше, — город заведения, выведенный на
-- чтении. Прогон Up на живой таблице не переписывает ни одной строки.

SET lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. Заведение становится необязательным.
--
-- DROP NOT NULL — это правка каталога, без скана и без переписывания строк, под
-- ACCESS EXCLUSIVE на доли миллисекунды. Внешний ключ на `restaurants`
-- остаётся: NULL его не нарушает (простой FK не проверяется для NULL), а
-- «указали несуществующее заведение» по-прежнему падает как раньше.
--
-- Все существующие частичные индексы и уникальные ограничения продолжают
-- работать: ни одно из них не строится по `restaurant_id` в одиночку так, чтобы
-- NULL менял его смысл, а те, что включают его в составной ключ, для строк
-- платформы просто не находят пары (в Postgres NULL не равен NULL).
-- ---------------------------------------------------------------------------

ALTER TABLE events ALTER COLUMN restaurant_id DROP NOT NULL;
ALTER TABLE promos ALTER COLUMN restaurant_id DROP NOT NULL;

COMMENT ON COLUMN events.restaurant_id IS
    'Заведение-хозяин. NULL = событие платформы: заведения нет, редактировать '
        'может только суперадмин, карточка рисуется без строки заведения.';
COMMENT ON COLUMN promos.restaurant_id IS
    'Заведение. NULL = акция платформы (см. events.restaurant_id).';

-- ---------------------------------------------------------------------------
-- 2. У акции появляется свой город — та же форма, что у события в 0084.
--
-- Семантика колонки повторяется дословно, и это главное здесь: две сущности,
-- которые лежат рядом в одной ленте и фильтруются одним и тем же `?city=`, не
-- должны понимать город по-разному.
--
--   city IS NULL + есть заведение → город берётся у заведения на ЧТЕНИИ
--                                   (COALESCE(p.city, r.city)) — сегодняшнее
--                                   поведение, буква в букву;
--   city IS NULL + заведения нет  → показывать во ВСЕХ городах;
--   city = 'Алматы'               → принудительно этот город.
--
-- Почему вывод на чтении, а не копия города заведения в строку акции — тот же
-- разбор, что в 0084: копия протухает при переезде заведения, а чтобы она не
-- протухала, пришлось бы каскадом переписывать все его акции.
--
-- Про блокировки — тоже как в 0084: nullable-колонки без DEFAULT строки не
-- переписывают; FK добавляется NOT VALID и валидируется отдельным statement'ом,
-- чтобы проверочный скан шёл не под ACCESS EXCLUSIVE; индекс частичный.
-- ---------------------------------------------------------------------------

ALTER TABLE promos
    ADD COLUMN city    varchar,
    ADD COLUMN city_id uuid;

ALTER TABLE promos
    ADD CONSTRAINT promos_city_id_fkey
        FOREIGN KEY (city_id) REFERENCES cities (id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE promos
    VALIDATE CONSTRAINT promos_city_id_fkey;

COMMENT ON COLUMN promos.city IS
    'Переопределение города. NULL = город берётся у заведения '
        '(COALESCE(p.city, r.city)), а у акции без заведения NULL означает '
        '«во всех городах».';
COMMENT ON COLUMN promos.city_id IS
    'Ссылка на справочник cities для переопределённого города. NULL при '
        'city IS NULL, а также когда написание не нашлось в city_aliases.';

CREATE INDEX idx_promos_city_id ON promos (city_id) WHERE city_id IS NOT NULL;

-- Триггер — точное зеркало events_sync_city() из 0084. Это КОПИЯ, а не общая
-- функция, и осознанно: общую пришлось бы либо объявить здесь и переподключить
-- к ней триггер `events`, переписав объект чужой, ещё не влитой миграции, либо
-- держать в 0084 и делать 0085 незапускаемой без неё. Двадцать строк дублируют
-- дешевле, чем связывать две миграции в одну неразделимую пару.
--
-- Правила: пустая строка → обе колонки NULL; сменили city_id → строка идёт за
-- ссылкой; иначе строка резолвится по city_aliases к каноническому написанию.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION promos_sync_city() RETURNS trigger AS
$$
DECLARE
    resolved uuid;
BEGIN
    IF NEW.city IS NULL OR btrim(NEW.city) = '' THEN
        NEW.city := NULL;
        NEW.city_id := NULL;
        RETURN NEW;
    END IF;

    IF TG_OP = 'UPDATE' AND NEW.city_id IS DISTINCT FROM OLD.city_id AND NEW.city_id IS NOT NULL THEN
        SELECT c.name INTO NEW.city FROM cities c WHERE c.id = NEW.city_id;
        RETURN NEW;
    END IF;

    SELECT a.city_id INTO resolved
    FROM city_aliases a
    WHERE a.alias = city_key(NEW.city);
    NEW.city_id := resolved;
    IF resolved IS NOT NULL THEN
        SELECT c.name INTO NEW.city FROM cities c WHERE c.id = resolved;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_promos_sync_city
    BEFORE INSERT OR UPDATE
    ON promos
    FOR EACH ROW
EXECUTE FUNCTION promos_sync_city();

-- ---------------------------------------------------------------------------
-- 3. Чего «витрина» НЕ имеет права выразить.
--
-- Событие платформы не может быть платным. Билет — это платёж, у платежа есть
-- получатель, а получатель в этой схеме — заведение: `payments.restaurant_id`
-- NOT NULL, сплиты и выплаты (0077) считаются на счёт заведения, возврат
-- оформляет персонал заведения. Продать билет от имени платформы сегодня
-- физически некуда, и лучше запретить это ограничением, чем узнать о дыре из
-- NULL-а в платёжной таблице.
--
-- NOT VALID здесь НЕ нужен: до этой миграции строк с restaurant_id IS NULL не
-- существует в принципе, поэтому скан заведомо пустой по условию.
-- ---------------------------------------------------------------------------

ALTER TABLE events
    ADD CONSTRAINT events_platform_not_ticketed
        CHECK (restaurant_id IS NOT NULL OR NOT ticketed);

-- ---------------------------------------------------------------------------
-- 4. Кнопка действия на карточке события.
--
--   action_label — подпись («Купить билет», «Зарегистрироваться»);
--   action_url   — ВНЕШНЯЯ ссылка. NULL при заданной подписи означает «кнопка
--                  ведёт на страницу самого события», то есть цель выводится
--                  из наличия ссылки и НЕ хранится третьей колонкой: два
--                  представления одного факта рано или поздно разъезжаются.
--
-- Ограничения:
--   * ссылка без подписи — нерисуемая кнопка, запрещаем;
--   * схема ссылки проверяется и здесь тоже. Основная проверка живёт в
--     domain.ValidateExternalActionURL (там же — запрет javascript:, data:,
--     учётных данных в URL и управляющих символов), а этот CHECK — второй
--     рубеж: он ловит запись мимо приложения (ручной UPDATE, будущий импорт) и
--     стоит ноль на INSERT.
-- ---------------------------------------------------------------------------

ALTER TABLE events
    ADD COLUMN action_label varchar,
    ADD COLUMN action_url   varchar;

ALTER TABLE events
    ADD CONSTRAINT events_action_url_needs_label
        CHECK (action_url IS NULL OR action_label IS NOT NULL),
    ADD CONSTRAINT events_action_url_scheme
        CHECK (action_url IS NULL OR action_url ~* '^https?://[^[:space:]]+$');

COMMENT ON COLUMN events.action_label IS
    'Подпись кнопки действия. NULL = кнопки нет.';
COMMENT ON COLUMN events.action_url IS
    'Внешняя ссылка кнопки (http/https). NULL при заданной подписи = кнопка '
        'ведёт на страницу самого события.';

-- +goose Down

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_action_url_scheme,
    DROP CONSTRAINT IF EXISTS events_action_url_needs_label,
    DROP COLUMN IF EXISTS action_url,
    DROP COLUMN IF EXISTS action_label;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_platform_not_ticketed;

DROP TRIGGER IF EXISTS trg_promos_sync_city ON promos;
DROP FUNCTION IF EXISTS promos_sync_city();
DROP INDEX IF EXISTS idx_promos_city_id;
ALTER TABLE promos
    DROP CONSTRAINT IF EXISTS promos_city_id_fkey,
    DROP COLUMN IF EXISTS city_id,
    DROP COLUMN IF EXISTS city;

-- Откат NOT NULL честно ломается, если «витрина» уже наполнена, — и это
-- правильно. Молча удалить контент платформы, чтобы колонка снова стала
-- обязательной, откат права не имеет: пусть тот, кто откатывает, сначала
-- решит, что делать с этими строками. Сообщение об ошибке будет ровно про
-- нарушение NOT NULL на конкретной таблице.
ALTER TABLE events ALTER COLUMN restaurant_id SET NOT NULL;
ALTER TABLE promos ALTER COLUMN restaurant_id SET NOT NULL;
