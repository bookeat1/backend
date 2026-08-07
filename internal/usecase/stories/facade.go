package stories

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes restaurant story reads. It is a read-only surface for the guest
// app today (the venue cabinet that curates stories is a separate task).
type Facade interface {
	// List returns the active stories of restaurantID in display order.
	List(ctx context.Context, restaurantID uuid.UUID) ([]domain.Story, error)
}

type facade struct {
	stories domain.StoryRepository
}

// NewFacade constructs the stories Facade.
func NewFacade(stories domain.StoryRepository) Facade {
	return &facade{stories: stories}
}

func (f *facade) List(ctx context.Context, restaurantID uuid.UUID) ([]domain.Story, error) {
	return f.stories.ListActiveByRestaurant(ctx, restaurantID)
}
