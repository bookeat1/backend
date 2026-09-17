// Package pushcampaigns is the application logic for manual push campaigns
// (spec push-campaigns-manual-spec-2026-09-17.md): the superadmin's
// «Отправить пуш» button on a published event or promo. It owns:
//
//   - Facade — the admin-facing operations (Estimate/Create/List/Get), gated
//     to domain.RoleAdmin for the two mutating ones and to RoleAdmin OR
//     PermRestaurantManage-at-that-restaurant for the read-only list;
//   - Sender — the cmd/worker background sender: claim+lease, the mandatory
//     subject re-read, the audience fan-out, the frequency caps and dedupe,
//     at-most-once delivery, and backoff.
//
// Both share ONE guest-classification function (Classify, classify.go) and
// ONE text-rendering function (renderText, text.go), so the numbers a
// superadmin sees in the confirmation modal and the pushes the worker
// actually sends can never disagree — see the spec's §5.4.
package pushcampaigns

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. Only domain.RoleAdmin may Estimate or
// Create a campaign (spec §0.1 — "аудитория весь город — общий ресурс,
// владельцу/менеджеру заведения права не даём"); ListLatestBySubjects/Get
// additionally allow restaurant staff with PermRestaurantManage, READ ONLY,
// at the campaign's own restaurant.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant" —
// the same seam usecase/events declares locally rather than importing
// usecase/restaurants' concrete type. Bound to restaurants.ManagerUseCase in
// bootstrap.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// SendVerdict is the provider's per-message outcome — an independent copy of
// notifications.MobilePushVerdict's three cases, not imported directly: this
// package declares its own small port here, the same discipline
// usecase/events uses for tasteProfileLoader instead of importing
// usecase/tastematch's types. bootstrap/deps.go adapts expopush.Sender's
// results into this shape when wiring Sender.
type SendVerdict int

const (
	SendDelivered SendVerdict = iota
	SendDeviceGone
	SendRejected
)

// SendResult is what the provider said about ONE message in a batch.
type SendResult struct {
	Verdict  SendVerdict
	TicketID string
}

// SendMessage is one recipient's rendered push, ready for the provider.
type SendMessage struct {
	Token     string
	Title     string
	Body      string
	Data      map[string]string
	ChannelID string
}

// BatchSender delivers many messages in one logical call (chunked internally
// to the provider's own request-size ceiling — see expopush.Sender.SendBatch),
// returning one result per input message, positionally aligned. A non-nil
// error means the call could not get an answer for every message (a
// transient HTTP failure partway through): the campaign is left `sending`
// and retried later (spec 3.11) rather than assumed sent.
type BatchSender func(ctx context.Context, msgs []SendMessage) ([]SendResult, error)

// languageReader batches guests' preferred_language for text rendering — a
// dedicated minimal reader (one column, many ids in one round trip), not the
// full domain.UserRepository (which has no batch method and would mean one
// query per audience member across a whole city) and not
// domain.UserRepository.GetByID repeated per guest. A userID absent from the
// result (deleted between audience read and send, or truly missing) falls
// back to Russian at the call site.
type languageReader interface {
	PreferredLanguages(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error)
}
