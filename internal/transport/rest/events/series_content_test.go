package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func seriesEvent() domain.Event {
	rid, ruleID := uuid.New(), uuid.New()
	cover := "https://cdn.example/nikos.jpg"
	return domain.Event{
		ID:               uuid.New(),
		RestaurantID:     &rid,
		RecurrenceID:     &ruleID,
		Title:            "Greek Party с Никосом",
		Description:      "Гость — Никос",
		CoverImageURL:    &cover,
		Tags:             []string{"Живая музыка"},
		Status:           domain.EventPublished,
		StartsAt:         time.Now(),
		EndsAt:           time.Now().Add(3 * time.Hour),
		ContentOverrides: []domain.EventContentField{domain.EventContentTitle, domain.EventContentCover},
	}
}

// The cabinet has to be able to draw "это поле у даты своё" next to the field,
// so the admin shape carries the markers.
func TestAdminResponseCarriesContentOverrides(t *testing.T) {
	body, err := json.Marshal(adminResponse(seriesEvent()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fields, ok := got["content_overrides"].([]any)
	if !ok || len(fields) != 2 || fields[0] != "title" || fields[1] != "cover_image_url" {
		t.Fatalf("the cabinet must be told which fields this date owns, got %v", got["content_overrides"])
	}
}

// The guest response must not change shape at all because of this feature: a
// guest sees the resolved content and has no business knowing where it came
// from.
func TestPublicResponseHidesContentOverrides(t *testing.T) {
	body, err := json.Marshal(publicResponse(seriesEvent(), "ru"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "content_overrides") {
		t.Fatalf("the guest shape must not carry the override markers: %s", body)
	}
}

// A date that follows its series entirely omits the field rather than sending
// an empty array, so an existing cabinet build sees exactly what it saw before.
func TestAdminResponseOmitsEmptyOverrides(t *testing.T) {
	e := seriesEvent()
	e.ContentOverrides = nil
	body, err := json.Marshal(adminResponse(e))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "content_overrides") {
		t.Fatalf("an inheriting date must not carry the field at all: %s", body)
	}
}
