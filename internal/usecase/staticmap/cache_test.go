package staticmap

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func img(n int) Image {
	return Image{Bytes: make([]byte, n), ContentType: "image/png", ETag: `"x"`}
}

func TestMemoryCacheRoundTrip(t *testing.T) {
	c := NewMemoryCache(time.Hour, 1<<20)
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get on an empty cache returned ok")
	}
	c.Set("a", img(10))
	got, ok := c.Get("a")
	if !ok || len(got.Bytes) != 10 {
		t.Fatalf("Get = %v/%d bytes, want a 10-byte hit", ok, len(got.Bytes))
	}
}

func TestMemoryCacheExpiresAfterTTL(t *testing.T) {
	c := NewMemoryCache(time.Minute, 1<<20)
	now := time.Now()
	c.now = func() time.Time { return now }

	c.Set("a", img(10))
	if _, ok := c.Get("a"); !ok {
		t.Fatal("fresh entry missing")
	}
	now = now.Add(time.Minute + time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry was served")
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d after expiry, want 0 (the entry must be dropped, not just hidden)", c.Len())
	}
}

// An image cache with no memory bound is a leak with extra steps.
func TestMemoryCacheEvictsLeastRecentlyUsedWithinBudget(t *testing.T) {
	c := NewMemoryCache(time.Hour, 300)
	c.Set("a", img(100))
	c.Set("b", img(100))
	c.Set("c", img(100))
	// Touch "a" so "b" becomes the least recently used.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a missing before eviction")
	}
	c.Set("d", img(100)) // over budget → one eviction

	if _, ok := c.Get("b"); ok {
		t.Error("b survived, want the least-recently-used entry evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s was evicted, want kept", k)
		}
	}
}

// A cache write must never fail a request, however awkward the input.
func TestMemoryCacheIgnoresUnstorableImages(t *testing.T) {
	c := NewMemoryCache(time.Hour, 100)
	c.Set("empty", Image{})
	c.Set("toobig", img(101))
	if c.Len() != 0 {
		t.Errorf("Len = %d, want 0", c.Len())
	}
	if _, ok := c.Get("toobig"); ok {
		t.Error("an over-budget image was stored")
	}
}

func TestMemoryCacheOverwriteKeepsBudgetAccounting(t *testing.T) {
	c := NewMemoryCache(time.Hour, 250)
	c.Set("a", img(100))
	c.Set("a", img(100)) // same key twice must not double-count
	c.Set("b", img(100))
	if _, ok := c.Get("a"); !ok {
		t.Error("a was evicted — the overwrite double-counted its bytes")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b missing")
	}
}

func TestMemoryCacheIsConcurrencySafe(t *testing.T) {
	c := NewMemoryCache(time.Hour, 1<<20)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := strconv.Itoa(i % 7)
			c.Set(k, img(64))
			c.Get(k)
		}(i)
	}
	wg.Wait()
}

func TestNewMemoryCacheFallsBackToDefaults(t *testing.T) {
	c := NewMemoryCache(0, 0)
	if c.TTL() != DefaultCacheTTL {
		t.Errorf("TTL = %v, want %v", c.TTL(), DefaultCacheTTL)
	}
	if c.maxBytes != DefaultCacheMaxBytes {
		t.Errorf("maxBytes = %d, want %d", c.maxBytes, DefaultCacheMaxBytes)
	}
}
