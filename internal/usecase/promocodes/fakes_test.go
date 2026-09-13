package promocodes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeCodes is an in-memory domain.PromoCodeRepository. LockByID records that
// it was called and in which order relative to the usage count, because "the
// count happens under the lock" is the property ConsumeTx exists for.
type fakeCodes struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]domain.PromoCode
	calls []string
}

func newFakeCodes(codes ...domain.PromoCode) *fakeCodes {
	f := &fakeCodes{byID: map[uuid.UUID]domain.PromoCode{}}
	for _, c := range codes {
		f.byID[c.ID] = c
	}
	return f
}

func (f *fakeCodes) Create(_ context.Context, c *domain.PromoCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	for _, existing := range f.byID {
		if existing.Code == c.Code {
			return fmt.Errorf("create: %w", domain.ErrAlreadyExists)
		}
	}
	c.CreatedAt, c.UpdatedAt = time.Now(), time.Now()
	f.byID[c.ID] = *c
	return nil
}

func (f *fakeCodes) GetByID(_ context.Context, id uuid.UUID) (*domain.PromoCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("get: %w", domain.ErrNotFound)
	}
	return &c, nil
}

func (f *fakeCodes) GetByCode(_ context.Context, code string) (*domain.PromoCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.byID {
		if c.Code == code {
			out := c
			return &out, nil
		}
	}
	return nil, fmt.Errorf("get by code: %w", domain.ErrNotFound)
}

func (f *fakeCodes) LockByID(ctx context.Context, id uuid.UUID) (*domain.PromoCode, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "lock")
	f.mu.Unlock()
	return f.GetByID(ctx, id)
}

func (f *fakeCodes) List(_ context.Context, filter domain.PromoCodeFilter) ([]domain.PromoCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PromoCode, 0, len(f.byID))
	for _, c := range f.byID {
		if filter.PromotionID != nil && c.PromotionID != *filter.PromotionID {
			continue
		}
		if len(filter.Statuses) > 0 {
			match := false
			for _, s := range filter.Statuses {
				match = match || s == c.Status
			}
			if !match {
				continue
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeCodes) Update(_ context.Context, c *domain.PromoCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[c.ID]; !ok {
		return fmt.Errorf("update: %w", domain.ErrNotFound)
	}
	c.UpdatedAt = time.Now()
	f.byID[c.ID] = *c
	return nil
}

func (f *fakeCodes) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[id]; !ok {
		return fmt.Errorf("delete: %w", domain.ErrNotFound)
	}
	delete(f.byID, id)
	return nil
}

// fakePromos answers GetPublicDetail for the promos it was told are live —
// exactly the visibility rule the real facade applies (a draft or expired
// campaign is ErrNotFound here, like an absent one).
type fakePromos struct {
	live map[uuid.UUID]domain.PromoListItem
	err  error
}

func newFakePromos(items ...domain.Promo) *fakePromos {
	f := &fakePromos{live: map[uuid.UUID]domain.PromoListItem{}}
	for _, p := range items {
		f.live[p.ID] = domain.PromoListItem{Promo: p}
	}
	return f
}

func (f *fakePromos) GetPublicDetail(_ context.Context, id uuid.UUID) (*domain.PromoListItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	it, ok := f.live[id]
	if !ok {
		return nil, fmt.Errorf("get public promo: %w", domain.ErrNotFound)
	}
	return &it, nil
}

// fakeUsage returns a fixed count and records the call, so a test can assert
// the count was taken AFTER the lock.
type fakeUsage struct {
	usage domain.PromoCodeUsage
	calls *[]string
}

func (f fakeUsage) CountPromoCodeUsage(_ context.Context, _ uuid.UUID, _ uuid.UUID) (domain.PromoCodeUsage, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, "count")
	}
	return f.usage, nil
}
