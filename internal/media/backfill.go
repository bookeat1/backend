package media

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Store is the slice of an object store this package needs. It is an
// interface for one reason above all others: it lets the backfill be tested
// against a fake, so nobody has to point the real thing at the real bucket to
// find out whether it deletes things.
//
// Note what is NOT here. There is no Delete and no Move. The backfill
// physically cannot remove or rename an object, because the vocabulary to do
// so was never handed to it. The originals in this bucket are the only copies
// that exist; "we were careful" is a weaker guarantee than "the code has no
// way to express it".
type Store interface {
	// List yields every key under prefix, in any order.
	List(ctx context.Context, prefix string) ([]string, error)
	// Head returns the size of key, or ok=false when it does not exist.
	Head(ctx context.Context, key string) (size int64, ok bool, err error)
	// Get reads an object whole.
	Get(ctx context.Context, key string) ([]byte, error)
	// Put writes body at key. Implementations MUST refuse a key that is not a
	// derivative (see IsDerived); the caller checks too, and both checks are
	// deliberate.
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

// ErrNotDerived is what a Store must return when asked to write outside the
// derived/ prefix. Reaching it means a bug upstream, and the write must not
// happen.
var ErrNotDerived = errors.New("media: refusing to write outside the derived/ prefix")

// Outcome classifies what happened to one (original, width) pair.
type Outcome string

const (
	// OutcomeCreated — the derivative did not exist and was written (or, in a
	// dry run, would have been).
	OutcomeCreated Outcome = "created"
	// OutcomeExists — a derivative is already there. This is what makes a
	// second run cheap and an interrupted run resumable: progress is stored in
	// the bucket itself, not in a checkpoint file that can disagree with it.
	OutcomeExists Outcome = "exists"
	// OutcomeTooSmall — the original is already no wider than this size.
	OutcomeTooSmall Outcome = "too-small"
	// OutcomeUnsupported — the original could not be decoded.
	OutcomeUnsupported Outcome = "unsupported"
	// OutcomeFailed — an I/O error. Retryable: rerunning skips everything that
	// did succeed.
	OutcomeFailed Outcome = "failed"
	// OutcomeSkippedDerived — the listed key is itself a derivative.
	OutcomeSkippedDerived Outcome = "skipped-derived"
)

// Result is one line of the backfill's log: what it did to what, and why.
type Result struct {
	OriginalKey  string
	DerivedKey   string
	Width        int
	Outcome      Outcome
	Reason       string
	OriginalSize int64
	DerivedSize  int64
}

// Report aggregates a run.
type Report struct {
	Results []Result
	// Bytes that would be served before and after, counting only the pairs
	// where a derivative now exists or was created. Comparable numbers, not a
	// bucket total.
	OriginalBytes int64
	DerivedBytes  int64
	Counts        map[Outcome]int
}

// Options configures one backfill run.
type Options struct {
	// Prefixes to walk. Empty means every prefix holding originals.
	Prefixes []string
	// Widths to generate. Empty means the package default.
	Widths []int
	// Apply switches from planning to writing. The ZERO VALUE IS A DRY RUN —
	// this is why the flag is named Apply and not DryRun: a caller that forgets
	// to set the field, or a test that builds Options{} literally, gets the
	// harmless behaviour. A `DryRun bool` field would default to false and
	// write to the bucket.
	Apply bool
	// Concurrency is the number of objects processed at once. <=1 is serial.
	Concurrency int
	// Limit stops after this many ORIGINALS (not pairs). 0 means no limit. It
	// exists so a first real run can be tried on a handful of objects.
	Limit int
	// OnResult, if set, is called for every Result as it happens, from
	// multiple goroutines. Implementations must be safe for concurrent use.
	// This is what streams progress to a terminal instead of holding it all to
	// the end — an interrupted run should still have told you what it did.
	OnResult func(Result)
}

// DefaultPrefixes are the prefixes under which originals live in this bucket,
// verified by listing it on 2026-07-27: restaurants/ (55 objects, 81.6 MB),
// menu/ (714), events/ (3). `derived/` is deliberately absent.
var DefaultPrefixes = []string{"restaurants/", "menu/", "events/"}

// Backfill generates every missing derivative for the originals under the
// configured prefixes.
//
// SAFETY, restated because it is the whole point of this function:
//   - it only ever calls Put, and only with keys produced by DerivedKey, which
//     always begin with `derived/`;
//   - the Store interface has no delete or rename;
//   - an existing derivative is left ALONE rather than rewritten, so a rerun
//     cannot even churn its own output;
//   - with Apply=false nothing is written at all.
//
// Resumability comes free from that last-but-one rule: the bucket is the
// progress record. Kill the process at any point and rerun it; it picks up
// where it stopped, having skipped what it already did, and the log says
// "exists" for those.
func Backfill(ctx context.Context, store Store, opts Options) (Report, error) {
	prefixes := opts.Prefixes
	if len(prefixes) == 0 {
		prefixes = DefaultPrefixes
	}
	widths := opts.Widths
	if len(widths) == 0 {
		widths = Widths
	}

	var originals []string
	for _, p := range prefixes {
		keys, err := store.List(ctx, p)
		if err != nil {
			return Report{}, fmt.Errorf("list %q: %w", p, err)
		}
		originals = append(originals, keys...)
	}
	// Sorted so two runs walk the bucket in the same order: an interrupted run
	// restarted with a Limit makes progress instead of retrying a random
	// sample of the same objects.
	sort.Strings(originals)
	if opts.Limit > 0 && len(originals) > opts.Limit {
		originals = originals[:opts.Limit]
	}

	workers := opts.Concurrency
	if workers < 1 {
		workers = 1
	}

	var (
		mu      sync.Mutex
		report  = Report{Counts: map[Outcome]int{}}
		jobs    = make(chan string)
		wg      sync.WaitGroup
		firstMu sync.Mutex
		firstEr error
	)

	record := func(rs []Result) {
		mu.Lock()
		for _, r := range rs {
			report.Results = append(report.Results, r)
			report.Counts[r.Outcome]++
			if r.Outcome == OutcomeCreated || r.Outcome == OutcomeExists {
				report.OriginalBytes += r.OriginalSize
				report.DerivedBytes += r.DerivedSize
			}
		}
		mu.Unlock()
		if opts.OnResult != nil {
			for _, r := range rs {
				opts.OnResult(r)
			}
		}
	}

	noteErr := func(err error) {
		firstMu.Lock()
		if firstEr == nil {
			firstEr = err
		}
		firstMu.Unlock()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				rs, err := backfillOne(ctx, store, key, widths, opts.Apply)
				if err != nil && ctx.Err() != nil {
					noteErr(err)
				}
				record(rs)
			}
		}()
	}

	for _, key := range originals {
		if ctx.Err() != nil {
			break
		}
		jobs <- key
	}
	close(jobs)
	wg.Wait()

	sort.Slice(report.Results, func(i, j int) bool {
		if report.Results[i].OriginalKey != report.Results[j].OriginalKey {
			return report.Results[i].OriginalKey < report.Results[j].OriginalKey
		}
		return report.Results[i].Width < report.Results[j].Width
	})

	if firstEr != nil {
		return report, firstEr
	}
	return report, ctx.Err()
}

