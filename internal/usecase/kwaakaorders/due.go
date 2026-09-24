package kwaakaorders

import (
	"time"

	"backend-core/internal/domain"
)

// dueDecision is the result of dueAt: whether the booking goes now, why, and
// its send window.
type dueDecision struct {
	Send       bool
	Trigger    domain.KitchenOrderTrigger
	DeadlineAt time.Time
}

// hold is how long a table stays "ours" after the visit/send moment.
const tableHold = 30 * time.Minute

// dueAt is the whole "when does a booking go to the kitchen" rule (plan §2.4):
//
//	confirmed + paid   → at starts_at − lead, window until starts_at + 30m
//	confirmed + unpaid → never
//	arrived            → immediately, window until max(ends_at, arrived_at + 30m)
//	anything else      → never
//
// A send moment earlier than enabledAt never goes: enabling a venue mid-evening
// must not push to the kitchen what the venue already keyed in by hand.
func dueAt(s *domain.KitchenClaimSubject, lead time.Duration, enabledAt *time.Time, now time.Time) dueDecision {
	var sendMoment time.Time
	var d dueDecision
	switch s.Status {
	case domain.BookingConfirmed:
		if !s.Paid {
			return dueDecision{}
		}
		due := s.StartsAt.Add(-lead)
		if now.Before(due) {
			return dueDecision{}
		}
		sendMoment = due
		if s.CapturedAt != nil && s.CapturedAt.After(sendMoment) {
			sendMoment = *s.CapturedAt
		}
		if s.ConfirmedAt != nil && s.ConfirmedAt.After(sendMoment) {
			sendMoment = *s.ConfirmedAt
		}
		d = dueDecision{Trigger: domain.KitchenTriggerLead, DeadlineAt: s.StartsAt.Add(tableHold)}
	case domain.BookingArrived:
		arrived := s.StartsAt
		if s.ArrivedAt != nil {
			arrived = *s.ArrivedAt
		}
		sendMoment = arrived
		deadline := arrived.Add(tableHold)
		if s.EndsAt.After(deadline) {
			deadline = s.EndsAt
		}
		d = dueDecision{Trigger: domain.KitchenTriggerArrived, DeadlineAt: deadline}
	default:
		return dueDecision{}
	}
	if now.After(d.DeadlineAt) {
		return dueDecision{}
	}
	if enabledAt != nil && sendMoment.Before(*enabledAt) {
		return dueDecision{}
	}
	d.Send = true
	return d
}

// tableHoldUntil is when our claim on a pool table lapses on its own.
func tableHoldUntil(s *domain.KitchenClaimSubject, now time.Time) time.Time {
	m := s.EndsAt
	if now.After(m) {
		m = now
	}
	return m.Add(tableHold)
}
