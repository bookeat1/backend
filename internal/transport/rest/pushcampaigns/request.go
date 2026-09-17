package pushcampaigns

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/pushcampaigns"
)

// createRequest is POST /admin/push-campaigns's body (spec criterion 4).
type createRequest struct {
	Kind            string `json:"kind" example:"event"`
	SubjectID       string `json:"subject_id"`
	ForceQuietHours bool   `json:"force_quiet_hours,omitempty"`
}

func (r createRequest) toInput() (uc.CreateInput, error) {
	kind := domain.PushCampaignKind(r.Kind)
	if !kind.Valid() {
		return uc.CreateInput{}, fmt.Errorf("%w: kind must be 'event' or 'promo'", domain.ErrValidation)
	}
	id, err := uuid.Parse(r.SubjectID)
	if err != nil {
		return uc.CreateInput{}, fmt.Errorf("%w: invalid subject_id", domain.ErrValidation)
	}
	return uc.CreateInput{Kind: kind, SubjectID: id, ForceQuietHours: r.ForceQuietHours}, nil
}
