package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PushCampaignKind names the two content types a campaign can be sent about.
// Stored as VARCHAR and validated in app code — never a DB enum.
type PushCampaignKind string

const (
	PushCampaignKindEvent PushCampaignKind = "event"
	PushCampaignKindPromo PushCampaignKind = "promo"
)

// Valid reports whether k is a known campaign kind.
func (k PushCampaignKind) Valid() bool {
	return k == PushCampaignKindEvent || k == PushCampaignKindPromo
}

// PushCampaignStatus is the campaign's lifecycle state (migration 0111, §5.3
// of the spec): queued -> sending -> done|failed; queued|sending ->
// cancelled|expired. Terminal: done, cancelled, expired, failed — there is no
// transition out of a terminal state, "send again" is a NEW campaign through
// the same button.
type PushCampaignStatus string

const (
	PushCampaignQueued    PushCampaignStatus = "queued"
	PushCampaignSending   PushCampaignStatus = "sending"
	PushCampaignDone      PushCampaignStatus = "done"
	PushCampaignCancelled PushCampaignStatus = "cancelled"
	PushCampaignExpired   PushCampaignStatus = "expired"
	PushCampaignFailed    PushCampaignStatus = "failed"
)

// Terminal reports whether s is one of the four states nothing transitions out
// of.
func (s PushCampaignStatus) Terminal() bool {
	switch s {
	case PushCampaignDone, PushCampaignCancelled, PushCampaignExpired, PushCampaignFailed:
		return true
	default:
		return false
	}
}

// PushCampaignCancelReason names why a queued/sending campaign was cancelled
// instead of sent, set by the worker's re-read (spec §3.9/§3.15, criterion
// 11). Free VARCHAR, validated in app code.
type PushCampaignCancelReason string

const (
	CancelReasonSubjectUnpublished PushCampaignCancelReason = "subject_unpublished"
	CancelReasonSubjectExpired     PushCampaignCancelReason = "subject_expired"
	CancelReasonVenueInactive      PushCampaignCancelReason = "venue_inactive"
	CancelReasonSubjectMissing     PushCampaignCancelReason = "subject_missing"
)

