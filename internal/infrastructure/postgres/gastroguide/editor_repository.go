// The editor (write) side of the gastroguide. It lives beside the guest read
// model in the same package because both speak to the same four tables of
// migration 0061 and share the column lists and the scanners — splitting them
// into two packages would mean maintaining two copies of collectionCols, which
// is exactly how a read and a write drift apart.
//
// The visibility rules of the guest model are NOT reused here, on purpose: the
// editor's whole job is to see drafts, archived collections and deactivated
// venues. What IS reused is the definition of "a venue a guest can open"
// (visibleVenues), because publication is checked against it — a collection is
// only allowed to go live if a guest would actually see something in it.
package gastroguide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

// EditorRepository implements domain.GastroguideEditorRepository.
type EditorRepository struct {
	pool sqltx.Querier
	tx   domain.TxManager
}

// NewEditor builds the gastroguide editor repository. It needs a TxManager of
// its own (unlike the read repository) because two of its operations —
// attaching a venue and reordering — are only correct as a unit of several
// statements.
func NewEditor(pool sqltx.Querier, tx domain.TxManager) *EditorRepository {
	return &EditorRepository{pool: pool, tx: tx}
}

var _ domain.GastroguideEditorRepository = (*EditorRepository)(nil)

const categoryCols = `cat.id, cat.slug, cat.title, cat.title_i18n, cat.position, cat.is_active,
	cat.created_at, cat.updated_at`

// --- categories ---

// ListAllCategories returns every rubric, active or not. The cabinet needs the
// inactive ones: a rubric is switched off, not deleted, and an editor who cannot
// see it cannot switch it back on.
func (r *EditorRepository) ListAllCategories(ctx context.Context) ([]domain.GuideCategory, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+categoryCols+` FROM gastroguide_categories cat ORDER BY cat.position, cat.id`)
	if err != nil {
		return nil, fmt.Errorf("list all guide categories: %w", err)
	}
	defer rows.Close()

	out := make([]domain.GuideCategory, 0)
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan guide category: %w", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guide categories: %w", err)
	}
	return out, nil
}

// GetCategory returns one rubric of any state. The editor reads it before an
// update because the translation patch it is about to apply is partial.
func (r *EditorRepository) GetCategory(ctx context.Context, id uuid.UUID) (*domain.GuideCategory, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+categoryCols+` FROM gastroguide_categories cat WHERE cat.id = $1`, id)
	c, err := scanCategory(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get guide category: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get guide category: %w", err)
	}
	return c, nil
}

// CreateCategory inserts a rubric.
func (r *EditorRepository) CreateCategory(ctx context.Context, in domain.GuideCategoryWrite) (*domain.GuideCategory, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO gastroguide_categories (id, slug, title, title_i18n, position, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+strings.ReplaceAll(categoryCols, "cat.", ""),
		uuid.New(), in.Slug, in.Title, i18nToDB(in.TitleI18n), in.Position, in.IsActive)
	c, err := scanCategory(row)
	if err != nil {
		return nil, mapSlugConflict("create guide category", err)
	}
	return c, nil
}

// UpdateCategory replaces a rubric's fields.
func (r *EditorRepository) UpdateCategory(ctx context.Context, id uuid.UUID, in domain.GuideCategoryWrite) (*domain.GuideCategory, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`UPDATE gastroguide_categories
		 SET slug = $2, title = $3, title_i18n = $4, position = $5, is_active = $6, updated_at = now()
		 WHERE id = $1
		 RETURNING `+strings.ReplaceAll(categoryCols, "cat.", ""),
		id, in.Slug, in.Title, i18nToDB(in.TitleI18n), in.Position, in.IsActive)
	c, err := scanCategory(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("update guide category: %w", domain.ErrNotFound)
		}
		return nil, mapSlugConflict("update guide category", err)
	}
	return c, nil
}

// --- collections ---

