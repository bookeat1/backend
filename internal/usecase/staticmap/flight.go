package staticmap

import (
	"context"
	"sync"
)

// flightGroup collapses concurrent identical renders into one provider call.
//
// This is the small subset of golang.org/x/sync/singleflight that this package
// needs; x/sync is only an indirect dependency of this module and one map plus
// one channel is not worth promoting it to a direct one.
//
// It exists for money, not for speed: on a cold cache a popular venue's screen
// can be opened by hundreds of guests within the same second, and without this
// every one of them is a separately billed provider request for the exact same
// picture.
//
// # Whose context the shared work runs on, and why it is nobody's
//
// NOT the leader's. The naive version runs fn on whichever caller happened to
// win the race, and that caller's cancellation then kills the render for
// everyone: guest A opens a cold venue screen and navigates away, A's request
// context is cancelled, and guests B–Z — whose own connections are perfectly
// fine — all get "provider unavailable", because the HTTP call they were
// waiting on was made with A's context. On mobile networks, which is exactly
// where this app lives, that is not a corner case.
//
// So the shared work runs on a context detached from every caller
// (context.WithoutCancel: request-scoped VALUES such as the request id are kept
// so logging still correlates, cancellation and deadline are not). It stays
// bounded — by the provider client's own timeout — and it stays exactly one
// call. Each caller keeps its own cancellation for its own WAIT: a caller that
// goes away stops waiting immediately, it just no longer takes everybody else's
// render down with it.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flight
}

type flight struct {
	done chan struct{}
	img  Image
	err  error
}

func newFlightGroup() *flightGroup {
	return &flightGroup{calls: make(map[string]*flight)}
}

// do runs fn for key, or joins the in-progress call with the same key. fn
// receives a context detached from every caller (see the type doc). Waiters
// share the leader's outcome, including its error — a provider outage is not
// worth N retries in the same instant.
//
// A caller whose own ctx ends while waiting gets that context's error and stops
// waiting; the shared render carries on for whoever is left, and its result
// still lands in the cache (fn is what writes it), so the work is not wasted
// even if every original caller has walked away.
func (g *flightGroup) do(ctx context.Context, key string, fn func(context.Context) (Image, error)) (Image, error) {
	g.mu.Lock()
	f, inProgress := g.calls[key]
	if !inProgress {
		f = &flight{done: make(chan struct{})}
		g.calls[key] = f
		// Detached here, at creation: whoever created the flight may be gone
		// long before fn returns.
		work := context.WithoutCancel(ctx)
		go func() {
			defer close(f.done)
			img, err := fn(work)
			f.img, f.err = img, err
			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
		}()
	}
	g.mu.Unlock()

	select {
	case <-f.done:
		return f.img, f.err
	case <-ctx.Done():
		return Image{}, ctx.Err()
	}
}
