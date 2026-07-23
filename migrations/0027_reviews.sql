-- +goose Up

-- Guest reviews & ratings for a restaurant. A review is a "verified" review:
-- the usecase (internal/usecase/reviews) only lets a guest create/edit one
-- when they have a COMPLETED booking at that restaurant — this table stores no
-- verification flag because the rule is enforced at write time, not persisted
-- as state that could drift. The role→permission gate for the venue's reply /
-- moderation actions lives in Go (internal/domain/rbac.go, PermStaffManage),
-- not here, same convention as every other table.
--
-- One active review per (restaurant, user): a UNIQUE constraint, not an
-- application-level "check before insert" — the guest upsert relies on it
-- (ON CONFLICT). Editing replaces the same row; deleting removes it, so the
-- constraint reads as "at most one live review per guest per venue".
CREATE TABLE reviews
(
    id            uuid PRIMARY KEY,
    restaurant_id uuid        NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    rating        smallint    NOT NULL CHECK (rating BETWEEN 1 AND 5),
    body          text        NOT NULL DEFAULT '',
    status        varchar     NOT NULL DEFAULT 'published' CHECK (status IN ('published', 'hidden')),
    owner_reply   text,
    replied_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- A reply and its timestamp are set together or not at all: an owner_reply
    -- without a replied_at (or vice-versa) is a bug, never a valid state.
    CONSTRAINT reviews_reply_pairing CHECK ((owner_reply IS NULL) = (replied_at IS NULL)),
    -- One live review per guest per venue. Editing upserts onto this key;
    -- deleting removes the row. The public listing / aggregate never depends
    -- on it, only the guest-facing upsert does.
    CONSTRAINT reviews_one_per_user_restaurant UNIQUE (restaurant_id, user_id)
);

-- +goose Down
DROP TABLE reviews;
