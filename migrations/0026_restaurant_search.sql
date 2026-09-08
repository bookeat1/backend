-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- restaurant_search_text builds the full searchable document of a restaurant
-- across EVERY locale: the base (ru) name/description columns plus every value
-- stored in the *_i18n jsonb translations. A venue's identity is
-- locale-independent, so indexing the union lets a query typed in any request
-- locale still match the venue; the request locale governs only which text the
-- response renders and how results are ranked, never whether a row can match.
--
-- The function is marked IMMUTABLE so it can back an expression index. That
-- promise only holds if the output is deterministic for a given input, which is
-- why string_agg has an explicit ORDER BY key — without it the concatenation
-- order of jsonb keys would be unspecified and the index could disagree with a
-- query's recomputation of the same expression.
-- +goose StatementBegin
-- COST 1000 (vs the default 100) reflects the real expense of this function:
-- two jsonb_each_text scans + string_agg per call. Without it the planner
-- underestimates a sequential scan that recomputes the function on every row
-- and skips the GIN indexes entirely; with it, the query below plans as a
-- BitmapOr over both indexes (verified with EXPLAIN on 3000 seeded rows).
CREATE OR REPLACE FUNCTION restaurant_search_text(
    name text, description text, name_i18n jsonb, description_i18n jsonb
) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE COST 1000 AS $$
    SELECT concat_ws(' ',
        name,
        description,
        (SELECT string_agg(value, ' ' ORDER BY key)
           FROM jsonb_each_text(COALESCE(name_i18n, '{}'::jsonb))),
        (SELECT string_agg(value, ' ' ORDER BY key)
           FROM jsonb_each_text(COALESCE(description_i18n, '{}'::jsonb)))
    )
$$;
-- +goose StatementEnd

-- Full-text index for exact/stemmed matching and ts_rank scoring. The 'russian'
-- config gives real morphological stemming for the predominantly-Russian
-- content; the query recomputes the identical to_tsvector('russian', ...)
-- expression so the planner can use this index.
CREATE INDEX idx_restaurants_search_fts ON restaurants
    USING gin (to_tsvector('russian',
        restaurant_search_text(name, description, name_i18n, description_i18n)));

-- Trigram index for typo tolerance / partial matches via the word_similarity
-- operators (<%). Complements FTS: a misspelled term that produces no matching
-- lexeme still finds the venue through trigram similarity to a substring.
CREATE INDEX idx_restaurants_search_trgm ON restaurants
    USING gin (
        restaurant_search_text(name, description, name_i18n, description_i18n)
        gin_trgm_ops);

-- +goose Down
DROP INDEX IF EXISTS idx_restaurants_search_trgm;
DROP INDEX IF EXISTS idx_restaurants_search_fts;
DROP FUNCTION IF EXISTS restaurant_search_text(text, text, jsonb, jsonb);
-- pg_trgm is intentionally NOT dropped: the extension is a shared, additive
-- facility that other features may rely on, and dropping it would fail if any
-- object still depends on it. Leaving it is the safe, reversible-in-practice
-- choice.