// backfillOne handles every width of a single original. It returns results
// rather than errors for anything object-specific: one corrupt file must not
// abort a run over 772 of them, it must be logged and stepped over.
func backfillOne(ctx context.Context, store Store, key string, widths []int, apply bool) ([]Result, error) {
	if IsDerived(key) {
		return []Result{{
			OriginalKey: key,
			Outcome:     OutcomeSkippedDerived,
			Reason:      "key is itself a derivative",
		}}, nil
	}

	out := make([]Result, 0, len(widths))

	// Which widths are actually missing? Answering this before downloading the
	// original is what makes a rerun over an already-done bucket cost a HEAD
	// per object instead of a full GET plus a decode.
	missing := make([]int, 0, len(widths))
	for _, w := range widths {
		dk := DerivedKey(key, w)
		if dk == "" {
			continue
		}
		size, ok, err := store.Head(ctx, dk)
		switch {
		case err != nil:
			out = append(out, Result{OriginalKey: key, DerivedKey: dk, Width: w,
				Outcome: OutcomeFailed, Reason: "head derivative: " + err.Error()})
		case ok:
			out = append(out, Result{OriginalKey: key, DerivedKey: dk, Width: w,
				Outcome: OutcomeExists, Reason: "derivative already present", DerivedSize: size})
		default:
			missing = append(missing, w)
		}
	}
	if len(missing) == 0 {
		return out, ctx.Err()
	}

	src, err := store.Get(ctx, key)
	if err != nil {
		for _, w := range missing {
			out = append(out, Result{OriginalKey: key, DerivedKey: DerivedKey(key, w), Width: w,
				Outcome: OutcomeFailed, Reason: "get original: " + err.Error()})
		}
		return out, err
	}
	origSize := int64(len(src))

	for _, w := range missing {
		dk := DerivedKey(key, w)
		r := Result{OriginalKey: key, DerivedKey: dk, Width: w, OriginalSize: origSize}

		rendered, err := Render(src, w)
		switch {
		case errors.Is(err, ErrTooSmall):
			r.Outcome, r.Reason = OutcomeTooSmall, err.Error()
			out = append(out, r)
			continue
		case errors.Is(err, ErrUnsupported):
			r.Outcome, r.Reason = OutcomeUnsupported, err.Error()
			out = append(out, r)
			continue
		case err != nil:
			r.Outcome, r.Reason = OutcomeFailed, "render: "+err.Error()
			out = append(out, r)
			continue
		}

		r.DerivedSize = int64(len(rendered.Bytes))

		// Belt and braces. DerivedKey cannot return a non-derived key, and the
		// Store refuses one too; this is the third check, and it is here
		// because the cost of being wrong once is an original that no longer
		// exists anywhere.
		if !IsDerived(dk) {
			r.Outcome, r.Reason = OutcomeFailed, "refusing to write outside derived/: "+dk
			out = append(out, r)
			continue
		}

		if !apply {
			r.Outcome, r.Reason = OutcomeCreated, "dry run: would create"
			out = append(out, r)
			continue
		}

		if err := store.Put(ctx, dk, rendered.Bytes, rendered.ContentType); err != nil {
			r.Outcome, r.Reason = OutcomeFailed, "put derivative: "+err.Error()
			out = append(out, r)
			if ctx.Err() != nil {
				return out, err
			}
			continue
		}
		r.Outcome, r.Reason = OutcomeCreated, "written"
		out = append(out, r)
	}

	return out, ctx.Err()
}
