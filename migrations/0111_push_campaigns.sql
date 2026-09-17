-- +goose Up

-- Пуш-кампании: суперадмин вручную рассылает пуш по опубликованной карточке
-- (событие/акция) всем гостям приложения того же города (spec
-- push-campaigns-manual-spec-2026-09-17.md, §5.2). Никакой автоматики — эта
-- миграция кладёт только данные под РУЧНУЮ отправку: очередь кампаний, решение
-- по каждому гостю (нужно для лимитов частоты и дедупа) и три маленькие правки
-- существующих таблиц, чтобы лента и тикеты знали про кампанию.

SET lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. Гость может выключить именно МАРКЕТИНГОВЫЕ пуши, не трогая брони.
--
-- opt-out, не opt-in (решение владельца §0.2): DEFAULT true, «строки нет» уже
-- сегодня означает «всё включено» (domain.DefaultNotificationPreference) —
-- добавление колонки с DEFAULT true не меняет эффективное поведение ни для
-- одного существующего гостя, ADD COLUMN ... DEFAULT на этой версии Postgres
-- не переписывает строки (быстрый путь для NOT NULL DEFAULT constant).
-- ---------------------------------------------------------------------------

ALTER TABLE user_notification_preferences
    ADD COLUMN promo_push_enabled boolean NOT NULL DEFAULT true;

COMMENT ON COLUMN user_notification_preferences.promo_push_enabled IS
    'Тумблер «Акции и события» (маркетинговые пуши по событиям/акциям всего '
        'города). DEFAULT true = opt-out: гость получает их, пока не выключит '
        'сам. Независим от notifications_enabled/push_enabled, но подчинён им '
        '(эффективно разрешено только когда все три true).';

-- ---------------------------------------------------------------------------
-- 2. Очередь кампаний. Одна строка = одно нажатие «Отправить пуш».
-- ---------------------------------------------------------------------------

CREATE TABLE push_campaigns
(
    id uuid PRIMARY KEY,
    -- 'event' | 'promo' — какая сущность. Валидируется в коде, не ENUM (та же
    -- дисциплина, что у booking status).
    kind             varchar(8)  NOT NULL,
    -- events.id / promos.id. БЕЗ FK: субъект может быть удалён после того, как
    -- кампания уже отправлена (3.15) — судьба исторической строки кампании не
    -- должна зависеть от того, жива ли карточка, ровно как push_tickets не
    -- зависит от booking_outbox (0102).
    subject_id       uuid        NOT NULL,
    -- NULL = субъект платформы (без заведения). Тоже без FK — то же самое
    -- «переживает удаление» рассуждение, а восстанавливать заведение по этому
    -- полю кампания и не пытается: оно тут только для отчёта GET
    -- /admin/push-campaigns?restaurant_id=.
    restaurant_id    uuid,
    -- Эффективный город субъекта НА МОМЕНТ СОЗДАНИЯ кампании (см. §5.4 —
    -- COALESCE(subject.city_id, restaurant.city_id)). NULL = «везде»
    -- (платформенный субъект без переопределения). FK на справочник ради
    -- целостности; города не удаляются (только is_active=false), поэтому
    -- ON DELETE SET NULL практически никогда не сработает — это подстраховка,
    -- не рабочий путь.
    city_id          uuid REFERENCES cities (id) ON DELETE SET NULL,
    -- Написание города для отчёта (модалка/бейдж), снимок на момент создания —
    -- не пересчитывается, даже если город потом переименуют.
    city             varchar,
    -- queued -> sending -> done|failed; queued|sending -> cancelled|expired.
    -- Валидируется в коде (домен), не ENUM.
    status           varchar(16) NOT NULL DEFAULT 'queued',
    -- Причина cancelled/failed, для бейджа кабинета и разбора. NULL пока не
    -- терминально или причина не нужна (done).
    cancel_reason    varchar(32),
    -- Создатель явно подтвердил отправку в тихие часы (3.8). Без этого
    -- POST в 21:00-10:00 Asia/Almaty падает 422 quiet_hours ДО создания
    -- строки — сюда попадает уже только подтверждённое решение.
    force_quiet_hours boolean    NOT NULL DEFAULT false,
    -- Оценка охвата на момент POST (что видел админ, подтверждая) — застывает,
    -- не пересчитывается: бейдж «отправляется» показывает именно её, пока
    -- воркер не даст точные sent_count/skipped_count/failed_count.
    estimated_recipients int     NOT NULL DEFAULT 0,
    -- Попытки ОТПРАВИТЬ ПАЧКУ Expo для этой кампании (не попытки на гостя —
    -- решение по гостю пишется один раз, см. push_campaign_recipients).
    -- 429/5xx у Expo бьёт по всей кампании сразу, поэтому и бэкофф на уровне
    -- кампании (критерий 16).
    attempts         int         NOT NULL DEFAULT 0,
    -- Бэкофф после неудачной попытки: воркер не берёт кампанию раньше этого
    -- времени. NULL = готова сразу (свежая queued, или прошлая попытка не
    -- проваливалась).
    next_attempt_at  timestamptz,
    -- Аренда claim+lease (критерий 10): пока lease_until в будущем, кампания
    -- считается «уже взятой» и второй процесс воркера её не тронет, даже если
    -- первый процесс упал между claim-транзакцией и отправкой (лок FOR UPDATE
    -- SKIP LOCKED снимается сразу после короткой claim-транзакции — держать
    -- его на время сетевого похода к Expo нельзя, см. usecase/payments.
    -- Reconciler за тем же обоснованием).
    lease_until      timestamptz,
    last_error       text,
    sent_count       int         NOT NULL DEFAULT 0,
    skipped_count    int         NOT NULL DEFAULT 0,
    failed_count     int         NOT NULL DEFAULT 0,
    -- Кто нажал «Отправить пуш». ON DELETE SET NULL — удаление аккаунта
    -- суперадмина не должно ломать историю уже отправленных кампаний
    -- (сравни created_by у payouts/promo_codes). Колонка nullable ИМЕННО
    -- из-за этого правила, хотя на запись усecase всегда обязан её заполнить —
    -- «NOT NULL при создании» это инвариант приложения, не ограничение БД.
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    CONSTRAINT push_campaigns_kind_check CHECK (kind IN ('event', 'promo')),
    CONSTRAINT push_campaigns_status_check
        CHECK (status IN ('queued', 'sending', 'done', 'cancelled', 'expired', 'failed'))
);

