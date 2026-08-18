-- +goose Up

-- Recurring events («Cocktail Wednesday», «Караоке-битва по четвергам»).
--
-- Until now an event was ALWAYS a single row with one starts_at/ends_at, so a
-- weekly happening could only be expressed by hand-inserting this week's rows —
-- and the Афиша emptied itself again a week later. This migration adds the RULE
-- (event_recurrences), the link from a generated occurrence back to its rule
-- (events.recurrence_id) and the tombstone table that keeps a cancelled or
-- deleted occurrence from being resurrected (event_recurrence_skips).
--
-- Three decisions worth stating up front:
--
-- 1. An occurrence is a REAL events row, never a virtual date computed at read
--    time. Tickets (event_tickets.event_id), capacity, the home feed, the
--    moderation queue and every existing read path already hang off an event
--    id; a virtual occurrence would have nothing to sell a ticket against.
--    The rule is therefore a GENERATOR, not a view.
--
-- 2. The rule stores WALL-CLOCK time (start_minutes since local midnight) plus
--    a zone, never an instant. "Every Wednesday at 19:00" means 19:00 on the
--    wall, and the generator resolves it through time.Date in the venue's zone
--    so a DST transition never shifts the event by an hour (the bug class of
--    2026-07-27, domain/venue_schedule.go).
--
-- 3. timezone is NULLABLE and means "this rule overrides the venue's zone".
--    NULL = use restaurants.timezone, and only if THAT is empty the platform
--    fallback (BOOKING_TIMEZONE_FALLBACK). Asia/Almaty is not hardcoded
--    anywhere: it is merely the current default of that env var. An
--    empty-string zone is refused by a CHECK, because time.LoadLocation("")
--    silently returns UTC — the exact silent-fallback trap documented in
--    domain/venue_timezone.go.
CREATE TABLE event_recurrences
(
    id                           uuid PRIMARY KEY,
    restaurant_id                uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,

    -- ---- the event template: copied onto every occurrence this rule creates ----
    -- Same columns, same types and same meaning as `events`, so a generated
    -- occurrence is indistinguishable from a hand-made event apart from
    -- recurrence_id. Localization follows the catalog convention (base scalar +
    -- optional *_i18n jsonb map).
    title                        varchar     NOT NULL,
    title_i18n                   jsonb,
    description                  text        NOT NULL DEFAULT '',
    description_i18n             jsonb,
    venue                        varchar     NOT NULL DEFAULT '',
    cover_image_url              varchar,
    tags                         text[]      NOT NULL DEFAULT '{}',
    -- The status every generated occurrence is BORN with. Default 'draft' is
    -- deliberately the same conservative default as events.status: a rule does
    -- not get to publish unreviewed content just by existing. The cabinet sends
    -- 'published' explicitly for a rule that should fill the Афиша by itself.
    occurrence_status            varchar     NOT NULL DEFAULT 'draft'
                                     CHECK (occurrence_status IN ('draft', 'published', 'hidden')),
    ticketed                     boolean     NOT NULL DEFAULT false,
    -- Integer minor units (tiyin), never a float — same rule as everywhere else.
    ticket_price_minor           bigint      CHECK (ticket_price_minor IS NULL OR ticket_price_minor >= 0),
    -- Capacity is PER OCCURRENCE: "20 seats every Wednesday" means 20 seats on
    -- each Wednesday, not 20 across the series.
    capacity                     integer     CHECK (capacity IS NULL OR capacity >= 0),
    tickets_refundable           boolean     NOT NULL DEFAULT false,
    ticket_refund_cutoff_minutes integer     NOT NULL DEFAULT 0
                                     CHECK (ticket_refund_cutoff_minutes BETWEEN 0 AND 43200),

    -- ---- the rule itself ----
    frequency                    varchar     NOT NULL
                                     CHECK (frequency IN ('daily', 'weekly', 'monthly')),
    -- ISO weekdays, 1 = Monday … 7 = Sunday (matches Postgres isodow and the
    -- API payload). Only meaningful for frequency='weekly'; empty otherwise.
    weekdays                     smallint[]  NOT NULL DEFAULT '{}',
    -- Day of month for frequency='monthly'. A month that has no such day (the
    -- 31st of February) simply produces no occurrence — the generator skips it
    -- rather than sliding the event to a day nobody asked for.
    month_day                    smallint,
    -- Local start time as minutes since local midnight (0..1439). Stored as an
    -- integer rather than `time` so it can never be mistaken for an instant.
    start_minutes                integer     NOT NULL CHECK (start_minutes >= 0 AND start_minutes < 1440),
    -- Absolute length of one occurrence; ends_at = starts_at + duration.
    duration_minutes             integer     NOT NULL CHECK (duration_minutes > 0 AND duration_minutes <= 10080),
    -- NULL = follow the venue's zone (see the header). NEVER '' — that means
    -- UTC to time.LoadLocation and would silently move the event.
    timezone                     varchar     CHECK (timezone IS NULL OR timezone <> ''),
    -- First calendar day the rule may produce an occurrence on, and the last
    -- one (INCLUSIVE) if the series has an end.
    starts_on                    date        NOT NULL,
    until_date                   date,
    -- Deactivating a rule stops FUTURE generation. It never deletes anything
    -- that was already generated — see the events.recurrence_id note below.
    is_active                    boolean     NOT NULL DEFAULT true,
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT event_recurrences_weekly_needs_weekdays
        CHECK (frequency <> 'weekly' OR cardinality(weekdays) > 0),
    CONSTRAINT event_recurrences_weekdays_in_range
        CHECK (weekdays <@ ARRAY [1, 2, 3, 4, 5, 6, 7]::smallint[]),
    CONSTRAINT event_recurrences_monthly_needs_day
        CHECK (frequency <> 'monthly' OR (month_day IS NOT NULL AND month_day BETWEEN 1 AND 31)),
    CONSTRAINT event_recurrences_until_after_start
        CHECK (until_date IS NULL OR until_date >= starts_on)
);

