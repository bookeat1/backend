package media

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeStore is an in-memory object store that records every mutation and
// screams if anything touches an original.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	// puts counts writes; originalsTouched records any write to a key that
	// existed before the run and is not a derivative.
	puts             int
	originalsTouched []string
	// original snapshot, to prove at the end that not a byte changed.
	snapshot map[string]string
	getErr   map[string]error
	putErr   map[string]error
}

func newFakeStore(objects map[string][]byte) *fakeStore {
	s := &fakeStore{
		objects:  map[string][]byte{},
		snapshot: map[string]string{},
		getErr:   map[string]error{},
		putErr:   map[string]error{},
	}
	for k, v := range objects {
		cp := make([]byte, len(v))
		copy(cp, v)
		s.objects[k] = cp
		s.snapshot[k] = string(cp)
	}
	return s
}

func (s *fakeStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *fakeStore) Head(_ context.Context, key string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return 0, false, nil
	}
	return int64(len(b)), true, nil
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.getErr[key]; err != nil {
		return nil, err
	}
	b, ok := s.objects[key]
	if !ok {
		return nil, errors.New("no such key: " + key)
	}
	return b, nil
}

func (s *fakeStore) Put(_ context.Context, key string, body []byte, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.putErr[key]; err != nil {
		return err
	}
	if !IsDerived(key) {
		s.originalsTouched = append(s.originalsTouched, key)
		return ErrNotDerived
	}
	if _, existed := s.snapshot[key]; existed {
		s.originalsTouched = append(s.originalsTouched, key)
	}
	s.puts++
	cp := make([]byte, len(body))
	copy(cp, body)
	s.objects[key] = cp
	return nil
}

// assertOriginalsIntact is the check that matters more than any assertion
// about output: every object that existed before the run still exists, byte
// for byte.
func (s *fakeStore) assertOriginalsIntact(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.originalsTouched) > 0 {
		t.Fatalf("backfill wrote over pre-existing objects: %v", s.originalsTouched)
	}
	for k, want := range s.snapshot {
		got, ok := s.objects[k]
		if !ok {
			t.Fatalf("original %q disappeared", k)
		}
		if string(got) != want {
			t.Fatalf("original %q was modified", k)
		}
	}
}

func bucketFixture(t *testing.T) *fakeStore {
	t.Helper()
	return newFakeStore(map[string][]byte{
		"restaurants/a/big.jpg":   sourceJPEG(t, 3000, 2000),
		"restaurants/a/small.jpg": sourceJPEG(t, 400, 300),
		"menu/b/dish.jpg":         sourceJPEG(t, 2000, 2000),
		"events/c/banner.jpg":     sourceJPEG(t, 1800, 900),
	})
}

func countBy(rep Report, o Outcome) int { return rep.Counts[o] }

// Dry run is the DEFAULT: a zero-valued Options must not write.
func TestBackfillDryRunIsTheDefaultAndWritesNothing(t *testing.T) {
	store := bucketFixture(t)

	rep, err := Backfill(context.Background(), store, Options{})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	if store.puts != 0 {
		t.Fatalf("dry run performed %d writes, want 0", store.puts)
	}
	store.assertOriginalsIntact(t)

	// It must still say precisely what it WOULD do.
	if got := countBy(rep, OutcomeCreated); got == 0 {
		t.Fatal("dry run planned nothing; expected it to list the derivatives it would create")
	}
	for _, r := range rep.Results {
		if r.Outcome == OutcomeCreated && !strings.Contains(r.Reason, "dry run") {
			t.Fatalf("dry run result does not say it is a dry run: %+v", r)
		}
		if r.Outcome == OutcomeCreated && !IsDerived(r.DerivedKey) {
			t.Fatalf("planned to create a non-derivative key: %q", r.DerivedKey)
		}
	}
}

func TestBackfillApplyCreatesDerivativesAndNeverTouchesOriginals(t *testing.T) {
	store := bucketFixture(t)

	rep, err := Backfill(context.Background(), store, Options{Apply: true, Concurrency: 4})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	store.assertOriginalsIntact(t)

	if store.puts == 0 {
		t.Fatal("apply run wrote nothing")
	}
	// Every written object is under derived/.
	store.mu.Lock()
	for k := range store.objects {
		if _, pre := store.snapshot[k]; !pre && !IsDerived(k) {
			store.mu.Unlock()
			t.Fatalf("new object %q created outside derived/", k)
		}
	}
	store.mu.Unlock()

	// The 400x300 original is below both sizes: two too-small skips, logged.
	if got := countBy(rep, OutcomeTooSmall); got != 2 {
		t.Fatalf("too-small skips = %d, want 2", got)
	}
	for _, r := range rep.Results {
		if r.Outcome == OutcomeTooSmall && r.Reason == "" {
			t.Fatalf("skip logged without a reason: %+v", r)
		}
	}

	if rep.DerivedBytes >= rep.OriginalBytes {
		t.Fatalf("derived total %d B is not smaller than original total %d B", rep.DerivedBytes, rep.OriginalBytes)
	}
	t.Logf("originals %d B -> derivatives %d B (%.1f%% of the bytes)",
		rep.OriginalBytes, rep.DerivedBytes, 100*float64(rep.DerivedBytes)/float64(rep.OriginalBytes))
}