-- Двойной клик / два админа (3.3): пока кампания по субъекту не терминальна,
-- второй POST должен упереться в 409, а не завести параллельную рассылку тому
-- же городу. Частичный уникальный индекс — это гарантия НА УРОВНЕ БАЗЫ
-- (гонка двух вставок ловится констрейнтом, а не read-then-write в коде),
-- ровно так же, как остальная платформа решает двойную бронь/двойной платёж.
CREATE UNIQUE INDEX uq_push_campaigns_subject_active
    ON push_campaigns (kind, subject_id) WHERE status IN ('queued', 'sending');

-- Рабочий запрос воркера: «взять следующую готовую кампанию» — queued (свежая)
-- или sending с истёкшей арендой и не в бэкоффе (см. Sender.Claim). Частичный
-- индекс сжимается по мере разбора очереди, ровно как idx_push_tickets_unresolved.
CREATE INDEX idx_push_campaigns_pending
    ON push_campaigns (created_at) WHERE status IN ('queued', 'sending');

-- Бейдж на карточке = «последняя кампания субъекта» (§5.3), критерии 5/6.
CREATE INDEX idx_push_campaigns_subject_created
    ON push_campaigns (kind, subject_id, created_at DESC);

-- campaigns_today_in_city (критерий 3, предупреждение в модалке).
CREATE INDEX idx_push_campaigns_city_created
    ON push_campaigns (city_id, created_at DESC);

COMMENT ON TABLE push_campaigns IS
    'Очередь ручных пуш-кампаний (одна кнопка «Отправить пуш» = одна строка). '
        'Не путать с обычной доставкой брони — это отдельный отправитель '
        '(usecase/pushcampaigns.Sender), не usecase/notifications.Dispatcher.';

-- ---------------------------------------------------------------------------
-- 3. Решение по каждому гостю. Существует ради трёх вещей разом: лимит
-- 1/сутки+3/неделю НА ГОСТЯ (а не на кампанию), дедуп «не слать повторно
-- получившим» (3.5) и at-most-once (критерий 14 — решение пишется ДО
-- отправки, повторный заход воркера не трогает sent/sending).
-- ---------------------------------------------------------------------------

CREATE TABLE push_campaign_recipients
(
    campaign_id uuid        NOT NULL REFERENCES push_campaigns (id) ON DELETE CASCADE,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- sending -> sent|failed (терминальны); skipped_* ставятся сразу и
    -- терминальны сразу — гость никогда не переходит из skipped_* в sent
    -- внутри ОДНОЙ кампании (повторная отправка — это новая строка кампании).
    status      varchar(20) NOT NULL,
    decided_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, user_id),
    CONSTRAINT push_campaign_recipients_status_check
        CHECK (status IN ('sending', 'sent', 'failed',
                           'skipped_optout', 'skipped_cap', 'skipped_duplicate', 'skipped_no_device'))
);

-- Лимит 24ч/7д и дедуп по субъекту (критерий 13) оба читают «мои sent-строки,
-- новые сначала» — один и тот же индекс обслуживает оба запроса (дедуп ещё
-- джойнит push_campaigns по kind/subject_id, но стартует с этого же индекса
-- по user_id). Частичный — skipped_*/sending читать под лимит не нужно.
CREATE INDEX idx_push_campaign_recipients_user_sent
    ON push_campaign_recipients (user_id, decided_at DESC) WHERE status = 'sent';