// PushCampaign is one press of «Отправить пуш» — a queued or resolved manual
// send to every guest of one city about one published event or promo
// (migration 0111). It carries no recipient list of its own: that lives in
// PushCampaignRecipient, one row per targeted guest.
type PushCampaign struct {
	ID        uuid.UUID
	Kind      PushCampaignKind
	SubjectID uuid.UUID
	// RestaurantID is the subject's own venue, nil for a platform subject.
	// Carried here (denormalized off the subject) purely for
	// GET /admin/push-campaigns?restaurant_id= — the campaign's own authority
	// scoping — and is never re-resolved after creation.
	RestaurantID *uuid.UUID
	// CityID / City are the subject's EFFECTIVE city at creation time — a
	// snapshot, not a live join. nil CityID means "every city" (a platform
	// subject with no city override).
	CityID *uuid.UUID
	City   *string

	Status              PushCampaignStatus
	CancelReason        *PushCampaignCancelReason
	ForceQuietHours     bool
	EstimatedRecipients int

	Attempts      int
	NextAttemptAt *time.Time
	LeaseUntil    *time.Time
	LastError     string

	SentCount    int
	SkippedCount int
	FailedCount  int

	// CreatedBy is nil-able only because the FK is ON DELETE SET NULL
	// (deleting the admin's account must not erase campaign history) — the
	// usecase always sets it on creation.
	CreatedBy  *uuid.UUID
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// PushCampaignRecipientStatus is one guest's outcome within one campaign
// (migration 0111). sending -> sent|failed; the skipped_* values are set
// immediately and are terminal — a guest never moves from skipped_* to sent
// within the SAME campaign (a repeat send is a new campaign row).
type PushCampaignRecipientStatus string

const (
	RecipientSending          PushCampaignRecipientStatus = "sending"
	RecipientSent             PushCampaignRecipientStatus = "sent"
	RecipientFailed           PushCampaignRecipientStatus = "failed"
	RecipientSkippedOptOut    PushCampaignRecipientStatus = "skipped_optout"
	RecipientSkippedCap       PushCampaignRecipientStatus = "skipped_cap"
	RecipientSkippedDuplicate PushCampaignRecipientStatus = "skipped_duplicate"
	RecipientSkippedNoDevice  PushCampaignRecipientStatus = "skipped_no_device"
)

// PushCampaignRecipient is one guest's decision within one campaign.
type PushCampaignRecipient struct {
	CampaignID uuid.UUID
	UserID     uuid.UUID
	Status     PushCampaignRecipientStatus
	DecidedAt  time.Time
}

// PushCampaignSubject is what the campaign's own facade AND the worker's
// re-read both need to know about the event/promo a campaign targets — one
// query, one shape, so the numbers shown in the confirmation modal and the
// numbers the worker acts on never come from two different reads (spec §5.4:
// "Оценка и воркер используют одну функцию классификации гостя", and the
// same discipline extends one level up, to resolving the subject itself).
//
// It carries NO reference to the concrete Event/Promo domain type on purpose:
// PushCampaignSubjectRepository is implemented by a UNION-style read model
// over both tables (see postgres/pushcampaign), the same shape
// postgres/feed's read model already uses for the same reason — events and
// promos stay independent entities, and this is the one place their shapes
// are aligned for a caller that only cares about "is this thing sendable".
type PushCampaignSubject struct {
	Kind         PushCampaignKind
	SubjectID    uuid.UUID
	RestaurantID *uuid.UUID
	// RestaurantIsActive is true (vacuously) when RestaurantID is nil — a
	// platform subject has no venue to be inactive.
	RestaurantIsActive bool
	// Status is the raw event_status/promo_status string ("draft" |
	// "published" | "hidden") — both enums share the same three values, so a
	// single string field here avoids inventing a third enum.
	Status string
	// StartsAt / EndsAt are the subject's own validity window. An EVENT is
	// expired once StartsAt <= now (criterion 8 — the happening already
	// started); a PROMO is expired once EndsAt <= now. Both fields are always
	// populated regardless of Kind so the caller does not need a second query
	// to pick the right one.
	StartsAt time.Time
	EndsAt   time.Time
	// CityID / CityName are the EFFECTIVE city right now:
	// COALESCE(subject.city_id, restaurant.city_id). nil CityID with
	// RestaurantID == nil means "every city" (legitimate); nil CityID with
	// RestaurantID != nil means the venue's own city string does not resolve
	// in the dictionary (CodeCityUnresolved at Create).
	CityID   *uuid.UUID
	CityName *string
	// Title / TitleI18n render the push body (see usecase/pushcampaigns/text.go).
	Title     string
	TitleI18n I18n
	// VenueName is the restaurant's display name, "" for a platform subject.
	VenueName string
}

// Expired reports whether the subject's own validity window has already
// closed, evaluated at `now` — criterion 8's `subject_expired` reason.
func (s PushCampaignSubject) Expired(now time.Time) bool {
	if s.Kind == PushCampaignKindPromo {
		return !s.EndsAt.After(now)
	}
	return !s.StartsAt.After(now)
}

// IsPlatform reports whether the subject has no host venue.
func (s PushCampaignSubject) IsPlatform() bool { return s.RestaurantID == nil }

// PushCampaignSubjectRepository resolves the live state of a campaign's
// target — read fresh both by the admin facade (Estimate/Create) and by the
// worker's mandatory re-read before it ever sends a single push (criterion
// 11). ErrNotFound when the subject row itself no longer exists (deleted —
// spec 3.15's `subject_missing`).
type PushCampaignSubjectRepository interface {
	Resolve(ctx context.Context, kind PushCampaignKind, subjectID uuid.UUID) (*PushCampaignSubject, error)
	// ListIDsByRestaurant returns every subject id of `kind` belonging to
	// restaurantID, in no particular order — the id set the admin app already
	// has (from its own events/promos list) and asks this service to overlay
	// campaign status onto, the same "client already knows the ids, ask for
	// status" shape FeedControl's listVenueFeed uses (spec criterion 5).
	ListIDsByRestaurant(ctx context.Context, kind PushCampaignKind, restaurantID uuid.UUID) ([]uuid.UUID, error)
	// ListPlatformIDs returns every subject id of `kind` with no host venue.
	ListPlatformIDs(ctx context.Context, kind PushCampaignKind) ([]uuid.UUID, error)
}

// PushAudienceRow is one candidate guest's raw signals for the classification
// chain (usecase/pushcampaigns.Classify) — computed by ONE SQL query
// (PushCampaignAudienceRepository.Classify) so the Estimate breakdown and the
// worker's actual send decision can never read two different answers for the
// same guest.
type PushAudienceRow struct {
	UserID uuid.UUID
	// Allowed is the guest's own opt-out settings, already collapsed to one
	// bool: notifications_enabled AND push_enabled AND promo_push_enabled (a
	// missing preferences row means every flag defaults to true).
	Allowed bool
	// Capped is true when the guest already has a `sent` row within the
	// daily or weekly cap window, for ANY campaign (not just this subject).
	Capped bool
	// Duplicate is true when the guest already has a `sent` row for THIS
	// (kind, subject_id), from any past campaign — the repeat-send rule
	// (spec 3.5): only guests who never got this exact subject are resent.
	Duplicate bool
	// HasDevice is true when the guest has at least one active device token.
	HasDevice bool
}

// PushCampaignAudienceRepository computes the city-scoped audience and its
// per-guest classification signals in one query — the SQL the spec calls out
// as shared between Estimate and the worker (§4 criterion 3 / §5.4).
type PushCampaignAudienceRepository interface {
	// Classify returns one row per guest in the audience: `deleted_at IS
	// NULL` and, when cityID is non-nil, `city_key(users.city)` resolves to
	// cityID through city_aliases; a nil cityID matches every guest
	// (platform subject with no city override, including users with no city
	// set at all — spec 3.7).
	Classify(ctx context.Context, cityID *uuid.UUID, kind PushCampaignKind, subjectID uuid.UUID,
		now time.Time, dailyCap, weeklyCap int) ([]PushAudienceRow, error)
}

// PushCampaignRepository persists the campaign queue.
type PushCampaignRepository interface {
	// Create inserts a new queued campaign. ErrAlreadyExists (mapped from the
	// partial unique index on (kind, subject_id) WHERE status IN
	// (queued,sending)) when an active campaign for the same subject already
	// exists — a DB-enforced guard against the double-click race (criterion 2
	// / 3.3), never a read-then-write check.
	Create(ctx context.Context, c *PushCampaign) error
	GetByID(ctx context.Context, id uuid.UUID) (*PushCampaign, error)
	// LatestBySubjects returns, for each (kind, subjectID) pair that has at
	// least one campaign, the most recent one by created_at — the badge's
	// data source (criterion 5). Pairs with no campaign are simply absent
	// from the result, never a zero-value entry.
	LatestBySubjects(ctx context.Context, kind PushCampaignKind, subjectIDs []uuid.UUID) (map[uuid.UUID]PushCampaign, error)
	// CountToday reports how many campaigns were created today (UTC calendar
	// day of `since`) in cityID — the modal's "already N campaigns today"
	// warning (criterion 3). cityID nil counts platform-wide ("every city")
	// campaigns only, mirroring the exact-NULL semantics city_id carries
	// everywhere else in this feature.
	CountToday(ctx context.Context, cityID *uuid.UUID, since time.Time) (int, error)

	// Claim atomically picks up to limit campaigns ready for the worker: a
	// fresh `queued` row, or a `sending` row whose lease has expired and
	// whose backoff has elapsed (FOR UPDATE SKIP LOCKED — criterion 10).
	// A `queued` row older than maxQueueAge is expired IN PLACE instead of
	// claimed (criterion 17) and returned separately, never handed to the
	// caller as claimed work. Must run inside TxManager.WithinTx so the row
	// lock is actually held for the claim itself; the lease (not the row
	// lock) is what keeps a second worker process off it afterwards.
	Claim(ctx context.Context, now time.Time, leaseFor, maxQueueAge time.Duration, limit int) (claimed []PushCampaign, expired []uuid.UUID, err error)
	// Cancel moves a queued/sending campaign to `cancelled` with reason,
	// zero pushes sent (criterion 11). A no-op (not an error) if the
	// campaign is no longer in a cancellable state — a concurrent finish
	// wins.
	Cancel(ctx context.Context, id uuid.UUID, reason PushCampaignCancelReason, at time.Time) error
	// Reschedule bumps attempts and sets next_attempt_at after a transient
	// (429/5xx) failure, or moves the campaign to `failed` once attempts
	// reaches maxAttempts (criterion 16).
	Reschedule(ctx context.Context, id uuid.UUID, lastError string, nextAttemptAt time.Time, maxAttempts int) error
	// Finish marks a campaign `done` with its final counters (criterion 18).
	Finish(ctx context.Context, id uuid.UUID, sent, skipped, failed int, at time.Time) error
}

// PushCampaignRecipientRepository persists the per-guest decision ledger.
type PushCampaignRecipientRepository interface {
	// ClaimSending inserts a `sending` row for every userID that does not
	// already have ONE for this campaign, and returns exactly the userIDs
	// that were newly inserted — the ones the caller must actually send to.
	// A userID already present (any status, including a previous `sending`
	// left by a crashed attempt) is silently excluded: criterion 14's "a
	// crash between send and record must not double-push" is enforced here,
	// at the write, not by a caller-side check-then-insert.
	ClaimSending(ctx context.Context, campaignID uuid.UUID, userIDs []uuid.UUID, at time.Time) (claimed []uuid.UUID, err error)
	// UnclaimSending removes the `sending` row ClaimSending wrote for each of
	// userIDs, ONLY while it is still `sending` — it is how the sender safely
	// retries a guest after a KNOWN-clean failure (the batch call itself
	// erred, so nothing was actually delivered) without weakening the
	// crash-safety ClaimSending exists for: a row this call does not find in
	// `sending` (already resolved to sent/failed by the same in-process call,
	// or already resolved on a previous pass) is left untouched. Retrying a
	// guest this way can only ever re-attempt an UNDELIVERED push — the
	// caller must never call it for a guest whose send came back Delivered.
	UnclaimSending(ctx context.Context, campaignID uuid.UUID, userIDs []uuid.UUID) error
	// Resolve finalizes ONE guest's outcome for the campaign (sent/failed —
	// the skipped_* statuses are written directly, never through
	// ClaimSending). Idempotent: resolving an already-resolved row again is
	// a no-op, not an error.
	Resolve(ctx context.Context, campaignID, userID uuid.UUID, status PushCampaignRecipientStatus, at time.Time) error
	// RecordSkipped writes the skipped_* rows in one batch — guests the
	// classification chain never intends to send to at all, so they never
	// pass through `sending`. ON CONFLICT DO NOTHING: a userID already
	// decided for this campaign (e.g. a retry pass after a partial send)
	// is left alone.
	RecordSkipped(ctx context.Context, campaignID uuid.UUID, userIDs []uuid.UUID, status PushCampaignRecipientStatus, at time.Time) error
	// CountByStatus returns this campaign's recipient counts grouped by
	// status — the skip-reason breakdown GET /admin/push-campaigns/:id
	// exposes (criterion 6).
	CountByStatus(ctx context.Context, campaignID uuid.UUID) (map[PushCampaignRecipientStatus]int, error)
}
