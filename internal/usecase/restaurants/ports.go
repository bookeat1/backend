// Package restaurants is the application logic for the restaurant catalog.
package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// userReader is the minimal slice of the users repository this package needs
// (verifying a manager assignee exists). Bound to the concrete user repo in deps.
type userReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}