// ListCollectionsAdmin returns collections of any status in editorial order.
func (r *EditorRepository) ListCollectionsAdmin(ctx context.Context, f domain.GuideCollectionAdminFilter) ([]domain.GuideCollection, int, error) {
	page, perPage := normalizePage(f.Page, f.PerPage)

	args := []any{}
	where := []string{"TRUE"}
	if len(f.Statuses) > 0 {
		statuses := make([]string, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			statuses = append(statuses, string(s))
		}
		args = append(args, statuses)
		where = append(where, `c.status = ANY($`+strconv.Itoa(len(args))+`::varchar[])`)
	}
	if f.City != nil {
		args = append(args, string(*f.City))
		where = append(where, `c.city = $`+strconv.Itoa(len(args)))
	}
	if f.Kind != nil {
		args = append(args, string(*f.Kind))
		where = append(where, `c.kind = $`+strconv.Itoa(len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		n := strconv.Itoa(len(args))
		where = append(where, `(c.slug ILIKE $`+n+` OR c.title ILIKE $`+n+`)`)
	}
	from := ` FROM gastroguide_collections c WHERE ` + strings.Join(where, " AND ")

	q := sqltx.From(ctx, r.pool)
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admin guide collections: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// collectionCols carries no placeholder of its own (the `now` parameter of
	// the guest queries belongs to the liveCollection predicate, which is
	// deliberately absent here), so the shared column list is reused verbatim
	// and the numbering below is only about the filters.
	args = append(args, perPage, (page-1)*perPage)
	rows, err := q.Query(ctx,
		`SELECT `+collectionCols+from+`
		 ORDER BY c.position, c.id
		 LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin guide collections: %w", err)
	}
	defer rows.Close()

	var items []domain.GuideCollection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan admin guide collection: %w", err)
		}
		items = append(items, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate admin guide collections: %w", err)
	}
	return items, total, nil
}

// GetCollectionAdmin returns one collection of any status with EVERY attached
// venue (deactivated ones included, flagged) and its rubrics.
func (r *EditorRepository) GetCollectionAdmin(ctx context.Context, id uuid.UUID) (*domain.GuideCollectionAdminDetail, error) {
	q := sqltx.From(ctx, r.pool)
	c, err := scanCollection(q.QueryRow(ctx,
		`SELECT `+collectionCols+` FROM gastroguide_collections c WHERE c.id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get admin guide collection: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get admin guide collection: %w", err)
	}

	venues, err := r.listAllVenues(ctx, id)
	if err != nil {
		return nil, err
	}
	cats, err := r.listCollectionCategories(ctx, id)
	if err != nil {
		return nil, err
	}
	return &domain.GuideCollectionAdminDetail{GuideCollection: *c, Venues: venues, Categories: cats}, nil
}

// CreateCollection inserts a collection. It is always a draft: publication has
// its own preconditions (usecase), and a create that could go straight live
// would let an editor publish an empty collection in one call.
func (r *EditorRepository) CreateCollection(ctx context.Context, in domain.GuideCollectionWrite) (*domain.GuideCollection, error) {
	id := uuid.New()
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO gastroguide_collections
			(id, slug, title, title_i18n, subtitle, subtitle_i18n, description, description_i18n,
			 cover_image_url, city, kind, status, position)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'draft', $12)`,
		id, in.Slug, in.Title, i18nToDB(in.TitleI18n), in.Subtitle, i18nToDB(in.SubtitleI18n),
		in.Description, i18nToDB(in.DescriptionI18n), in.CoverImageURL, cityArg(in.City),
		string(in.Kind), in.Position)
	if err != nil {
		return nil, mapSlugConflict("create guide collection", err)
	}
	return r.getCollection(ctx, id)
}

// UpdateCollection replaces the editable fields and leaves status/published_at
// untouched — a typo fix must never change what a guest can see.
func (r *EditorRepository) UpdateCollection(ctx context.Context, id uuid.UUID, in domain.GuideCollectionWrite) (*domain.GuideCollection, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastroguide_collections
		 SET slug = $2, title = $3, title_i18n = $4, subtitle = $5, subtitle_i18n = $6,
			 description = $7, description_i18n = $8, cover_image_url = $9, city = $10,
			 kind = $11, position = $12, updated_at = now()
		 WHERE id = $1`,
		id, in.Slug, in.Title, i18nToDB(in.TitleI18n), in.Subtitle, i18nToDB(in.SubtitleI18n),
		in.Description, i18nToDB(in.DescriptionI18n), in.CoverImageURL, cityArg(in.City),
		string(in.Kind), in.Position)
	if err != nil {
		return nil, mapSlugConflict("update guide collection", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("update guide collection: %w", domain.ErrNotFound)
	}
	return r.getCollection(ctx, id)
}

// SetCollectionStatus moves a collection between publication states. The DB
// CHECK (published ⇒ published_at IS NOT NULL) is the backstop; the usecase
// supplies the time so a violation here would be our bug, not an editor's.
func (r *EditorRepository) SetCollectionStatus(ctx context.Context, id uuid.UUID, status domain.GuideCollectionStatus, publishedAt *time.Time) (*domain.GuideCollection, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastroguide_collections
		 SET status = $2, published_at = $3, updated_at = now()
		 WHERE id = $1`, id, string(status), publishedAt)
	if err != nil {
		return nil, fmt.Errorf("set guide collection status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("set guide collection status: %w", domain.ErrNotFound)
	}
	return r.getCollection(ctx, id)
}

// CountActiveVenues counts the venues of the collection a guest could open right
// now, using the SAME predicate the guest listing uses.
func (r *EditorRepository) CountActiveVenues(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM gastroguide_collection_venues cv
		 JOIN restaurants r ON r.id = cv.restaurant_id
		 WHERE cv.collection_id = $1 AND r.is_active`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active guide collection venues: %w", err)
	}
	return n, nil
}

