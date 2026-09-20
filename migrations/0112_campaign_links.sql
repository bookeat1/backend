-- +goose Up

-- Marathon Almaty 2026 QR flyer resolver (spec
-- marathon-remainder-plan-20260914.md, M1). campaign_links is the generic
-- "printed QR slug -> destination" dictionary: a slug on a flyer never
-- changes once printed, but WHERE it sends people can (Detour outage, a
-- typo, a new campaign reusing the pattern) — hence a DB row with a
-- target_url rather than a constant in code.
--
-- promotion_id has NO FK, same posture as bookings.promotion_id (migration
-- 0004): it tags a campaign, promos rows can be seeded/retired independently,
-- and a dangling id here only means "the landing's second button points at a
-- promo that no longer resolves", never a broken booking write.
--
-- campaign_link_hits is the scan counter: one row per GET /m/:slug that
-- resolved to an active link, written before the HTML is served (see
-- transport/rest/campaign). No PII — platform (ios/android/other) and a
-- timestamp only, indexed by link_id for the per-slug scan count.
--
-- NUMBER. Spec (14.09) reserved 0109, but as of today (2026-09-20) the
-- highest migration on origin/develop is 0111 (push_campaigns) — 0109/0110
-- went to foodie_options work in the meantime. 0112 is the next free number.
SET lock_timeout = '3s';

CREATE TABLE campaign_links (
    id UUID PRIMARY KEY,
    slug VARCHAR(255) NOT NULL UNIQUE,
    campaign VARCHAR(255) NOT NULL,
    placement VARCHAR(255) NOT NULL,
    promotion_id UUID,
    target_url TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT campaign_links_status_check CHECK (status IN ('active', 'disabled'))
);

CREATE TABLE campaign_link_hits (
    id UUID PRIMARY KEY,
    link_id UUID NOT NULL REFERENCES campaign_links (id) ON DELETE CASCADE,
    platform VARCHAR(20) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX campaign_link_hits_link_id_idx ON campaign_link_hits (link_id);

-- The one slug this campaign needs on the printed flyer (spec §6, open
-- question 1: a single slug, no placement split). promotion_id matches the
-- «Марафон Алматы» platform promo seeded by migration 0107.
INSERT INTO campaign_links (
    id, slug, campaign, placement, promotion_id, target_url, status, created_at
) VALUES (
    gen_random_uuid(),
    'am26-flyer',
    'marathon-almaty-2026',
    'flyer',
    '6a3736b9-d4e5-4ec6-9ed2-7233476184fd',
    'https://bookeat.godetour.link/lQ9BPpUvJc',
    'active',
    now()
);

-- +goose Down

DROP TABLE IF EXISTS campaign_link_hits;
DROP TABLE IF EXISTS campaign_links;
