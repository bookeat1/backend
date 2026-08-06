package homefeed

import (
	"context"
	"fmt"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Articles implements domain.ArticleRepository.
type Articles struct{ pool sqltx.Querier }

// NewArticles builds the article repository.
func NewArticles(pool sqltx.Querier) *Articles { return &Articles{pool: pool} }

var _ domain.ArticleRepository = (*Articles)(nil)

func (r *Articles) ListActive(ctx context.Context) ([]domain.Article, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, title, author_label, cover_url, url, published_at,
			is_active, sort, created_at, updated_at
		 FROM articles WHERE is_active = true
		 ORDER BY published_at DESC NULLS LAST, sort`)
	if err != nil {
		return nil, fmt.Errorf("list articles: %w", err)
	}
	defer rows.Close()
	var out []domain.Article
	for rows.Next() {
		var a domain.Article
		if err := rows.Scan(&a.ID, &a.Title, &a.AuthorLabel, &a.CoverURL, &a.URL,
			&a.PublishedAt, &a.IsActive, &a.Sort, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
