package bookings

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// policyEnvelope mirrors response.Envelope for the policy payload.
type policyEnvelope struct {
	Data bookingPolicyResponse `json:"data"`
}

func decodePolicy(t *testing.T, body []byte) bookingPolicyResponse {
	t.Helper()
	var env policyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode policy response: %v (body %s)", err, body)
	}
	return env.Data
}

func TestPatchBookingPolicy(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()

	t.Run("manager patches a subset and gets the effective policy back", func(t *testing.T) {
		d := newDeps()
		d.role = domain.RoleRestaurant
		d.manages = true
		r := newRouter(d)

		w := do(r, http.MethodPatch, "/api/v1/restaurants/"+rid.String()+"/booking-policy",
			gin.H{"auto_confirm": false, "confirm_sla_minutes": 20, "booking_buffer_minutes": 0},
			authHeader(uid))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
		}
		if d.policy.patchCnt != 1 || d.policy.gotID != rid {
			t.Fatalf("usecase called %d time(s) with id %s, want 1 with %s",
				d.policy.patchCnt, d.policy.gotID, rid)
		}
		// Pointers: an omitted field must arrive as nil, an explicit zero as 0/false.
		in := d.policy.gotIn
		if in.AutoConfirm == nil || *in.AutoConfirm {
			t.Errorf("auto_confirm = %v, want pointer to false", in.AutoConfirm)
		}
		if in.BookingBufferMinutes == nil || *in.BookingBufferMinutes != 0 {
			t.Errorf("buffer = %v, want pointer to 0", in.BookingBufferMinutes)
		}
		if in.ConfirmSLAMinutes == nil || *in.ConfirmSLAMinutes != 20 {
			t.Errorf("confirm_sla = %v, want pointer to 20", in.ConfirmSLAMinutes)
		}
		if in.Timezone != nil || in.BookingHorizonDays != nil {
			t.Errorf("omitted fields must stay nil, got tz=%v horizon=%v", in.Timezone, in.BookingHorizonDays)
		}

		got := decodePolicy(t, w.Body.Bytes())
		if got.RestaurantID != rid.String() {
			t.Errorf("restaurant_id = %s, want %s", got.RestaurantID, rid)
		}
		if got.Effective.ConfirmSLAMinutes != 20 || got.Effective.AutoConfirm {
			t.Errorf("effective = %+v, want confirm_sla 20 and auto_confirm false", got.Effective)
		}
		if got.Effective.DurationMinutes != 90 || got.Effective.Timezone != "Asia/Almaty" {
			t.Errorf("effective inherited values = %+v", got.Effective)
		}
		if got.Overrides.AutoConfirm == nil || *got.Overrides.AutoConfirm {
			t.Errorf("overrides.auto_confirm = %v, want false", got.Overrides.AutoConfirm)
		}
		if got.Overrides.Timezone != nil {
			t.Errorf("overrides.timezone = %v, want null (inherited)", *got.Overrides.Timezone)
		}
	})

	t.Run("admin may patch any venue", func(t *testing.T) {
		d := newDeps()
		d.role = domain.RoleAdmin
		d.manages = false
		r := newRouter(d)

		w := do(r, http.MethodPatch, "/api/v1/restaurants/"+rid.String()+"/booking-policy",
			gin.H{"auto_confirm": true}, authHeader(uid))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
		}
	})

	t.Run("empty patch is rejected", func(t *testing.T) {
		d := newDeps()
		d.role = domain.RoleRestaurant
		d.manages = true
		r := newRouter(d)

		w := do(r, http.MethodPatch, "/api/v1/restaurants/"+rid.String()+"/booking-policy",
			gin.H{}, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
		if d.policy.patchCnt != 0 {
			t.Errorf("usecase called %d times, want 0", d.policy.patchCnt)
		}
	})

	t.Run("get returns the current policy", func(t *testing.T) {
		d := newDeps()
		d.role = domain.RoleRestaurant
		d.manages = true
		r := newRouter(d)

		w := do(r, http.MethodGet, "/api/v1/restaurants/"+rid.String()+"/booking-policy", nil, authHeader(uid))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
		}
		if got := decodePolicy(t, w.Body.Bytes()); got.Effective.Timezone != "Asia/Almaty" {
			t.Errorf("timezone = %q, want Asia/Almaty", got.Effective.Timezone)
		}
	})
}
