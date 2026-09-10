-- +goose Up

-- Data migration: seeds the ONE platform promo that is the «Марафон Алматы»
-- campaign's reference tag. See docs/PRD.md's booking flow (promotion_id,
-- migration 0004) and the marathon spec (project memory
-- marathon-qr-promo-20260906) for the surrounding feature — this migration
-- only creates the row that campaign tags bookings against.
--
-- WHY A MIGRATION AND NOT A ONE-OFF POST /admin/platform/promos CALL. The
-- promo's id is the campaign's tag, referenced from now on by
-- bookings.promotion_id (no FK, migration 0004) and handed to the mobile/web
-- clients as a constant. A migration gives that id a single, reviewable,
-- reproducible source of truth across every environment (local, test, prod)
-- instead of "whatever a human pasted into Postman once" — the same reasoning
-- campaign_links slugs would get in the fuller QR spec, applied to the one row
-- this narrower pass actually needs.
--
-- WHY status='published' WITH NO VENUE. RestaurantID NULL is a PLATFORM promo
-- (migration 0085): "все активные заведения" (owner's answer, marathon spec
-- §0.7) falls out for free — there is no restaurant to scope it to, so a
-- booking at ANY restaurant may carry this tag. published + a window
-- containing "now" is exactly the visibility PromoRepository.GetPublic checks
-- (internal/domain/promo.go), which is also what
-- usecase/bookings.createUseCase now validates a guest-supplied promotion_id
-- against — see create.go.
--
-- WHY feed_status='approved' TOO. POST /admin/platform/promos auto-approves a
-- platform promo's OWN card the moment it is created (no venue submits to
-- itself — see usecase/promos.Create); a row seeded straight into the table
-- mirrors that same end state rather than sitting at 'not_submitted' forever,
-- invisible to the merchandising feed a real API call would have put it in.
-- feed_reviewed_by is left NULL: nobody's admin account approved this one, a
-- migration did, and inventing a reviewer id would misrepresent the audit
-- trail.
--
-- WHY discount_percent IS NULL. The owner's decision (marathon spec §0.6):
-- "скидки нет ... промокод остаётся, но меняет смысл: это метка участия".
--
-- THE WINDOW. Marathon day is 2026-09-27 (spec §0.5). starts_at opens a few
-- weeks early so bookings can be made ahead of the day; ends_at leaves a
-- month of buffer past it for confirmations, visits and the mercy export to
-- settle before the tag stops validating on new bookings. Adjust with a
-- plain UPDATE if marketing needs a different window — nothing else depends
-- on these two timestamps.
--
-- NUMBER. Highest migration number across every origin/* and local branch as
-- of 2026-09-10 is 0106 (`story_expiry`, unmerged); 0107 is free.
SET lock_timeout = '3s';

INSERT INTO promos (
    id, restaurant_id, title, description, starts_at, ends_at, terms,
    status, feed_status, feed_submitted_at, feed_reviewed_at,
    created_at, updated_at
) VALUES (
    '6a3736b9-d4e5-4ec6-9ed2-7233476184fd',
    NULL,
    'Марафон Алматы',
    'Забронируйте стол с меткой акции «Марафон Алматы» и приходите — после '
        'подтверждённого визита мы вручим подарок.',
    '2026-09-01 00:00:00+06',
    '2026-10-27 00:00:00+06',
    'Скидки нет: это метка участия в акции. Подарок (мерч) вручается '
        'сотрудником вручную после подтверждённого визита, по списку '
        'участников.',
    'published',
    'approved',
    now(),
    now(),
    now(),
    now()
);

-- +goose Down

DELETE FROM promos WHERE id = '6a3736b9-d4e5-4ec6-9ed2-7233476184fd';
