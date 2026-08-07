-- +goose Up

-- The Figma «Афиша» card and the event detail show short TAG CHIPS under the
-- venue·date line ("Бранч", "Живая музыка", "Коктейли", "Красивый вид"). Events
-- carried no tags until now. This adds a plain Postgres text[] of short labels
-- surfaced on the public event read endpoints and editable from the cabinet.
--
-- text[] (not a jsonb array, not a join table): the tags are a flat, short list
-- of free-text chips with no attributes of their own and no cross-event
-- identity to normalize — the same shape the guest app renders inline. A join
-- table would buy referential integrity nobody asked for and cost every read a
-- second query.
--
-- NOT NULL DEFAULT '{}' on purpose: "no tags" is the empty array, never NULL, so
-- no read path has to special-case a nil and the API always serializes a real
-- list ([]). Safe on live rows: adding a NOT NULL column WITH a constant default
-- is a catalog-only change in Postgres (11+), it does not rewrite the table, and
-- every existing row reads back '{}'.
ALTER TABLE events
    ADD COLUMN tags text[] NOT NULL DEFAULT '{}';

-- +goose Down

ALTER TABLE events
    DROP COLUMN tags;