// --- collection ↔ rubric ---

// SetCollectionCategories replaces the whole rubric set in one transaction:
// delete-then-insert, so "remove from Завтраки and add to Ужины" is one atomic
// edit and never leaves a collection briefly in neither.
func (r *EditorRepository) SetCollectionCategories(ctx context.Context, collectionID uuid.UUID, categoryIDs []uuid.UUID) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := sqltx.From(ctx, r.pool)
		if err := r.assertCollectionExists(ctx, collectionID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx,
			`DELETE FROM gastroguide_collection_categories WHERE collection_id = $1`, collectionID); err != nil {
			return fmt.Errorf("clear guide collection categories: %w", err)
		}
		if len(categoryIDs) == 0 {
			return nil
		}
		positions := make([]int32, len(categoryIDs))
		for i := range categoryIDs {
			positions[i] = int32(i + 1)
		}
		_, err := q.Exec(ctx,
			`INSERT INTO gastroguide_collection_categories (collection_id, category_id, position)
			 SELECT $1, t.cat_id, t.pos FROM unnest($2::uuid[], $3::int[]) AS t(cat_id, pos)`,
			collectionID, categoryIDs, positions)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
				return fmt.Errorf("set guide collection categories: %w: unknown category", domain.ErrNotFound)
			}
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return fmt.Errorf("set guide collection categories: %w: a category was listed twice", domain.ErrValidation)
			}
			return fmt.Errorf("set guide collection categories: %w", err)
		}
		return nil
	})
}

// --- collection ↔ venue ---

