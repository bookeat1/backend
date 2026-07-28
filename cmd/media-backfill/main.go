// Command media-backfill generates the resized derivatives of the photos that
// are already sitting in the R2 bucket, so the apps can stop shipping upload
// originals to phones.
//
// WHAT IT DOES, IN ONE SENTENCE: for every original under restaurants/, menu/
// and events/, it writes derived/w640/<key>.jpg and derived/w1280/<key>.jpg if
// they are not there yet, and nothing else.
//
// WHAT IT CANNOT DO. It cannot delete, rename, or overwrite anything. The
// object store behind it (internal/infrastructure/mediastore) exposes no
// delete operation at all, and its Put refuses any key outside derived/. The
// bucket's originals are the only copies that exist; this tool is built so
// that a bug in it cannot cost one.
//
// DRY RUN IS THE DEFAULT. Without -apply it downloads and resizes nothing to
// the bucket — it lists exactly which objects it would create, at what size,
// and prints the totals. Adding -apply is the only way to write.
//
// IDEMPOTENT AND RESUMABLE. Progress lives in the bucket: an existing
// derivative is skipped, never rewritten. Interrupt it with Ctrl-C, or lose
// the connection halfway, and rerunning picks up exactly where it stopped. A
// second run over a finished bucket writes nothing and says "exists" for
// everything.
//
// Usage:
//
//	media-backfill                      # dry run over everything
//	media-backfill -limit 5             # dry run over the first 5 originals
//	media-backfill -limit 5 -apply      # really write, for 5 originals
//	media-backfill -apply               # really write, for the whole bucket
//	media-backfill -prefix scratch/     # confine a run to one prefix
//
// Flags:
//
//	-apply          write to the bucket (default false: dry run)
//	-prefix         repeatable; default restaurants/, menu/, events/
//	-limit          stop after N originals (0 = no limit)
//	-concurrency    objects in flight (default 8)
//	-quiet          totals only, no per-object lines
//
// Env (see internal/infrastructure/mediastore):
//
//	R2_ENDPOINT, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"

	"backend-core/internal/infrastructure/mediastore"
	"backend-core/internal/media"
)

type prefixList []string

func (p *prefixList) String() string     { return fmt.Sprint(*p) }
func (p *prefixList) Set(v string) error { *p = append(*p, v); return nil }

func main() {
	var (
		apply       = flag.Bool("apply", false, "write derivatives to the bucket (default: dry run, writes nothing)")
		limit       = flag.Int("limit", 0, "stop after N originals (0 = no limit)")
		concurrency = flag.Int("concurrency", 8, "objects processed at once")
		quiet       = flag.Bool("quiet", false, "print totals only")
		prefixes    prefixList
	)
	flag.Var(&prefixes, "prefix", "prefix to walk; repeatable (default restaurants/, menu/, events/)")
	flag.Parse()

	if err := run(*apply, *limit, *concurrency, *quiet, prefixes); err != nil {
		fmt.Fprintln(os.Stderr, "media-backfill:", err)
		os.Exit(1)
	}
}

func run(apply bool, limit, concurrency int, quiet bool, prefixes []string) error {
	// Ctrl-C cancels the context rather than killing the process outright, so
	// the run stops between objects and still prints its totals. Nothing is
	// left half-written either way: a PUT either completed or it did not, and
	// the next run redoes exactly the ones that did not.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := mediastore.ConfigFromEnv()
	if err != nil {
		return err
	}
	store, err := mediastore.New(ctx, cfg)
	if err != nil {
		return err
	}

	mode := "DRY RUN — nothing will be written"
	if apply {
		mode = "APPLY — derivatives will be written"
	}
	fmt.Printf("bucket %s\nmode:   %s\nsizes:  ", cfg.Bucket, mode)
	for i, w := range media.Widths {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("w%d", w)
	}
	fmt.Println()

	var mu sync.Mutex
	onResult := func(r media.Result) {
		if quiet {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Outcome {
		case media.OutcomeCreated:
			verb := "WOULD CREATE"
			if apply {
				verb = "CREATED"
			}
			fmt.Printf("%-12s %s  (%s -> %s)\n", verb, r.DerivedKey,
				humanBytes(r.OriginalSize), humanBytes(r.DerivedSize))
		default:
			// Every skip says what was skipped and why — a silent skip is
			// indistinguishable from a bug.
			fmt.Printf("%-12s %s  [%s] %s\n", "SKIP", keyOf(r), r.Outcome, r.Reason)
		}
	}

	report, runErr := media.Backfill(ctx, store, media.Options{
		Prefixes:    prefixes,
		Apply:       apply,
		Concurrency: concurrency,
		Limit:       limit,
		OnResult:    onResult,
	})

	printSummary(report, apply)

	if runErr != nil {
		return runErr
	}
	if report.Counts[media.OutcomeFailed] > 0 {
		// A non-zero exit so a wrapper script or a human notices, while the
		// successful part of the run is kept — rerunning is safe and cheap.
		return fmt.Errorf("%d derivative(s) failed; rerun to retry just those",
			report.Counts[media.OutcomeFailed])
	}
	return nil
}

func keyOf(r media.Result) string {
	if r.DerivedKey != "" {
		return r.DerivedKey
	}
	return r.OriginalKey
}

func printSummary(rep media.Report, apply bool) {
	fmt.Println("\n--- summary ---")

	outcomes := make([]string, 0, len(rep.Counts))
	for o := range rep.Counts {
		outcomes = append(outcomes, string(o))
	}
	sort.Strings(outcomes)
	for _, o := range outcomes {
		fmt.Printf("%-16s %d\n", o, rep.Counts[media.Outcome(o)])
	}

	if rep.OriginalBytes > 0 {
		pct := 100 * float64(rep.DerivedBytes) / float64(rep.OriginalBytes)
		fmt.Printf("\noriginals covered:  %s\nderivatives:        %s (%.1f%% of the original bytes)\n",
			humanBytes(rep.OriginalBytes), humanBytes(rep.DerivedBytes), pct)
		fmt.Printf("saved per full load: %s\n", humanBytes(rep.OriginalBytes-rep.DerivedBytes))
	}
	if !apply {
		fmt.Println("\nnothing was written (dry run). Rerun with -apply to write.")
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