COMMENT ON TABLE push_campaign_recipients IS
    'Решение по одному гостю в одной кампании. sent — засчитывается в лимит '
        '24ч/7д и в дедуп «не слать повторно» для ЭТОГО subject_id, по любой '
        'кампании (не только этой).';

-- ---------------------------------------------------------------------------
-- 4. Лента `notifications` (0069) узнаёт про кампании.
--
-- outbox_event_id был NOT NULL FK booking_outbox — ровно один продюсер
-- (usecase/notifications.FeedNotifier). Теперь продюсеров два: booking outbox
-- И push-кампания, и у записи кампании нет outbox-события вовсе. Колонка
-- становится NULLABLE и теряет FK (та же причина, что у push_tickets.
-- outbox_event_id в 0102 — судьба записи ленты не должна зависеть от того,
-- жива ли строка booking_outbox), а CHECK гарантирует, что у каждой строки
-- ровно один продюсер, никогда оба и никогда ни одного.
-- ---------------------------------------------------------------------------

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_outbox_event_id_fkey,
    ALTER COLUMN outbox_event_id DROP NOT NULL,
    ADD COLUMN campaign_id uuid REFERENCES push_campaigns (id) ON DELETE SET NULL,
    -- Дальше — куда тапнуть. event_id/promo_id NULLABLE и ON DELETE SET NULL,
    -- ровно как уже существующие booking_id/restaurant_id на этой же таблице:
    -- удалённый субъект не должен стирать запись истории гостя, только её
    -- диплинк (3.15 — тап после этого просто помечает прочитанным).
    ADD COLUMN event_id uuid REFERENCES events (id) ON DELETE SET NULL,
    ADD COLUMN promo_id uuid REFERENCES promos (id) ON DELETE SET NULL,
    ADD CONSTRAINT notifications_one_producer_check
        CHECK (num_nonnulls(outbox_event_id, campaign_id) = 1);

-- Идемпотентность кампании на ленте: «один гость — одна запись на кампанию»,
-- отдельно от существующего uq_notifications_event_user (тот ключ бессмыслен
-- здесь — outbox_event_id у этих строк всегда NULL, а NULL в уникальном
-- индексе Postgres не сравнивается сам с собой, так что старый индекс молча
-- пропустил бы дубли). Партиционируется по campaign_id IS NOT NULL, чтобы не
-- задевать существующий путь бронирований вообще.
CREATE UNIQUE INDEX uq_notifications_campaign_user
    ON notifications (campaign_id, user_id) WHERE campaign_id IS NOT NULL;

COMMENT ON COLUMN notifications.outbox_event_id IS
    'Событие booking_outbox, породившее запись — для брони/напоминания. NULL '
        'у записей пуш-кампании (см. campaign_id). Ровно одно из двух не NULL.';
COMMENT ON COLUMN notifications.campaign_id IS
    'Пуш-кампания, породившая запись — для событий/акций. NULL у записей брони.';
COMMENT ON COLUMN notifications.event_id IS
    'Событие для диплинка (тип event). NULL для остальных типов записи.';
COMMENT ON COLUMN notifications.promo_id IS
    'Акция для диплинка (тип promo). NULL для остальных типов записи.';

-- ---------------------------------------------------------------------------
-- 5. Квитанции Expo (0102) узнают, какой кампании принадлежит тикет — то же
-- forensics-поле, что и существующий outbox_event_id на этой таблице: НЕТ FK
-- (тикет живёт сутки и его судьба не должна зависеть от кампании), просто
-- «какая кампания это отправила», для разбора инцидентов.
-- ---------------------------------------------------------------------------

ALTER TABLE push_tickets
    ADD COLUMN campaign_id uuid;

COMMENT ON COLUMN push_tickets.campaign_id IS
    'Кампания, породившая этот тикет (см. outbox_event_id — то же самое поле '
        'для пуш-кампаний, тоже без FK, тоже только для разбора инцидентов).';

-- +goose Down

ALTER TABLE push_tickets
    DROP COLUMN campaign_id;

DROP INDEX IF EXISTS uq_notifications_campaign_user;
ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_one_producer_check,
    DROP COLUMN campaign_id,
    DROP COLUMN event_id,
    DROP COLUMN promo_id;
-- Восстановление NOT NULL + FK падает (и обязано упасть) если к моменту
-- отката уже существуют строки ленты, рождённые пуш-кампанией
-- (outbox_event_id IS NULL) — у отката нет данных, чтобы придумать им
-- outbox-событие задним числом. На СВЕЖЕЙ базе / до первой кампании откат
-- проходит чисто, что и проверяет migration_test.go.
ALTER TABLE notifications
    ALTER COLUMN outbox_event_id SET NOT NULL,
    ADD CONSTRAINT notifications_outbox_event_id_fkey
        FOREIGN KEY (outbox_event_id) REFERENCES booking_outbox (id) ON DELETE CASCADE;

DROP TABLE push_campaign_recipients;
DROP TABLE push_campaigns;

ALTER TABLE user_notification_preferences
    DROP COLUMN promo_push_enabled;
