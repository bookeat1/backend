package staticmap

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeRestaurants is the minimal RestaurantCoords double: an in-memory catalog
// plus a call counter, so a test can assert the cache keeps the DB out of the
// hot path too, not just the provider.
type fakeRestaurants struct {
	byID  map[uuid.UUID]*domain.RestaurantAggregate
	calls atomic.Int64
	err   error
}

func newFakeRestaurants() *fakeRestaurants {
	return &fakeRestaurants{byID: map[uuid.UUID]*domain.RestaurantAggregate{}}
}

func (f *fakeRestaurants) add(id uuid.UUID, lat, lon *float64, active bool) {
	f.byID[id] = &domain.RestaurantAggregate{
		Restaurant: domain.Restaurant{ID: id, Latitude: lat, Longitude: lon, IsActive: active},
	}
}

func (f *fakeRestaurants) GetByID(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return r, nil
}

// fakeProvider records every render and returns a scripted outcome. render is
// overridable so a test can block inside the provider (the stampede and
// cancellation cases).
//
// The override receives the CONTEXT the usecase handed the provider, because
// that is the whole thing under test in the cancellation cases: a real provider
// is an *http.Client, and an http.Client aborts the moment its context ends. A
// fake that ignores the context would happily pass against a broken
// implementation — see blockingRender, which models the real behaviour.
type fakeProvider struct {
	mu     sync.Mutex
	calls  atomic.Int64
	last   RenderRequest
	img    Image
	err    error
	render func(context.Context, RenderRequest) (Image, error)
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Render(ctx context.Context, req RenderRequest) (Image, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.last = req
	fn := p.render
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return p.img, p.err
}

// blockingRender is a provider that hangs until release is closed and — like a
// real HTTP client — dies if the context it was given ends first. It signals on
// entered once it is actually in flight.
func blockingRender(entered chan<- struct{}, release <-chan struct{}, img Image) func(context.Context, RenderRequest) (Image, error) {
	return func(ctx context.Context, _ RenderRequest) (Image, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return img, nil
		case <-ctx.Done():
			return Image{}, ctx.Err()
		}
	}
}

func (p *fakeProvider) lastRequest() RenderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func ptr[T any](v T) *T { return &v }
