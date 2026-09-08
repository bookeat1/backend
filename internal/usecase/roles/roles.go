// Package roles owns who may change a global role, and the rules that keep a
// platform from locking itself out.
package roles

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the caller. Only a platform administrator may change roles, and the
// check lives here as well as in the router: the router says "an admin reached
// this handler", this package says "an admin may do this particular thing".
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// UseCase changes global roles and reads their history.
type UseCase struct {
	repo  domain.UserRoleRepository
	users domain.UserRepository
}

// NewUseCase wires the role repository and the user reader.
func NewUseCase(repo domain.UserRoleRepository, users domain.UserRepository) *UseCase {
	return &UseCase{repo: repo, users: users}
}

const maxSearch = 50

// SetRole changes one user's global role.
//
// Four rules, each of which exists because of a way this goes wrong:
//
//  1. only an administrator may call it at all;
//  2. the target role must be one the system knows — a typo must not create a
//     user with a role nothing recognises, which would silently deny them
//     everything;
//  3. an administrator may not demote THEMSELVES, because the usual way to do
//     that by accident is to demote the account you are signed in as and then
//     have nobody left who can undo it;
//  4. the LAST administrator may not be demoted by anyone, for the same reason
//     the venue code refuses to remove a venue's last owner: a platform with
//     zero administrators can only be repaired from the database again, which
//     is the state this feature exists to leave behind.
func (u *UseCase) SetRole(ctx context.Context, actor Actor, targetID uuid.UUID, to domain.Role, reason *string) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only a platform administrator may change roles", domain.ErrForbidden)
	}
	if !to.Valid() {
		return domain.WithCode(domain.CodeValidation,
			fmt.Errorf("%w: unknown role %q", domain.ErrValidation, to))
	}

	target, err := u.users.GetByID(ctx, targetID)
	if err != nil {
		return err
	}
	if target.Role == to {
		// Already there. Not an error and NOT an audit row: recording a change
		// that did not happen would make the history lie.
		return nil
	}

	if target.ID == actor.UserID && to != domain.RoleAdmin {
		return domain.WithCode(domain.CodeForbidden,
			fmt.Errorf("%w: you cannot take your own administrator rights away", domain.ErrForbidden))
	}

	if target.Role == domain.RoleAdmin && to != domain.RoleAdmin {
		admins, err := u.repo.CountByRole(ctx, domain.RoleAdmin)
		if err != nil {
			return err
		}
		if admins <= 1 {
			return domain.WithCode(domain.CodeForbidden,
				fmt.Errorf("%w: this is the last administrator; promote somebody else first", domain.ErrForbidden))
		}
	}

	actorID := actor.UserID
	return u.repo.SetRole(ctx, domain.UserRoleChange{
		UserID:   target.ID,
		ActorID:  &actorID,
		FromRole: target.Role,
		ToRole:   to,
		Reason:   trimmed(reason),
	})
}

// History returns a user's role changes, newest first. Administrators only:
// knowing who granted whom what is itself sensitive.
func (u *UseCase) History(ctx context.Context, actor Actor, userID uuid.UUID, limit int) ([]domain.UserRoleChange, error) {
	if actor.Role != domain.RoleAdmin {
		return nil, fmt.Errorf("%w: only a platform administrator may read role history", domain.ErrForbidden)
	}
	return u.repo.History(ctx, userID, clamp(limit))
}

// Search finds candidates to promote.
func (u *UseCase) Search(ctx context.Context, actor Actor, query string, limit int) ([]domain.User, error) {
	if actor.Role != domain.RoleAdmin {
		return nil, fmt.Errorf("%w: only a platform administrator may search users", domain.ErrForbidden)
	}
	return u.repo.Search(ctx, strings.TrimSpace(query), clamp(limit))
}

// EnsureBootstrapAdmin promotes the owner's account on a platform that has no
// administrator at all.
//
// This is the answer to "where does the FIRST administrator come from". Without
// it every fresh deployment starts with nobody able to grant anything, and the
// only fix is an UPDATE typed on the server — the exact thing this package
// replaces.
//
// It is deliberately narrow: it does nothing when an administrator already
// exists, so it cannot be used to quietly re-grant rights somebody removed on
// purpose, and it does nothing when the email is unset or unknown.
func (u *UseCase) EnsureBootstrapAdmin(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	admins, err := u.repo.CountByRole(ctx, domain.RoleAdmin)
	if err != nil {
		return err
	}
	if admins > 0 {
		return nil
	}
	target, err := u.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The owner has not signed up yet. Not an error: the next start
			// will find them.
			return nil
		}
		return err
	}
	if target.Role == domain.RoleAdmin {
		return nil
	}
	reason := "первый администратор платформы, назначен при развёртывании"
	return u.repo.SetRole(ctx, domain.UserRoleChange{
		UserID:   target.ID,
		ActorID:  nil, // the platform itself; naming a person here would be a lie
		FromRole: target.Role,
		ToRole:   domain.RoleAdmin,
		Reason:   &reason,
	})
}

func clamp(limit int) int {
	if limit <= 0 || limit > maxSearch {
		return maxSearch
	}
	return limit
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}
