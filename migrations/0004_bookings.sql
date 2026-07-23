-- +goose Up

-- btree_gist lets a GiST exclusion constraint mix an equality operator on a
-- scalar column (table_id WITH =) with an overlap operator on a range
-- (slot WITH &&). Without it the constraint in booking_tables cannot be built.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Booking policy overrides, all NULLABLE: NULL means "take the global default
-- from BOOKING_DEFAULT_* / BOOKING_TIMEZONE_FALLBACK env".
ALTER TABLE restaurants
    ADD COLUMN timezone                 varchar,
    ADD COLUMN booking_duration_minutes integer,
    ADD COLUMN booking_buffer_minutes   integer,
    ADD COLUMN booking_lead_minutes     integer,
    ADD COLUMN booking_horizon_days     integer,
    ADD COLUMN cancel_deadline_minutes  integer,
    ADD COLUMN confirm_sla_minutes      integer,
    ADD COLUMN max_guests_per_booking   integer,
    ADD COLUMN auto_confirm             boolean;

-- promotion_id / event_id are intentionally stored WITHOUT a foreign key: a
-- promotion or an event may be deleted, but the booking must survive it as a
-- historical fact.
CREATE TABLE bookings
(
    id                         uuid PRIMARY KEY,
    restaurant_id              uuid        NOT NULL REFERENCES restaurants (id),
    user_id                    uuid        REFERENCES users (id) ON DELETE SET NULL,
    name                       varchar     NOT NULL,
    phone                      varchar     NOT NULL,
    email                      varchar     NOT NULL DEFAULT '',
    phone_normalized           varchar     NOT NULL,
    guests                     integer     NOT NULL CHECK (guests > 0),
    starts_at                  timestamptz NOT NULL,
    ends_at                    timestamptz NOT NULL,
    status                     varchar     NOT NULL DEFAULT 'pending',
    source                     varchar     NOT NULL DEFAULT 'app',
    notes                      varchar,
    promotion_id               uuid,
    event_id                   uuid,
    created_by_admin           boolean     NOT NULL DEFAULT false,
    forced_placement           boolean     NOT NULL DEFAULT false,
    confirmed_at               timestamptz,
    arrived_at                 timestamptz,
    cancelled_at               timestamptz,
    cancelled_by               varchar,
    cancellation_reason_code   varchar,
    cancellation_reason        varchar,
    late_notification_sent     boolean     NOT NULL DEFAULT false,
    user_notified_late_at      timestamptz,
    user_late_message          varchar,
    reminder_60_sent_at        timestamptz,
    reminder_30_sent_at        timestamptz,
    original_booking_time_text varchar,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX idx_bookings_restaurant_starts ON bookings (restaurant_id, starts_at);
CREATE INDEX idx_bookings_user_starts ON bookings (user_id, starts_at DESC);
CREATE INDEX idx_bookings_status_starts ON bookings (status, starts_at);
CREATE INDEX idx_bookings_phone_normalized ON bookings (phone_normalized);

-- slot ALREADY INCLUDES the restaurant's buffer on both sides, resolved inside
-- the same transaction as the insert. Overbooking is prevented by the database,
-- not by an application-level check (a check loses the race, a constraint does
-- not). active is maintained exclusively by the trigger below.
CREATE TABLE booking_tables
(
    id         uuid PRIMARY KEY,
    booking_id uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    table_id   uuid        NOT NULL REFERENCES restaurant_tables (id),
    slot       tstzrange   NOT NULL,
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    EXCLUDE USING gist (table_id WITH =, slot WITH &&) WHERE (active)
);
CREATE INDEX idx_booking_tables_booking ON booking_tables (booking_id);
CREATE INDEX idx_booking_tables_table ON booking_tables (table_id);

-- +goose StatementBegin
CREATE FUNCTION sync_booking_tables_active() RETURNS trigger AS
$$
BEGIN
    UPDATE booking_tables
    SET active = (NEW.status IN ('pending', 'confirmed', 'arrived'))
    WHERE booking_id = NEW.id
      AND active IS DISTINCT FROM (NEW.status IN ('pending', 'confirmed', 'arrived'));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_bookings_sync_tables_active
    AFTER UPDATE OF status
    ON bookings
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION sync_booking_tables_active();

-- menu_item_id is nullable and ON DELETE SET NULL: the dish may disappear from
-- the menu, but the pre-order line stays. item_price_minor is frozen at booking
-- time (tiyn) and is never rewritten when the menu price changes.
CREATE TABLE booking_items
(
    id               uuid PRIMARY KEY,
    booking_id       uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    menu_item_id     uuid        REFERENCES menu_items (id) ON DELETE SET NULL,
    item_name        varchar     NOT NULL,
    item_price_minor bigint      NOT NULL DEFAULT 0 CHECK (item_price_minor >= 0),
    currency         varchar(3)  NOT NULL DEFAULT 'KZT',
    quantity         integer     NOT NULL DEFAULT 1 CHECK (quantity > 0),
    status           varchar     NOT NULL DEFAULT 'pending',
    comment          varchar,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_booking_items_booking ON booking_items (booking_id);

CREATE TABLE booking_messages
(
    id          uuid PRIMARY KEY,
    booking_id  uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    sender_type varchar     NOT NULL,
    sender_id   uuid        REFERENCES users (id) ON DELETE SET NULL,
    message     varchar     NOT NULL,
    is_read     boolean     NOT NULL DEFAULT false,
    read_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_booking_messages_booking_created ON booking_messages (booking_id, created_at);

-- restaurant_id NULL = a global (platform-wide) blacklist entry.
CREATE TABLE booking_blacklist
(
    id               uuid PRIMARY KEY,
    restaurant_id    uuid        REFERENCES restaurants (id) ON DELETE CASCADE,
    user_id          uuid        REFERENCES users (id) ON DELETE SET NULL,
    phone_normalized varchar,
    email            varchar,
    reason           varchar,
    created_by       uuid        REFERENCES users (id) ON DELETE SET NULL,
    is_active        boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (user_id IS NOT NULL OR phone_normalized IS NOT NULL OR email IS NOT NULL)
);
-- Partial unique indexes: one active entry per (scope, identifier). Global rows
-- (restaurant_id IS NULL) need their own index because NULL never equals NULL.
CREATE UNIQUE INDEX idx_booking_blacklist_rest_phone
    ON booking_blacklist (restaurant_id, phone_normalized)
    WHERE is_active AND restaurant_id IS NOT NULL AND phone_normalized IS NOT NULL;
CREATE UNIQUE INDEX idx_booking_blacklist_global_phone
    ON booking_blacklist (phone_normalized)
    WHERE is_active AND restaurant_id IS NULL AND phone_normalized IS NOT NULL;
CREATE INDEX idx_booking_blacklist_user ON booking_blacklist (user_id) WHERE is_active;
CREATE INDEX idx_booking_blacklist_email ON booking_blacklist (email) WHERE is_active;

CREATE TABLE booking_rate_log
(
    id               uuid PRIMARY KEY,
    user_id          uuid        REFERENCES users (id) ON DELETE SET NULL,
    phone_normalized varchar,
    email            varchar,
    restaurant_id    uuid        REFERENCES restaurants (id) ON DELETE CASCADE,
    action           varchar     NOT NULL DEFAULT 'create',
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_booking_rate_log_phone_created ON booking_rate_log (phone_normalized, created_at DESC);
CREATE INDEX idx_booking_rate_log_user_created ON booking_rate_log (user_id, created_at DESC);

CREATE TABLE restaurant_surveys
(
    id               uuid PRIMARY KEY,
    -- booking_id is nullable in Supabase: a guest can rate a place without a booking.
    booking_id       uuid        REFERENCES bookings (id) ON DELETE CASCADE,
    restaurant_id    uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    rating_overall   smallint    NOT NULL CHECK (rating_overall BETWEEN 1 AND 5),
    rating_food      smallint    NOT NULL CHECK (rating_food BETWEEN 1 AND 5),
    rating_service   smallint    NOT NULL CHECK (rating_service BETWEEN 1 AND 5),
    rating_ambience  smallint    NOT NULL CHECK (rating_ambience BETWEEN 1 AND 5),
    nps              smallint    NOT NULL CHECK (nps BETWEEN 0 AND 10),
    comment          varchar,
    dismissed        boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_restaurant_surveys_booking ON restaurant_surveys (booking_id) WHERE booking_id IS NOT NULL;
CREATE INDEX idx_restaurant_surveys_restaurant ON restaurant_surveys (restaurant_id, created_at DESC);

-- Written in the same transaction as the status change it describes.
CREATE TABLE booking_status_history
(
    id          uuid PRIMARY KEY,
    booking_id  uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    from_status varchar,
    to_status   varchar     NOT NULL,
    actor_type  varchar     NOT NULL,
    actor_id    uuid,
    reason      varchar,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_booking_status_history_booking ON booking_status_history (booking_id, created_at);

-- Transactional outbox: written together with the booking mutation, drained by
-- a worker that hands events to the existing (Deno edge) notification layer.
CREATE TABLE booking_outbox
(
    id           uuid PRIMARY KEY,
    booking_id   uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    event_type   varchar     NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX idx_booking_outbox_unpublished ON booking_outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX idx_booking_outbox_booking ON booking_outbox (booking_id);

-- +goose Down
DROP TABLE booking_outbox;
DROP TABLE booking_status_history;
DROP TABLE restaurant_surveys;
DROP TABLE booking_rate_log;
DROP TABLE booking_blacklist;
DROP TABLE booking_messages;
DROP TABLE booking_items;
DROP TRIGGER trg_bookings_sync_tables_active ON bookings;
DROP FUNCTION sync_booking_tables_active();
DROP TABLE booking_tables;
DROP TABLE bookings;

ALTER TABLE restaurants
    DROP COLUMN timezone,
    DROP COLUMN booking_duration_minutes,
    DROP COLUMN booking_buffer_minutes,
    DROP COLUMN booking_lead_minutes,
    DROP COLUMN booking_horizon_days,
    DROP COLUMN cancel_deadline_minutes,
    DROP COLUMN confirm_sla_minutes,
    DROP COLUMN max_guests_per_booking,
    DROP COLUMN auto_confirm;

-- btree_gist intentionally left installed: it may be in use by other objects.
-- DROP EXTENSION IF EXISTS btree_gist;
