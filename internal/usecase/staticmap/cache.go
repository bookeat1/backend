package staticmap

import (
	"container/list"
	"sync"
	"time"
)

// MemoryCache is the default Cache: an in-process, TTL'd, byte-budgeted LRU.
//
// Why in-process and not Redis/S3: this repo has no cache infrastructure at
// all, and a static map image is the cheapest possible thing to lose — a miss
// costs one provider request, not correctness. An in-process cache needs no new
// dependency, no new failure mode and no new ops surface. The cost is that N
// application instances warm N caches independently (N provider requests per
// image per TTL instead of one); at BookEat's instance count that is
// negligible, and the Cache port is there for the day it is not.
//
// Bounded two ways, because an unbounded image cache is a memory leak with
// extra steps: entries expire after ttl, and the total stored byte count never
// exceeds maxBytes (least-recently-used entries are dropped first).
type MemoryCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxBytes int64
	bytes    int64
	items    map[string]*list.Element
	order    *list.List // front = most recently used

	// now is injectable so TTL behaviour is testable without sleeping.
	now func() time.Time
}

type cacheEntry struct {
	key       string
	img       Image
	expiresAt time.Time
	size      int64
}

// Cache defaults. 24h because a restaurant's coordinates change roughly never
// (see UseCase.Render on why the cache key deliberately does NOT include the
// coordinates), and 64 MiB because that is a few hundred renders — far more
// than the catalog has venues — while staying a rounding error next to the
// process's normal footprint.
const (
	DefaultCacheTTL      = 24 * time.Hour
	DefaultCacheMaxBytes = int64(64 << 20)
)

// NewMemoryCache builds the in-process cache. Non-positive ttl/maxBytes fall
// back to the defaults, so a misconfigured env var degrades to "sane", never to
// "cache everything forever".
func NewMemoryCache(ttl time.Duration, maxBytes int64) *MemoryCache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	if maxBytes <= 0 {
		maxBytes = DefaultCacheMaxBytes
	}
	return &MemoryCache{
		ttl:      ttl,
		maxBytes: maxBytes,
		items:    make(map[string]*list.Element),
		order:    list.New(),
		now:      time.Now,
	}
}

// TTL reports the configured entry lifetime. The transport layer reuses it as
// the HTTP max-age so a client and the server agree on how stale a map may get.
func (c *MemoryCache) TTL() time.Duration { return c.ttl }

// Get returns the cached image, or ok=false on a miss or an expired entry.
func (c *MemoryCache) Get(key string) (Image, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Image{}, false
	}
	e := el.Value.(*cacheEntry)
	if !c.now().Before(e.expiresAt) {
		c.removeElement(el)
		return Image{}, false
	}
	c.order.MoveToFront(el)
	return e.img, true
}

// Set stores img under key, evicting least-recently-used entries until the
// budget fits. An image larger than the whole budget is simply not cached — a
// cache write never fails a request.
func (c *MemoryCache) Set(key string, img Image) {
	sz := int64(len(img.Bytes))
	if sz == 0 || sz > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
	e := &cacheEntry{key: key, img: img, expiresAt: c.now().Add(c.ttl), size: sz}
	c.items[key] = c.order.PushFront(e)
	c.bytes += sz
	for c.bytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
	}
}

// Len reports how many entries are currently held (test/observability helper).
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// removeElement drops el. Caller holds the mutex.
func (c *MemoryCache) removeElement(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.order.Remove(el)
	delete(c.items, e.key)
	c.bytes -= e.size
}