// AttachVenue appends a venue after the last one.
//
// It runs inside a transaction that first takes a row lock on the COLLECTION.
// Without it two editors appending at the same moment both read the same
// max(position) and both write it, and the second commit dies on the unique
// constraint with a 500-shaped error. The lock makes membership changes on one
// collection serial, which costs nothing (a collection is edited by one person
// at a time) and removes the race entirely.
func (r *EditorRepository) AttachVenue(ctx context.Context, collectionID uuid.UUID, in domain.GuideVenueAttachment) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := sqltx.From(ctx, r.pool)
		if err := r.lockCollection(ctx, collectionID); err != nil {
			return err
		}
		var next int
		if err := q.QueryRow(ctx,
			`SELECT COALESCE(max(position), 0) + 1 FROM gastroguide_collection_venues
			 WHERE collection_id = $1`, collectionID).Scan(&next); err != nil {
			return fmt.Errorf("next guide venue position: %w", err)
		}
		_, err := q.Exec(ctx,
			`INSERT INTO gastroguide_collection_venues
				(collection_id, restaurant_id, position, note, note_i18n, event_id, promo_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			collectionID, in.RestaurantID, next, in.Note, i18nToDB(in.NoteI18n),
			in.EventID, in.PromoID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				switch pgErr.Code {
				case uniqueViolation:
					return domain.WithCode(domain.CodeGuideVenueAlreadyAttached,
						fmt.Errorf("attach guide venue: %w: already in this collection", domain.ErrAlreadyExists))
				case foreignKeyViolation:
					return fmt.Errorf("attach guide venue: %w: unknown restaurant", domain.ErrNotFound)
				}
			}
			return fmt.Errorf("attach guide venue: %w", err)
		}
		return nil
	})
}

// DetachVenue removes a venue and CLOSES THE GAP: every venue after it moves up
// one. Leaving a hole would be harmless for the guest (the read only sorts) but
// not for the editor — the next attach computes max+1, so after a few
// detach/attach cycles the numbers drift far away from the visible order and the
// reorder payloads become impossible to reason about in a bug report.
//
// The renumbering and the delete are one transaction; the shift cannot collide
// with the deleted row because it is already gone, and the deferrable unique
// constraint tolerates the intermediate states anyway.
func (r *EditorRepository) DetachVenue(ctx context.Context, collectionID, restaurantID uuid.UUID) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := sqltx.From(ctx, r.pool)
		if err := r.lockCollection(ctx, collectionID); err != nil {
			return err
		}
		var pos int
		err := q.QueryRow(ctx,
			`DELETE FROM gastroguide_collection_venues
			 WHERE collection_id = $1 AND restaurant_id = $2
			 RETURNING position`, collectionID, restaurantID).Scan(&pos)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("detach guide venue: %w", domain.ErrNotFound)
			}
			return fmt.Errorf("detach guide venue: %w", err)
		}
		if _, err := q.Exec(ctx,
			`UPDATE gastroguide_collection_venues SET position = position - 1
			 WHERE collection_id = $1 AND position > $2`, collectionID, pos); err != nil {
			return fmt.Errorf("close guide venue position gap: %w", err)
		}
		return nil
	})
}

// UpdateVenueNote rewrites the editor's line under one venue's card.
func (r *EditorRepository) UpdateVenueNote(ctx context.Context, collectionID, restaurantID uuid.UUID, note string, noteI18n domain.I18n) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastroguide_collection_venues SET note = $3, note_i18n = $4
		 WHERE collection_id = $1 AND restaurant_id = $2`,
		collectionID, restaurantID, note, i18nToDB(noteI18n))
	if err != nil {
		return fmt.Errorf("update guide venue note: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update guide venue note: %w", domain.ErrNotFound)
	}
	return nil
}

