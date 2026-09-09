-- +goose Up

-- ПЕРЕВОДЫ ДЛЯ ПОЛЕЙ, КОТОРЫЕ ГОСТЬ ВИДИТ, А ПЕРЕВЕСТИ БЫЛО НЕЧЕМ.
--
-- Схема многоязычности в этой базе везде одна: рядом с обычной колонкой
-- (`terms`, `venue`, `caption`) лежит `<колонка>_i18n jsonb` вида
-- {"ru": …, "kk": …, "en": …}. Базовая колонка ВСЕГДА содержит русский текст,
-- чтение резолвит карту первой и падает на колонку, если перевода нет
-- (domain.I18n.Resolve). Инвариант: значение по ключу `ru` равно базовой
-- колонке — его держит код записи, а не БД (см. ниже, почему не CHECK).
--
-- Пять полей ниже этой пары не имели вовсе, хотя их текст показывается гостю:
--   promos.terms                — условия акции («только зал, не суммируется»)
--   events.venue                — зал/площадка внутри заведения
--   events.action_label         — подпись кнопки на карточке («Купить билет»)
--   event_recurrences.venue     — то же поле у ПРАВИЛА серии, откуда даты его
--                                 наследуют (0097)
--   restaurant_stories.caption  — подпись сторис
--
-- БЕЗОПАСНОСТЬ НА ЖИВЫХ СТРОКАХ. Все пять — `ADD COLUMN <name> jsonb` БЕЗ
-- NOT NULL и БЕЗ DEFAULT: на PG 11+ это правка только каталога, без перезаписи
-- таблицы и без долгой блокировки. Существующие строки получают NULL, что для
-- чтения означает «переводов нет» — ровно то, что и было до миграции.
-- Бэкфилла нет намеренно: класть в `ru` копию колонки бессмысленно (резолв и
-- так падает на колонку), а класть русский текст под ключ `kk` — врать гостю.
--
-- ПОЧЕМУ НЕТ CHECK-ОГРАНИЧЕНИЯ НА ИНВАРИАНТ `ru`. Оно проверялось бы на каждой
-- записи и упало бы на любой строке, где перевод уже разошёлся с колонкой
-- (такие строки в проде есть у ресторанов), превратив редактирование в 500.
-- Инвариант держит один шлюз записи в usecase — как и для всех прежних
-- `*_i18n`-колонок, ни одна из которых CHECK не имеет.
--
-- НОМЕР. На 2026-08-29 максимум по ВСЕМ веткам origin — 0100
-- (`origin/develop`, `0100_menu_language_kk.sql`); 0101 свободен.
-- Проверено `git ls-tree origin/<branch> migrations/` по каждой ветке.

SET lock_timeout = '3s';

ALTER TABLE promos
    ADD COLUMN terms_i18n jsonb;

ALTER TABLE events
    ADD COLUMN venue_i18n jsonb,
    ADD COLUMN action_label_i18n jsonb;

ALTER TABLE event_recurrences
    ADD COLUMN venue_i18n jsonb;

ALTER TABLE restaurant_stories
    ADD COLUMN caption_i18n jsonb;

-- Два CHECK'а на то, что ПРОВЕРЯЕМО дёшево: перевода не может быть у поля, у
-- которого нет базового русского текста (карточка без кнопки, сторис без
-- подписи). Полный инвариант («ru равен колонке») сюда не выносится — см. выше.
-- Обе таблицы маленькие (события — сотни строк, сторис — единицы), обе колонки
-- только что созданы и во всех строках NULL, поэтому валидация ограничения не
-- находит ни одной строки для проверки и не держит блокировку заметное время.
ALTER TABLE events
    ADD CONSTRAINT events_action_label_i18n_needs_label
        CHECK (action_label_i18n IS NULL OR action_label IS NOT NULL);

ALTER TABLE restaurant_stories
    ADD CONSTRAINT restaurant_stories_caption_i18n_needs_caption
        CHECK (caption_i18n IS NULL OR caption IS NOT NULL);

COMMENT ON COLUMN promos.terms_i18n IS
  'Переводы условий акции: {"kk": …, "en": …}. Ключ ''ru'' равен колонке terms (инвариант держит usecase/promos). NULL = переводов нет, гость видит русский текст.';
COMMENT ON COLUMN events.venue_i18n IS
  'Переводы площадки события. Ключ ''ru'' равен колонке venue. Наследуется датой от правила серии вместе с venue (content_overrides = ''venue'', миграция 0097).';
COMMENT ON COLUMN events.action_label_i18n IS
  'Переводы подписи кнопки на карточке события. Ключ ''ru'' равен колонке action_label. Смысл имеет только когда кнопка есть (action_label NOT NULL).';
COMMENT ON COLUMN event_recurrences.venue_i18n IS
  'Переводы площадки в шаблоне серии. Копируются в venue_i18n каждой сгенерированной даты, если дата не ведёт поле venue сама.';
COMMENT ON COLUMN restaurant_stories.caption_i18n IS
  'Переводы подписи сторис. Ключ ''ru'' равен колонке caption; у карточки без подписи (caption IS NULL) переводов быть не может.';

-- +goose Down

-- Откат симметричный: колонки существуют только с этой миграции, никто, кроме
-- неё, их не создаёт, данных до неё в них не было. Уже записанные переводы при
-- откате ТЕРЯЮТСЯ — это осознанно: держать «висящую» колонку после отката
-- значит оставить старому коду поле, в которое он не пишет, но которое читает
-- следующий накат, и получить в нём вчерашний текст.

ALTER TABLE restaurant_stories
    DROP CONSTRAINT IF EXISTS restaurant_stories_caption_i18n_needs_caption,
    DROP COLUMN IF EXISTS caption_i18n;

ALTER TABLE event_recurrences DROP COLUMN IF EXISTS venue_i18n;

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_action_label_i18n_needs_label,
    DROP COLUMN IF EXISTS action_label_i18n,
    DROP COLUMN IF EXISTS venue_i18n;

ALTER TABLE promos DROP COLUMN IF EXISTS terms_i18n;
