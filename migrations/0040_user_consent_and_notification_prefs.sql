-- +goose Up

-- Guest data-processing consent + notification opt-out (increment 1).
--
-- Two independent concerns for a GUEST (user), complementing the existing
-- account soft-delete/anonymization ("right to be forgotten"):
--
--   user_consents                  — an APPEND-ONLY audit log of the guest's
--                                    consent to data processing. One row per
--                                    grant OR revoke, never updated or deleted,
--                                    so the platform can prove exactly what was
--                                    agreed, in which policy version, and when.
--                                    The CURRENT state of a consent type is the
--                                    LATEST row for that (user_id, consent_type).
--
--   user_notification_preferences  — the guest's own notification opt-out. A
--                                    MISSING row means "all enabled" (default
--                                    on): opting out is an explicit write, so no
--                                    backfill of existing users is needed. This
--                                    is the GUEST-facing switch; the separate
--                                    restaurant_notification_settings table is
--                                    for STAFF alerts and is untouched here.
--
-- consent_type and version are free VARCHARs validated non-empty in app code
-- (e.g. terms_of_service, privacy_policy, marketing, analytics) — deliberately
-- NOT a DB enum: the canonical consent-type list and the versioning policy are
-- an owner/legal decision, kept as data, not baked into the schema.

CREATE TABLE user_consents
(
    id           uuid PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    consent_type varchar(64) NOT NULL,
    version      varchar(64) NOT NULL,
    granted      boolean     NOT NULL,
    source       varchar(16) NOT NULL, -- "app" | "web"
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- "Latest row per (user, consent_type)" — serves the current-state read
-- (ORDER BY created_at DESC, id DESC) and the per-user history listing.
CREATE INDEX idx_user_consents_user_type_created
    ON user_consents (user_id, consent_type, created_at DESC, id DESC);

CREATE TABLE user_notification_preferences
(
    user_id               uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- Master opt-out: false means the guest wants NO notifications at all.
    notifications_enabled boolean     NOT NULL DEFAULT true,
    -- Per-channel toggles, nested under the master switch: a channel is only
    -- eligible when BOTH notifications_enabled AND the channel flag are true.
    push_enabled          boolean     NOT NULL DEFAULT true,
    email_enabled         boolean     NOT NULL DEFAULT true,
    updated_at            timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE user_notification_preferences;
DROP TABLE user_consents;
