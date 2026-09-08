-- +goose Up

-- The restaurant detail and the search/list cards want to show an ACTUAL
-- average-check range ("8 000–15 000 ₸") next to the existing categorical
-- price tier price_category ("₸"/"₸₸"/"₸₸₸"). price_category stays exactly as
-- it is — this is purely additive, older clients keep reading the tier and
-- never see the new numbers.
--
-- price_min / price_max are the lower and upper bound of the average check in
-- WHOLE tenge (integer, not minor units: this is a human-facing display range,
-- not a charge — no arithmetic is done on it, so there is nothing to lose to a
-- rounding to the tenge). Both NULLABLE with no default on purpose: most venues
-- have not declared a range yet, and inventing a 0–0 for them would make the
-- API claim a free average check. NULL means "no range declared", the response
-- omits the field and the client draws no range line.
--
-- The CHECK enforces both-or-neither AND ordering: either both bounds are NULL,
-- or both are set with 0 <= price_min <= price_max. The both-or-neither half is
-- deliberate — a plain `price_min IS NULL OR (...)` would let a half-set row
-- (min set, max NULL) slip through, because in Postgres a CHECK only fails on
-- FALSE and `NULL >= 5000` is unknown, not false. Spelling out both NOT NULL
-- makes the half-set case evaluate to FALSE and be refused at the schema, so no
-- read path downstream has to defend against a lone bound.
--
-- Safe on live rows: two nullable columns with no default are a catalog-only
-- change, Postgres does not rewrite the table, and the CHECK holds for every
-- existing row (both bounds NULL → first branch TRUE).
ALTER TABLE restaurants
    ADD COLUMN price_min integer,
    ADD COLUMN price_max integer,
    ADD CONSTRAINT restaurants_price_range_check CHECK (
        (price_min IS NULL AND price_max IS NULL)
        OR (price_min IS NOT NULL AND price_max IS NOT NULL
            AND price_min >= 0 AND price_max >= price_min)
    );

-- +goose Down

ALTER TABLE restaurants
    DROP CONSTRAINT restaurants_price_range_check,
    DROP COLUMN price_max,
    DROP COLUMN price_min;
