package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ContentSource records where a candidate event/promo draft came from. It is
// source-agnostic on purpose: a staff member typing one in by hand, a future
// AI content parser reading a restaurant's Instagram, a website scrape, a
// Telegram channel, or an afisha/Kwaaka feed all produce the SAME record and
// land in the same human-review queue. Stored as VARCHAR, validated here —
// never a Postgres ENUM. Mirrors ExternalSource for occupancy holds.
type ContentSource string

const (
	// ContentSourceManual is a draft a staff member entered by hand.
	ContentSourceManual ContentSource = "manual"
	// ContentSourceInstagram is a draft extracted from a venue's Instagram.
	ContentSourceInstagram ContentSource = "instagram"
	// ContentSourceWebsite is a draft extracted from a venue's own website.
	ContentSourceWebsite ContentSource = "website"
	// ContentSourceTelegram is a draft extracted from a Telegram channel/post.
	ContentSourceTelegram ContentSource = "telegram"
	// ContentSourceAfisha is a draft extracted from an events aggregator (afisha).
	ContentSourceAfisha ContentSource = "afisha"
	// ContentSourceKwaaka is reserved for a Kwaaka-fed content draft.
	ContentSourceKwaaka ContentSource = "kwaaka"
)

// Valid reports whether s is a known content source.
func (s ContentSource) Valid() bool {
	switch s {
	case ContentSourceManual, ContentSourceInstagram, ContentSourceWebsite,
		ContentSourceTelegram, ContentSourceAfisha, ContentSourceKwaaka:
		return true
	}
	return false
}

// DraftKind is what a draft will become once approved: an Event or a Promo.
type DraftKind string

const (
	DraftKindEvent DraftKind = "event"
	DraftKindPromo DraftKind = "promo"
)

// Valid reports whether k is a known draft kind.
func (k DraftKind) Valid() bool {
	return k == DraftKindEvent || k == DraftKindPromo
}

// DraftStatus is where a draft sits in the human-in-the-loop review gate. A
// draft is born pending_review and NEVER auto-publishes: a staff member with
// PermRestaurantManage must explicitly approve it (which creates the real
// published Event/Promo) or reject it. Both are terminal.
type DraftStatus string

const (
	// DraftPendingReview is awaiting a staff decision — the only state from
	// which approve/reject may act.
	DraftPendingReview DraftStatus = "pending_review"
	// DraftApproved was turned into a real published entity (CreatedEventID /
	// CreatedPromoID points at it).
	DraftApproved DraftStatus = "approved"
	// DraftRejected was dismissed by staff; nothing was created.
	DraftRejected DraftStatus = "rejected"
)

// Valid reports whether s is a known draft status.
func (s DraftStatus) Valid() bool {
	switch s {
	case DraftPendingReview, DraftApproved, DraftRejected:
		return true
	}
	return false
}

// ContentDraft is a CANDIDATE event or promo submitted by some source into the
// review queue. It is the ingestion seam a future AI content parser writes
// into (via ContentDraftRepository.Create) — there is deliberately no external
// HTTP submission endpoint in this increment; the parser is an internal caller.
//
// RawPayload is the original extracted text/fields exactly as the source
// produced them (jsonb), kept for audit and so a reviewer can see what was
// parsed. The Suggested* fields are the normalized proposal a staff member
// reviews and, on approval, become the created entity. Suggested*I18n mirror
// the localized shape of Event/Promo so a translation the parser found is
// carried through to the published entity.
type ContentDraft struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Kind         DraftKind
	Source       ContentSource
	// SourceRef is the source system's own id for this item (a post id), the
	// dedup key a future parser can use. nil for a hand-entered draft.
	SourceRef *string
	// SourceURL is a human-openable link back to the original, nil when none.
	SourceURL *string
	// RawPayload is the untouched original extraction (jsonb). Never nil — an
	// empty object ({}) is stored when a source carried no structured payload.
	RawPayload json.RawMessage

	SuggestedTitle           string
	SuggestedTitleI18n       I18n
	SuggestedDescription     string
	SuggestedDescriptionI18n I18n
	// SuggestedStartsAt/EndsAt is the proposed window. Required to approve
	// (both an event and a promo need a window); the usecase rejects approval
	// of a draft missing either.
	SuggestedStartsAt *time.Time
	SuggestedEndsAt   *time.Time
	// SuggestedVenue applies to an event draft; SuggestedTerms to a promo
	// draft. Both nil-able; the irrelevant one for a given Kind is ignored.
	SuggestedVenue *string
	SuggestedTerms *string

	Status     DraftStatus
	ReviewedBy *uuid.UUID
	ReviewedAt *time.Time
	// CreatedEventID / CreatedPromoID is set on approval to the entity created
	// from this draft (exactly one, matching Kind). Both nil while pending or
	// rejected.
	CreatedEventID *uuid.UUID
	CreatedPromoID *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ContentDraftRepository persists candidate content drafts and their review
// outcome. Get* return ErrNotFound when absent.
type ContentDraftRepository interface {
	// Create inserts a new draft in pending_review. This is the method a future
	// internal parser calls — there is no external HTTP endpoint for it. An
	// unknown restaurant_id (FK violation) maps to ErrNotFound.
	Create(ctx context.Context, d *ContentDraft) error
	// GetByID returns a draft by its id (staff resolve the draft and its
	// restaurant before authorizing).
	GetByID(ctx context.Context, id uuid.UUID) (*ContentDraft, error)
	// ListPendingByRestaurant returns a restaurant's pending_review drafts,
	// oldest first (a review queue is FIFO) with id as a stable tie-breaker,
	// paginated, plus the total pending count.
	ListPendingByRestaurant(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]ContentDraft, int, error)
	// MarkApproved transitions a draft from pending_review to approved,
	// recording the reviewer, the time and the created entity id, all inside
	// the caller's transaction (so it commits atomically with the entity's
	// insert). It is a CAS on status = 'pending_review': ErrInvalidStatus when
	// the draft is not pending, ErrNotFound when the id is absent. Exactly one
	// of eventID/promoID must be non-nil.
	MarkApproved(ctx context.Context, id uuid.UUID, reviewedBy uuid.UUID, at time.Time, eventID, promoID *uuid.UUID) error
	// MarkRejected transitions a draft from pending_review to rejected.
	// Same CAS semantics as MarkApproved. Nothing else is created.
	MarkRejected(ctx context.Context, id uuid.UUID, reviewedBy uuid.UUID, at time.Time) error
}
