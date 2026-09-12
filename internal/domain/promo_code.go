package domain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// PromoCodeStatus is a promo code's lifecycle state, stored as VARCHAR and
// validated here (never a Postgres ENUM — schema rule, migrations/embed.go).
// It is deliberately NOT the same thing as the linked promo's PromoStatus:
// the campaign card may keep hanging on the storefront while the code stops
// being accepted, and the other way round. Two objects, two lifecycles.
type PromoCodeStatus string

const (
	// PromoCodeDraft is a code being prepared in the cabinet. Never accepted
	// from a guest.
	PromoCodeDraft PromoCodeStatus = "draft"
	// PromoCodeActive is the only state in which a guest may redeem the code
	// (and only inside [StartsAt, ExpiresAt) with a live promo behind it).
	PromoCodeActive PromoCodeStatus = "active"
	// PromoCodePaused is "we stopped accepting it" — reversible, and it says
	// nothing about the promo's own visibility.
	PromoCodePaused PromoCodeStatus = "paused"
	// PromoCodeArchived is terminal: the code stays in booking history, but
	// nothing brings it back.
	PromoCodeArchived PromoCodeStatus = "archived"
)

// PromoCodeStatuses lists every valid status, in lifecycle order.
var PromoCodeStatuses = []PromoCodeStatus{
	PromoCodeDraft, PromoCodeActive, PromoCodePaused, PromoCodeArchived,
}

// Valid reports whether s is a known status.
func (s PromoCodeStatus) Valid() bool {
	switch s {
	case PromoCodeDraft, PromoCodeActive, PromoCodePaused, PromoCodeArchived:
		return true
	}
	return false
}

// CanTransitionTo reports whether the cabinet may move a code from s to next:
//
//	draft ──► active ◄──► paused
//	  │         │           │
//	  └─────────┴───► archived (terminal)
//
// Keeping the status where it already is is allowed for every valid status,
// archived included: the admin screen sends a PATCH with the whole status
// field, and re-sending the current value is a no-op, not an attempt to leave
// a terminal state. An UNKNOWN status on either side is always refused.
func (s PromoCodeStatus) CanTransitionTo(next PromoCodeStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true
	}
	switch s {
	case PromoCodeDraft:
		// Straight to paused is refused on purpose: "prepared but not yet
		// switched on" is what draft already means, so draft→paused would be
		// a second spelling of the same state.
		return next == PromoCodeActive || next == PromoCodeArchived
	case PromoCodeActive:
		return next == PromoCodePaused || next == PromoCodeArchived
	case PromoCodePaused:
		return next == PromoCodeActive || next == PromoCodeArchived
	case PromoCodeArchived:
		return false
	}
	return false
}

// PromoCodeMaxLen is the code column's width (migration 0108).
const PromoCodeMaxLen = 32

// PromoCodeMinLen is the shortest code a human can be asked to type without
// collisions becoming likely.
const PromoCodeMinLen = 3

// promoCodeRe is the shape of a NORMALIZED code and mirrors the CHECK
// constraint in migration 0108. Validation runs against the normalized form
// only — see NormalizePromoCode.
var promoCodeRe = regexp.MustCompile(`^[A-Z0-9]{3,32}$`)

