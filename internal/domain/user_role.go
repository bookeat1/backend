package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// UserRoleChange is one entry in the audit of global role changes: who changed
// whose role, from what to what, and why.
//
// It exists because a role is a current state and this is its history.
// users.role answers "what may this person do now"; these rows answer "how did
// it get that way", which is the first question anybody asks after something
// goes wrong.
type UserRoleChange struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// ActorID is who made the change. Nil means the platform itself — today
	// that is the bootstrap promoting the first administrator on a system that
	// has none, where naming an actor would be a lie.
	ActorID   *uuid.UUID
	FromRole  Role
	ToRole    Role
	Reason    *string
	CreatedAt time.Time
}

// UserRoleRepository owns the global role of a user and the audit behind it.
type UserRoleRepository interface {
	// SetRole writes the new role AND the audit row in one transaction. The two
	// must not be separable: a role change with no trace is exactly the state
	// this feature exists to end.
	SetRole(ctx context.Context, change UserRoleChange) error
	// CountByRole is what makes "you cannot remove the last administrator"
	// enforceable. Counting in SQL rather than listing and measuring in Go
	// keeps the check honest under concurrency.
	CountByRole(ctx context.Context, role Role) (int, error)
	// History returns a user's role changes, newest first.
	History(ctx context.Context, userID uuid.UUID, limit int) ([]UserRoleChange, error)
	// Search finds users by a fragment of email, phone or name, for the screen
	// where an administrator picks whom to promote. An empty query lists the
	// most recently created users rather than everybody.
	Search(ctx context.Context, query string, limit int) ([]User, error)
}