// ReorderVenues writes a whole new ordering at once.
//
// The contract is "send the intended FINAL order", not a sequence of swaps, and
// the two halves of that are enforced here:
//
//  1. The payload must name EXACTLY the current members, each once. A missing,
//     extra or repeated id means the editor's screen is out of date, and
//     guessing what they meant (append the missing ones? drop the strangers?)
//     would silently rewrite a curation. It is CodeGuideOrderMismatch and
//     nothing is written.
//
//  2. All the new numbers are written by ONE statement inside ONE transaction,
//     which is what the DEFERRABLE INITIALLY DEFERRED unique (collection_id,
//     position) buys: a rotation passes through states where two rows share a
//     number, and with an immediate constraint that update would be rejected
//     row-by-row. Deferred, the constraint is checked once at COMMIT, so either
//     the entire new ordering lands or none of it does. There is no half-applied
//     order and no window in which the guest read sees duplicates.
//
// The collection row is locked first so a concurrent attach cannot slip a new
// venue in between the membership check and the write.
func (r *EditorRepository) ReorderVenues(ctx context.Context, collectionID uuid.UUID, restaurantIDs []uuid.UUID) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.lockCollection(ctx, collectionID); err != nil {
			return err
		}
		current, err := r.ListCollectionVenueIDs(ctx, collectionID)
		if err != nil {
			return err
		}
		if err := sameSet(current, restaurantIDs); err != nil {
			return domain.WithCode(domain.CodeGuideOrderMismatch,
				fmt.Errorf("reorder guide venues: %w: %s", domain.ErrValidation, err.Error()))
		}
		if len(restaurantIDs) == 0 {
			return nil
		}
		positions := make([]int32, len(restaurantIDs))
		for i := range restaurantIDs {
			positions[i] = int32(i + 1)
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`UPDATE gastroguide_collection_venues cv
			 SET position = t.pos
			 FROM unnest($2::uuid[], $3::int[]) AS t(rid, pos)
			 WHERE cv.collection_id = $1 AND cv.restaurant_id = t.rid`,
			collectionID, restaurantIDs, positions); err != nil {
			return fmt.Errorf("reorder guide venues: %w", err)
		}
		return nil
	})
}

// ListCollectionVenueIDs returns the members in editorial order.
func (r *EditorRepository) ListCollectionVenueIDs(ctx context.Context, collectionID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT restaurant_id FROM gastroguide_collection_venues
		 WHERE collection_id = $1 ORDER BY position, restaurant_id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list guide collection venue ids: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan guide collection venue id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guide collection venue ids: %w", err)
	}
	return out, nil
}

// --- helpers ---

// sameSet reports why want is not exactly have (order ignored). It is a plain
// multiset comparison and returns a message an editor can act on, not just
// "invalid".
func sameSet(have, want []uuid.UUID) error {
	if len(have) != len(want) {
		return fmt.Errorf("the order lists %d venues, the collection holds %d", len(want), len(have))
	}
	seen := make(map[uuid.UUID]bool, len(have))
	for _, id := range have {
		seen[id] = true
	}
	used := make(map[uuid.UUID]bool, len(want))
	for _, id := range want {
		if used[id] {
			return fmt.Errorf("venue %s is listed twice", id)
		}
		if !seen[id] {
			return fmt.Errorf("venue %s is not in this collection", id)
		}
		used[id] = true
	}
	return nil
}

// lockCollection takes a row lock on the collection and doubles as its existence
// check, so a membership write against an unknown collection is ErrNotFound
// rather than a foreign-key 500.
func (r *EditorRepository) lockCollection(ctx context.Context, id uuid.UUID) error {
	var found uuid.UUID
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT id FROM gastroguide_collections WHERE id = $1 FOR UPDATE`, id).Scan(&found)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("guide collection: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("lock guide collection: %w", err)
	}
	return nil
}

func (r *EditorRepository) assertCollectionExists(ctx context.Context, id uuid.UUID) error {
	return r.lockCollection(ctx, id)
}

func (r *EditorRepository) getCollection(ctx context.Context, id uuid.UUID) (*domain.GuideCollection, error) {
	c, err := scanCollection(sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+collectionCols+` FROM gastroguide_collections c WHERE c.id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read guide collection: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("read guide collection: %w", err)
	}
	return c, nil
}

