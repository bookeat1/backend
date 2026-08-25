// Package cities is the city dictionary usecase: the platform-owned reference
// list (create / rename / hide / reorder — superadmin only) and the resolution
// of a written city spelling into a dictionary entry.
//
// The rule the whole package exists to enforce (ADR-023): the city belongs to
// OUR panel. The legacy system stopped owning it, the two constants in
// internal/domain stopped being the list, and nobody types a city by hand into
// a venue any more — a venue points at an entry.
package cities

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. Only domain.RoleAdmin may touch the
// dictionary; reads are anonymous (the app's city chips are a public screen).
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// venueCityWriter rewrites the DERIVED backward-compatibility column
// `restaurants.city` after a dictionary entry is renamed.
//
// A narrow port rather than a full RestaurantRepository dependency on purpose:
// this package must be able to write that ONE column and nothing else, so it
// can never revert a venue's other fields with a stale read-modify-write.
type venueCityWriter interface {
	RenameCityString(ctx context.Context, cityID uuid.UUID, name string) (int64, error)
}

// UseCase is the city dictionary API surface.
type UseCase interface {
	// List returns the dictionary. includeInactive is honoured ONLY for a
	// superadmin — a guest asking for hidden entries gets the active list,
	// not a 403, because this route is called anonymously.
	List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.CityEntry, error)
	Create(ctx context.Context, actor Actor, in SaveInput) (*domain.CityEntry, error)
	// Update renames / retranslates / reorders / hides an entry, and rewrites
	// the venues' derived city string in the SAME transaction when the name
	// changed.
	Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.CityEntry, error)
	// SetActive hides (false) or restores (true) an entry. There is no hard
	// delete: venues reference the city and carry its name as a live string.
	SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.CityEntry, error)
	// Reorder sets the display order to the given id sequence.
	Reorder(ctx context.Context, actor Actor, ids []uuid.UUID) ([]domain.CityEntry, error)
	// AddAlias registers an extra spelling that must resolve to this city and
	// returns the entry, so the caller sees which city it landed on.
	AddAlias(ctx context.Context, actor Actor, id uuid.UUID, alias string) (*domain.CityEntry, error)

	// Resolve turns a written city value — a Russian name from an old build, a
	// code from a new one, a historical spelling — into a dictionary entry.
	// Returns nil WITHOUT an error when nothing matches: an unknown city is a
	// normal answer, and the caller decides what it means (the catalog filter
	// keeps the raw string and finds nothing, which is the pre-existing
	// behaviour).
	Resolve(ctx context.Context, raw string) (*domain.CityEntry, error)
}

type useCase struct {
	repo   domain.CityRepository
	venues venueCityWriter
	tx     domain.TxManager
}

// NewUseCase constructs the city usecase.
func NewUseCase(repo domain.CityRepository, venues venueCityWriter, tx domain.TxManager) UseCase {
	return &useCase{repo: repo, venues: venues, tx: tx}
}

// SaveInput carries the mutable dictionary fields. Every field is a pointer so
// Update can distinguish "absent from the request" (preserve) from "explicitly
// provided" — the same PATCH convention the restaurants and cuisines facades
// use.
type SaveInput struct {
	Code         *string
	Name         *string
	NameI18n     domain.I18n
	DisplayOrder *int
	IsActive     *bool
}

func (u *useCase) List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.CityEntry, error) {
	return u.repo.List(ctx, domain.CityFilter{
		IncludeInactive: includeInactive && actor.Role == domain.RoleAdmin,
	})
}

// requirePlatform is the single gate on every dictionary mutation. The
// transport already mounts these routes behind RequireRole(RoleAdmin); this is
// the defense-in-depth re-check, so a future re-mount on a wider group cannot
// silently hand the dictionary to venue staff.
func requirePlatform(actor Actor) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only the platform may manage the city dictionary", domain.ErrForbidden)
	}
	return nil
}