-- Cabinet listing: one venue's rules, newest first with id as a stable
-- pagination tie-breaker (same shape as idx_events_restaurant_admin).
CREATE INDEX idx_event_recurrences_restaurant
    ON event_recurrences (restaurant_id, created_at DESC, id DESC);

-- The generator's only scan: active rules, keyset-paginated by id.
CREATE INDEX idx_event_recurrences_active
    ON event_recurrences (id)
    WHERE is_active;

-- The link from a generated occurrence back to its rule.
--
-- ON DELETE SET NULL, deliberately, NOT CASCADE: deleting a rule must never
-- delete occurrences that already happened (they carry sold tickets, reviews
-- and history). An orphaned occurrence simply becomes an ordinary one-off
-- event, which is exactly what it is once nothing generates it any more.
--
-- NULL also covers every event that predates this feature, including the six
-- occurrences hand-inserted on production as a stopgap.
ALTER TABLE events
    ADD COLUMN recurrence_id uuid REFERENCES event_recurrences (id) ON DELETE SET NULL;

-- Idempotency of the generator, enforced by the DATABASE and not by a
-- read-then-insert check: one occurrence per (rule, slot). A second pass — or
-- two workers passing at the same instant — inserts nothing, because the
-- generator's INSERT carries ON CONFLICT DO NOTHING against this index.
--
-- Partial (WHERE recurrence_id IS NOT NULL) so ordinary one-off events, which
-- all share a NULL recurrence_id, are not indexed at all.
CREATE UNIQUE INDEX uniq_events_recurrence_slot
    ON events (recurrence_id, starts_at)
    WHERE recurrence_id IS NOT NULL;

-- Tombstones: slots this rule must NEVER fill again.
--
-- The unique index above already stops a duplicate while the occurrence row
-- EXISTS, which is what makes an edited or hidden ("cancelled") occurrence
-- safe: the generator only ever inserts, never updates, and any existing row in
-- any status blocks its slot. The one case the index cannot cover is a HARD
-- DELETE — the row is gone, the slot is free again, and the next pass would
-- cheerfully recreate the event the venue just removed. usecase/events records
-- a skip here before deleting a generated occurrence (and before moving one to
-- a different time), and the generator refuses any slot that has a tombstone.
CREATE TABLE event_recurrence_skips
(
    recurrence_id  uuid        NOT NULL REFERENCES event_recurrences (id) ON DELETE CASCADE,
    slot_starts_at timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recurrence_id, slot_starts_at)
);

-- +goose Down

-- Down is safe on a populated database and loses no occurrence: dropping
-- recurrence_id turns every generated event back into an ordinary one-off event
-- (rows, tickets and history untouched), and only the rules themselves — which
-- nothing else references — go away. The skips table must go FIRST: its FK
-- points at event_recurrences.
DROP TABLE event_recurrence_skips;

-- Dropping the column drops uniq_events_recurrence_slot and the FK with it.
ALTER TABLE events
    DROP COLUMN recurrence_id;

DROP TABLE event_recurrences;