// listAllVenues is the editor's twin of listVenues: same columns and same order,
// WITHOUT the rest.is_active filter. IsActive comes back on every row so the
// cabinet can mark the dark ones.
func (r *EditorRepository) listAllVenues(ctx context.Context, collectionID uuid.UUID) ([]domain.GuideCollectionVenue, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT cv.restaurant_id, cv.position, cv.note, cv.note_i18n,
			rest.name, rest.name_i18n, rest.address, rest.address_i18n,
			rest.cuisine_type, rest.cuisine_type_i18n, rest.city, rest.price_category,
			img.image_url, rest.is_active
		 FROM gastroguide_collection_venues cv
		 JOIN restaurants rest ON rest.id = cv.restaurant_id
		 LEFT JOIN LATERAL (
			SELECT ri.image_url FROM restaurant_images ri
			WHERE ri.restaurant_id = rest.id
			ORDER BY ri.is_primary DESC, ri.created_at, ri.id
			LIMIT 1
		 ) img ON true
		 WHERE cv.collection_id = $1
		 ORDER BY cv.position, cv.restaurant_id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list admin guide collection venues: %w", err)
	}
	defer rows.Close()

	out := make([]domain.GuideCollectionVenue, 0)
	for rows.Next() {
		var v domain.GuideCollectionVenue
		var noteI18n, nameI18n, addrI18n, cuisineI18n []byte
		if err := rows.Scan(&v.RestaurantID, &v.Position, &v.Note, &noteI18n,
			&v.Name, &nameI18n, &v.Address, &addrI18n,
			&v.CuisineType, &cuisineI18n, &v.City, &v.PriceCategory, &v.PrimaryImageURL,
			&v.IsActive); err != nil {
			return nil, fmt.Errorf("scan admin guide collection venue: %w", err)
		}
		v.NoteI18n = i18nFromDB(noteI18n)
		v.NameI18n = i18nFromDB(nameI18n)
		v.AddressI18n = i18nFromDB(addrI18n)
		v.CuisineTypeI18n = i18nFromDB(cuisineI18n)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin guide collection venues: %w", err)
	}
	return out, nil
}

// listCollectionCategories returns the rubrics of one collection in the order
// they hold INSIDE it, inactive ones included — same reason as the venues.
func (r *EditorRepository) listCollectionCategories(ctx context.Context, collectionID uuid.UUID) ([]domain.GuideCategory, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+categoryCols+`
		 FROM gastroguide_collection_categories cc
		 JOIN gastroguide_categories cat ON cat.id = cc.category_id
		 WHERE cc.collection_id = $1
		 ORDER BY cc.position, cat.id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list guide collection categories: %w", err)
	}
	defer rows.Close()
	out := make([]domain.GuideCategory, 0)
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan guide collection category: %w", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guide collection categories: %w", err)
	}
	return out, nil
}

func scanCategory(row pgx.Row) (*domain.GuideCategory, error) {
	var c domain.GuideCategory
	var titleI18n []byte
	if err := row.Scan(&c.ID, &c.Slug, &c.Title, &titleI18n, &c.Position, &c.IsActive,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.TitleI18n = i18nFromDB(titleI18n)
	return &c, nil
}

// mapSlugConflict turns the unique(slug) violation into the machine-readable
// refusal an editor can act on ("pick another slug") instead of a 500.
func mapSlugConflict(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return domain.WithCode(domain.CodeGuideSlugTaken,
			fmt.Errorf("%s: %w: slug is already taken", op, domain.ErrAlreadyExists))
	}
	return fmt.Errorf("%s: %w", op, err)
}

// i18nToDB writes an empty translation map as SQL NULL rather than '{}': the
// guest read treats both the same, and NULL is what every existing *_i18n
// column holds when nothing was translated.
func i18nToDB(m domain.I18n) any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// SetVenueHighlight ставит (или снимает, когда оба аргумента nil) событие либо
// акцию у уже добавленного в подборку заведения. Отсутствующая связка — это
// ErrNotFound, а не молчаливый ноль строк: редактор должен узнать, что правил
// блок, которого в подборке уже нет.
func (r *EditorRepository) SetVenueHighlight(ctx context.Context, collectionID, restaurantID uuid.UUID, eventID, promoID *uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastroguide_collection_venues
		 SET event_id = $3, promo_id = $4
		 WHERE collection_id = $1 AND restaurant_id = $2`,
		collectionID, restaurantID, eventID, promoID)
	if err != nil {
		return fmt.Errorf("set guide venue highlight: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set guide venue highlight: %w", domain.ErrNotFound)
	}
	return nil
}
