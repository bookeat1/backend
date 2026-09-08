package bookings

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The venue owner's question — "who put 12 people in a 10-seat room last
// Saturday" — has to be answerable through the product, not only through psql.
func TestListCapacityOverrides(t *testing.T) {
	rid, uid, bid, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	bucket := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)

	d := newDeps()
	d.role = domain.RoleRestaurant
	d.manages = true
	d.overrides.list = []domain.BookingCapacityOverride{{
		ID: uuid.New(), BookingID: bid, RestaurantID: rid,
		ActorUserID: actorID, ActorType: domain.ActorManager,
		Guests: 12, DeclaredSeats: 10, SeatsOver: 2,
		PeakBucketStart: bucket, PeakSeatsTaken: 12, CreatedAt: bucket,
	}}
	r := newRouter(d)

	from, to := bucket.Add(-6*time.Hour), bucket.Add(12*time.Hour)
	path := "/api/v1/restaurants/" + rid.String() + "/capacity-overrides" +
		"?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	w := do(r, http.MethodGet, path, nil, authHeader(uid))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	var body struct {
		Data []capacityOverrideResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body)
	}
	if len(body.Data) != 1 {
		t.Fatalf("rows = %d, want 1 (body %s)", len(body.Data), w.Body)
	}
	got := body.Data[0]
	if got.BookingID != bid.String() || got.ActorUserID != actorID.String() ||
		got.ActorType != string(domain.ActorManager) || got.SeatsOver != 2 ||
		got.DeclaredSeats != 10 || got.PeakSeatsTaken != 12 {
		t.Fatalf("row = %+v", got)
	}
	// The window is passed through, not invented by the handler.
	if !d.overrides.from.Equal(from) || !d.overrides.to.Equal(to) {
		t.Fatalf("usecase got window %s..%s, want %s..%s", d.overrides.from, d.overrides.to, from, to)
	}

	// A window that is not two RFC3339 timestamps is refused rather than
	// silently widened to everything the venue ever did.
	w = do(r, http.MethodGet, "/api/v1/restaurants/"+rid.String()+"/capacity-overrides", nil, authHeader(uid))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing window: status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}