func (u *useCase) Create(ctx context.Context, actor Actor, in SaveInput) (*domain.CityEntry, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	c := &domain.CityEntry{ID: uuid.New(), IsActive: true}
	if err := applyInput(c, in); err != nil {
		return nil, err
	}
	if c.Code == "" || c.Name == "" {
		return nil, fmt.Errorf("%w: code and name are required", domain.ErrValidation)
	}
	if err := u.repo.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Update is the rename path, and the reason it runs in a transaction.
//
// A rename touches two places: the dictionary row and the derived string on
// every venue in that city. If only the first landed, the catalog would list
// the city under its new name while its venues still said the old one — and
// the old build's ?city= filter would match neither. Both halves land together
// or neither does.
func (u *useCase) Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.CityEntry, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}

	var out *domain.CityEntry
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		c, err := u.repo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		before := c.Name
		if err := applyInput(c, in); err != nil {
			return err
		}
		if c.Code == "" || c.Name == "" {
			return fmt.Errorf("%w: code and name must not be empty", domain.ErrValidation)
		}
		// Update also (re)registers the new name as an alias. It MUST run
		// before the venues are rewritten: the database trigger re-resolves
		// city_id from the written string, and a string with no alias yet
		// would null the link out.
		if err := u.repo.Update(ctx, c); err != nil {
			return err
		}
		if c.Name != before {
			n, err := u.venues.RenameCityString(ctx, c.ID, c.Name)
			if err != nil {
				return err
			}
			slog.Info("city renamed", "city_id", c.ID, "from", before, "to", c.Name, "venues_updated", n)
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (u *useCase) SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.CityEntry, error) {
	return u.Update(ctx, actor, id, SaveInput{IsActive: &active})
}

func (u *useCase) Reorder(ctx context.Context, actor Actor, ids []uuid.UUID) ([]domain.CityEntry, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	if err := u.repo.Reorder(ctx, dedupe(ids)); err != nil {
		return nil, err
	}
	// Deliberately the FULL list including hidden entries: reordering is done
	// on the admin screen, which shows them, and answering with a shorter list
	// than the caller sent would read as "some ids were dropped".
	return u.repo.List(ctx, domain.CityFilter{IncludeInactive: true})
}

func (u *useCase) AddAlias(ctx context.Context, actor Actor, id uuid.UUID, alias string) (*domain.CityEntry, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	c, err := u.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := u.repo.AddAlias(ctx, id, alias); err != nil {
		return nil, err
	}
	return c, nil
}

func (u *useCase) Resolve(ctx context.Context, raw string) (*domain.CityEntry, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	c, err := u.repo.ResolveAlias(ctx, raw)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func applyInput(c *domain.CityEntry, in SaveInput) error {
	if in.Code != nil {
		code := strings.ToLower(strings.TrimSpace(*in.Code))
		if err := validateCode(code); err != nil {
			return err
		}
		c.Code = code
	}
	if in.Name != nil {
		// Collapsed exactly the way domain.NormalizeCityKey and the SQL
		// city_key() collapse it, so the stored name and its alias key can
		// never disagree about whitespace.
		c.Name = strings.Join(strings.Fields(*in.Name), " ")
	}
	if in.NameI18n != nil {
		c.NameI18n = in.NameI18n
	}
	if in.DisplayOrder != nil {
		c.DisplayOrder = *in.DisplayOrder
	}
	if in.IsActive != nil {
		c.IsActive = *in.IsActive
	}
	return nil
}

// validateCode keeps Code a stable machine key: lowercase latin, digits and
// underscores only. It travels in query strings (?city=almaty) and clients key
// local assets off it, so anything else would break either the URL or the
// lookup.
func validateCode(code string) error {
	if code == "" || len(code) > 64 {
		return fmt.Errorf("%w: code must be 1..64 characters", domain.ErrValidation)
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf("%w: code may contain only a-z, 0-9 and _", domain.ErrValidation)
		}
	}
	return nil
}

// dedupe keeps first-seen order: a repeated id would otherwise decide its own
// position twice.
func dedupe(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
