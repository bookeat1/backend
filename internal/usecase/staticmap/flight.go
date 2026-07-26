package staticmap

import "sync"

// flightGroup collapses concurrent identical renders into one provider call.
//
// This is the ~40-line subset of golang.org/x/sync/singleflight that this
// package needs; x/sync is only an indirect dependency of this module and one
// map plus one channel is not worth promoting it to a direct one.
//
// It exists for money, not for speed: on a cold cache a popular venue's screen
// can be opened by hundreds of guests within the same second, and without this
// every one of them is a separately billed provider request for the exact same
// picture.
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

// do runs fn for key, or waits for the in-progress call with the same key and
// returns its result. The waiters share the leader's outcome, including its
// error — a provider outage is not worth N retries in the same instant.
func (g *flightGroup) do(key string, fn func() (Image, error)) (Image, error) {
	g.mu.Lock()
	if f, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-f.done
		return f.img, f.err
	}
	f := &flight{done: make(chan struct{})}
	g.calls[key] = f
	g.mu.Unlock()

	f.img, f.err = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	close(f.done)

	return f.img, f.err
}
