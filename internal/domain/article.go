package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Article is an editorial card shown in the mobile Home ("Explore") screen's
// "Статьи" block. URL points to the external/deep-linked content; the app does
// not render article bodies.
type Article struct {
	ID          uuid.UUID
	Title       string
	AuthorLabel *string
	CoverURL    *string
	URL         *string
	PublishedAt *time.Time
	IsActive    bool
	Sort        int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ArticleRepository reads the articles catalog.
type ArticleRepository interface {
	// ListActive returns active articles ordered by published_at (most recent
	// first, NULLs last), then sort.
	ListActive(ctx context.Context) ([]Article, error)
}
