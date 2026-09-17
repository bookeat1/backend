package pushcampaigns

import (
	"time"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/pushcampaigns"
)

type textResponse struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// lastCampaignResponse is the estimate's "уже отправляли..." summary
// (criterion 3 / spec 3.5), nil when the subject has never had a campaign.
type lastCampaignResponse struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	SentCount  int        `json:"sent_count"`
}

type estimateResponse struct {
	City                 string                  `json:"city"`
	InCity               int                     `json:"in_city"`
	NoDevice             int                     `json:"no_device"`
	OptedOut             int                     `json:"opted_out"`
	Capped               int                     `json:"capped"`
	AlreadyReceived      int                     `json:"already_received"`
	Eligible             int                     `json:"eligible"`
	QuietHoursNow        bool                    `json:"quiet_hours_now"`
	CampaignsTodayInCity int                     `json:"campaigns_today_in_city"`
	LastCampaign         *lastCampaignResponse   `json:"last_campaign"`
	Preview              map[string]textResponse `json:"preview"`
}

func newEstimateResponse(r uc.EstimateResult) estimateResponse {
	preview := make(map[string]textResponse, len(r.Preview))
	for lang, t := range r.Preview {
		preview[lang] = textResponse{Title: t.Title, Body: t.Body}
	}
	out := estimateResponse{
		City: r.City, InCity: r.InCity, NoDevice: r.NoDevice, OptedOut: r.OptedOut,
		Capped: r.Capped, AlreadyReceived: r.AlreadyReceived, Eligible: r.Eligible,
		QuietHoursNow: r.QuietHoursNow, CampaignsTodayInCity: r.CampaignsTodayInCity,
		Preview: preview,
	}
	if r.LastCampaign != nil {
		out.LastCampaign = &lastCampaignResponse{
			ID: r.LastCampaign.ID.String(), Status: string(r.LastCampaign.Status),
			FinishedAt: r.LastCampaign.FinishedAt, SentCount: r.LastCampaign.SentCount,
		}
	}
	return out
}

// createdResponse is POST's 201 body (criterion 4).
type createdResponse struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	EstimatedRecipients int    `json:"estimated_recipients"`
}

func newCreatedResponse(c domain.PushCampaign) createdResponse {
	return createdResponse{ID: c.ID.String(), Status: string(c.Status), EstimatedRecipients: c.EstimatedRecipients}
}

// summaryResponse is one row of GET /admin/push-campaigns (criterion 5).
type summaryResponse struct {
	SubjectID           string     `json:"subject_id"`
	ID                  string     `json:"id"`
	Status              string     `json:"status"`
	CreatedAt           time.Time  `json:"created_at"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	SentCount           int        `json:"sent_count"`
	SkippedCount        int        `json:"skipped_count"`
	FailedCount         int        `json:"failed_count"`
	EstimatedRecipients int        `json:"estimated_recipients"`
	CancelReason        *string    `json:"cancel_reason,omitempty"`
}

func newSummaryResponse(c domain.PushCampaign) summaryResponse {
	out := summaryResponse{
		SubjectID: c.SubjectID.String(), ID: c.ID.String(), Status: string(c.Status),
		CreatedAt: c.CreatedAt, FinishedAt: c.FinishedAt, SentCount: c.SentCount,
		SkippedCount: c.SkippedCount, FailedCount: c.FailedCount, EstimatedRecipients: c.EstimatedRecipients,
	}
	if c.CancelReason != nil {
		reason := string(*c.CancelReason)
		out.CancelReason = &reason
	}
	return out
}

// detailResponse is GET /admin/push-campaigns/:id (criterion 6): the same
// summary plus the skip-reason breakdown for a human to diagnose.
type detailResponse struct {
	summaryResponse
	SkipCounts map[string]int `json:"skip_counts"`
}

func newDetailResponse(r uc.GetResult) detailResponse {
	counts := make(map[string]int, len(r.SkipCounts))
	for status, n := range r.SkipCounts {
		counts[string(status)] = n
	}
	return detailResponse{summaryResponse: newSummaryResponse(r.Campaign), SkipCounts: counts}
}
