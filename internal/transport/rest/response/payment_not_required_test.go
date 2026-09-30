package response

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"backend-core/internal/domain"
)

// The wire shape a client keys on: 422 with the generic message and the stable
// code. The message stays "validation failed" (HandleError never leaks usecase
// text), so the code is the contract.
func TestHandleError_PaymentNotRequiredCode(t *testing.T) {
	w := httptest.NewRecorder()
	HandleError(w, domain.WithCode(domain.CodePaymentNotRequired,
		fmt.Errorf("%w: this booking requires no payment", domain.ErrValidation)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "validation failed" || body["code"] != "payment_not_required" || len(body) != 2 {
		t.Fatalf("body = %v, want {error: validation failed, code: payment_not_required}", body)
	}

	// A plain validation error is unchanged: generic code.
	w = httptest.NewRecorder()
	HandleError(w, fmt.Errorf("%w: something else", domain.ErrValidation))
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != string(domain.CodeValidation) {
		t.Fatalf("plain validation body = %v (err %v), want the generic code", body, err)
	}
}