// NormalizePromoCode folds every spelling a guest may type of the same code
// into the single form that is stored and compared: upper case, with spaces
// and dashes removed. "marathon26", "MARATHON26", " marathon 26 " and
// "marathon-26" are one code (spec criterion 5).
//
// Only separators a human inserts for readability are dropped — whitespace
// (including the non-breaking space a copy-paste from a poster carries),
// dashes (ASCII '-' plus the en/em dash and the minus sign an autocorrect
// turns '-' into) and the underscore. The spec names spaces and dashes; the
// underscore is folded with them because no valid code can contain one
// anyway (see promoCodeRe), so treating it as a separator can only turn a
// typo into a working code, never two distinct codes into one. Any OTHER
// character survives normalization and is then
// refused by ValidatePromoCode, so a typo can never be silently normalized
// into somebody else's code.
func NormalizePromoCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		switch {
		case unicode.IsSpace(r), r == ' ':
			continue
		case r == '-', r == '‐', r == '‑', r == '‒',
			r == '–', r == '—', r == '−', r == '_':
			continue
		default:
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// ValidatePromoCode checks an ALREADY NORMALIZED code against the stored
// shape. Returns ErrValidation for anything the database's CHECK would also
// refuse, so a bad code comes back as a 422 naming the field instead of a
// Postgres error. Callers normalize first: ValidatePromoCode(" mar 26 ")
// fails, ValidatePromoCode(NormalizePromoCode(" mar 26 ")) does not.
func ValidatePromoCode(code string) error {
	if !promoCodeRe.MatchString(code) {
		return fmt.Errorf("promo code must be %d-%d latin letters or digits: %w",
			PromoCodeMinLen, PromoCodeMaxLen, ErrValidation)
	}
	return nil
}

// PromoCode is a code a guest types on the booking confirmation step. It is
// the SECOND way to fill an existing Booking.PromotionID, not a second field
// with the same purpose: redeeming a code tags the booking with PromotionID
// and nothing else. There is no discount field — today a code is a
// participation tag (owner's decision, 12.09.2026); a future discount is an
// additive column, not a reshape.
//
// Title, terms and the cover picture are NOT duplicated here: the guest sees
// the linked promo's own localized text, so a code and its card can never
// drift apart.
//
// There is deliberately NO activation counter (ADR-047). The single source of
// truth for how much of a limit is spent is the bookings themselves:
//
//	count(distinct user_id) from bookings
//	 where promo_code_id = $1 and status not in ('cancelled','no_show')
//
// so a cancellation or a no-show frees its place with no decrement path that
// could silently drift. Spending is serialized by
// PromoCodeRepository.LockByID inside the booking-creation transaction.
type PromoCode struct {
	ID uuid.UUID
	// Code is stored normalized (NormalizePromoCode) and is unique.
	Code string
	// PromotionID is the promo this code stands for; this exact value is what
	// ends up in Booking.PromotionID. The database refuses to delete a promo
	// while a code points at it (ON DELETE RESTRICT, migration 0108).
	PromotionID uuid.UUID
	// StartsAt/ExpiresAt is the code's OWN acceptance window, not the promo's.
	// The promo may run longer than the code is accepted, or the other way
	// round; redeeming checks both.
	StartsAt  time.Time
	ExpiresAt time.Time
	// MaxUsesTotal is nil for "no overall limit". It counts DISTINCT GUESTS,
	// not bookings: it answers "how many people may join the campaign".
	MaxUsesTotal *int
	// MaxUsesPerUser is how many bookings ONE guest may tag with this code.
	// Always >= 1.
	MaxUsesPerUser int
	Status         PromoCodeStatus
	// CreatedBy is who created the code in the cabinet, nil for a code that
	// nobody's account created. It carries NO foreign key on users on purpose
	// (see migration 0108): attribution, not referential integrity.
	CreatedBy *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RedeemableAt reports whether the code itself would accept a redemption at
// t: active, and inside [StartsAt, ExpiresAt). It says nothing about the
// linked promo's own liveness or about the limits — those need a promo read
// and a bookings count under a lock respectively, both of which belong to the
// usecase, not to a value check.
func (c PromoCode) RedeemableAt(t time.Time) bool {
	return c.Status == PromoCodeActive && !t.Before(c.StartsAt) && t.Before(c.ExpiresAt)
}

// Expired reports whether the acceptance window has closed at t.
func (c PromoCode) Expired(t time.Time) bool { return !t.Before(c.ExpiresAt) }

// NotStarted reports whether the acceptance window has not opened yet at t.
func (c PromoCode) NotStarted(t time.Time) bool { return t.Before(c.StartsAt) }

// Validate checks the invariants the database also enforces (migration 0108),
// so an invalid code is refused with ErrValidation before Postgres sees it.
func (c PromoCode) Validate() error {
	if err := ValidatePromoCode(c.Code); err != nil {
		return err
	}
	if c.PromotionID == uuid.Nil {
		return fmt.Errorf("promo code must reference a promo: %w", ErrValidation)
	}
	if !c.Status.Valid() {
		return fmt.Errorf("unknown promo code status %q: %w", c.Status, ErrValidation)
	}
	if !c.ExpiresAt.After(c.StartsAt) {
		return fmt.Errorf("promo code expires_at must be after starts_at: %w", ErrValidation)
	}
	if c.MaxUsesTotal != nil && *c.MaxUsesTotal < 1 {
		// Zero is not "switched off" — that is what the paused status is for.
		return fmt.Errorf("promo code max_uses_total must be at least 1: %w", ErrValidation)
	}
	if c.MaxUsesPerUser < 1 {
		return fmt.Errorf("promo code max_uses_per_user must be at least 1: %w", ErrValidation)
	}
	return nil
}

// PromoCodeFilter narrows an admin listing of codes. The zero value lists
// every code.
type PromoCodeFilter struct {
	// PromotionID narrows to the codes of one promo.
	PromotionID *uuid.UUID
	// Statuses keeps only codes in one of the given statuses; empty means all.
	Statuses []PromoCodeStatus
}

// PromoCodeRepository persists promo_codes (migration 0108).
type PromoCodeRepository interface {
	// Create inserts a code. A duplicate code maps to ErrAlreadyExists, an
	// unknown promotion_id (FK violation) to ErrNotFound.
	Create(ctx context.Context, c *PromoCode) error
	// GetByID reads a code by id regardless of status. ErrNotFound if absent.
	GetByID(ctx context.Context, id uuid.UUID) (*PromoCode, error)
	// GetByCode reads a code by its NORMALIZED string, regardless of status —
	// the caller decides what "not active" means for it, because a guest
	// typing a paused code must be told "не действует", not "не найден".
	// ErrNotFound if absent.
	GetByCode(ctx context.Context, code string) (*PromoCode, error)
	// LockByID reads a code and holds a row lock on it until the surrounding
	// transaction ends (SELECT ... FOR UPDATE, ADR-047). It MUST be called
	// inside a transaction (sqltx.WithinTx) — outside one the lock is released
	// immediately and guarantees nothing.
	//
	// LOCK ORDER: promo_code FIRST, venue (capacity.LockVenue) SECOND. The
	// order is one-way and mandatory; any path that takes the venue lock
	// before the code's deadlocks against booking creation.
	LockByID(ctx context.Context, id uuid.UUID) (*PromoCode, error)
	// List returns codes matching filter, newest first.
	List(ctx context.Context, filter PromoCodeFilter) ([]PromoCode, error)
	// Update overwrites the mutable fields of an existing code (everything but
	// id, code and created_by — the code string is immutable by definition and
	// its snapshot already lives on past bookings). ErrNotFound if absent.
	Update(ctx context.Context, c *PromoCode) error
	// Delete removes a code. ErrNotFound if absent. Whether a code that has
	// already been redeemed may be deleted at all is a usecase decision
	// (archiving is the answer there) — the repository only executes.
	Delete(ctx context.Context, id uuid.UUID) error
}