// Safe to run twice: the second run writes nothing and says why.
func TestBackfillIsIdempotent(t *testing.T) {
	store := bucketFixture(t)

	if _, err := Backfill(context.Background(), store, Options{Apply: true}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst := store.puts

	rep, err := Backfill(context.Background(), store, Options{Apply: true})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if store.puts != afterFirst {
		t.Fatalf("second run wrote %d more objects, want 0", store.puts-afterFirst)
	}
	store.assertOriginalsIntact(t)

	if countBy(rep, OutcomeCreated) != 0 {
		t.Fatalf("second run claims to have created %d objects", countBy(rep, OutcomeCreated))
	}
	if countBy(rep, OutcomeExists) == 0 {
		t.Fatal("second run did not report the existing derivatives it skipped")
	}
}

// An interrupted run must be resumable: whatever it managed to write stays,
// and a rerun completes the rest.
func TestBackfillResumesAfterAnInterruptedRun(t *testing.T) {
	store := bucketFixture(t)

	// Simulate the interruption: one specific derivative refuses to be written.
	blocked := DerivedKey("menu/b/dish.jpg", WidthLarge)
	store.putErr[blocked] = errors.New("connection reset")

	rep, err := Backfill(context.Background(), store, Options{Apply: true})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if countBy(rep, OutcomeFailed) != 1 {
		t.Fatalf("failures = %d, want 1", countBy(rep, OutcomeFailed))
	}
	// One object failing must not stop the others.
	if countBy(rep, OutcomeCreated) == 0 {
		t.Fatal("a single failure aborted the whole run")
	}

	// The obstacle clears; the rerun fills exactly the gap.
	delete(store.putErr, blocked)
	rep2, err := Backfill(context.Background(), store, Options{Apply: true})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if countBy(rep2, OutcomeCreated) != 1 {
		t.Fatalf("rerun created %d, want exactly the 1 that had failed", countBy(rep2, OutcomeCreated))
	}
	if _, ok, _ := store.Head(context.Background(), blocked); !ok {
		t.Fatalf("%q still missing after the rerun", blocked)
	}
	store.assertOriginalsIntact(t)
}

// Running over a bucket that already contains derivatives must not derive them
// again, or every run multiplies the object count.
func TestBackfillNeverDerivesADerivative(t *testing.T) {
	store := bucketFixture(t)
	if _, err := Backfill(context.Background(), store, Options{Apply: true}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Now walk EVERY prefix, derived/ included, the way a careless operator would.
	rep, err := Backfill(context.Background(), store, Options{
		Apply:    true,
		Prefixes: []string{""},
	})
	if err != nil {
		t.Fatalf("full-bucket run: %v", err)
	}
	for _, r := range rep.Results {
		if r.Outcome == OutcomeCreated {
			t.Fatalf("created %q on a full-bucket rerun", r.DerivedKey)
		}
	}
	if countBy(rep, OutcomeSkippedDerived) == 0 {
		t.Fatal("full-bucket run did not report skipping the derivatives it listed")
	}
	store.assertOriginalsIntact(t)
}

// A corrupt object must be logged and stepped over, not abort 771 good ones.
func TestBackfillLogsUndecodableObjectsAndContinues(t *testing.T) {
	store := bucketFixture(t)
	store.objects["restaurants/a/broken.jpg"] = []byte("not an image at all")
	store.snapshot["restaurants/a/broken.jpg"] = "not an image at all"

	rep, err := Backfill(context.Background(), store, Options{Apply: true})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if countBy(rep, OutcomeUnsupported) != 2 {
		t.Fatalf("unsupported = %d, want 2 (one per width)", countBy(rep, OutcomeUnsupported))
	}
	if countBy(rep, OutcomeCreated) == 0 {
		t.Fatal("a corrupt object aborted the run")
	}
	store.assertOriginalsIntact(t)
}

func TestBackfillLimitBoundsARun(t *testing.T) {
	store := bucketFixture(t)

	rep, err := Backfill(context.Background(), store, Options{Limit: 1})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range rep.Results {
		seen[r.OriginalKey] = true
	}
	if len(seen) != 1 {
		t.Fatalf("Limit 1 touched %d originals: %v", len(seen), seen)
	}
}

func TestBackfillStreamsProgress(t *testing.T) {
	store := bucketFixture(t)
	var mu sync.Mutex
	var streamed int

	rep, err := Backfill(context.Background(), store, Options{
		Concurrency: 4,
		OnResult: func(Result) {
			mu.Lock()
			streamed++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if streamed != len(rep.Results) {
		t.Fatalf("streamed %d results, report holds %d", streamed, len(rep.Results))
	}
}

// The Store contract itself must refuse a non-derivative key.
func TestStoreRefusesToWriteOutsideDerivedPrefix(t *testing.T) {
	store := bucketFixture(t)
	err := store.Put(context.Background(), "restaurants/a/big.jpg", []byte("x"), "image/jpeg")
	if !errors.Is(err, ErrNotDerived) {
		t.Fatalf("Put to an original = %v, want ErrNotDerived", err)
	}
}
