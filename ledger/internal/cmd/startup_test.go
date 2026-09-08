package cmd

// Where chit's ~35 ms of startup actually goes.
//
// Dossier 11.3 narrows it by SUBTRACTION - 32.1 ms measured, minus an 8.8 ms
// Go-binary floor, minus 3.0 ms of package init, leaving ~20 ms attributed to
// Execute() and hypothesised to be the command registry. Subtraction is not a
// measurement, and 11.3 says so.
//
// This measures it directly instead, in-process, so no process startup is in
// the number at all - which is strictly better than subtracting a floor
// measured from a different binary built with a different toolchain.

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func median(d []time.Duration) time.Duration {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
	return d[len(d)/2]
}

// TestStartupCostBreakdown reports, it does not assert. A threshold here would
// be a machine-speed test; the point is the SHARE each part takes, which is
// what decides whether lazy command construction is worth a patch.
func TestStartupCostBreakdown(t *testing.T) {
	const reps = 200
	// Both escapes ON, matching how the factory actually runs chit. Without
	// them CheckVersion spawns `git --version` and passiveUpdateCheck runs,
	// and the in-process number comes out LARGER than the whole process.
	t.Setenv("LEDGER_NO_VERSION_CHECK", "1")
	t.Setenv("LEDGER_NO_UPDATE_CHECK", "1")

	// 1. The whole in-process command path for `version`, which root.go
	//    exempts from store resolution: no store resolved, no git spawned, no
	//    config read. Whatever this costs is Execute() and nothing else.
	full := make([]time.Duration, 0, reps)
	for i := 0; i < reps; i++ {
		var so, se bytes.Buffer
		start := time.Now()
		if code := ExecuteArgs([]string{"version"}, &so, &se); code != 0 {
			t.Fatalf("version failed: %d %s", code, se.String())
		}
		full = append(full, time.Since(start))
	}

	// 2. Just the registry loop: build every command, add none. This is the
	//    hypothesised culprit - 23 constructors, each building a
	//    *cobra.Command and its flag set, all of them run before cobra parses
	//    argv and discovers 22 were irrelevant.
	reg := make([]time.Duration, 0, reps)
	for i := 0; i < reps; i++ {
		ctx := &Ctx{Stdout: io.Discard, Stderr: io.Discard}
		root := &cobra.Command{Use: "chit"}
		start := time.Now()
		for _, f := range registry {
			root.AddCommand(f(ctx))
		}
		reg = append(reg, time.Since(start))
	}

	// 3. Construction alone, without cobra's AddCommand bookkeeping, to say
	//    whether the cost is ours or cobra's.
	build := make([]time.Duration, 0, reps)
	for i := 0; i < reps; i++ {
		ctx := &Ctx{Stdout: io.Discard, Stderr: io.Discard}
		start := time.Now()
		for _, f := range registry {
			_ = f(ctx)
		}
		build = append(build, time.Since(start))
	}

	mf, mr, mb := median(full), median(reg), median(build)
	t.Logf("registered commands              : %d", len(registry))
	t.Logf("ExecuteArgs([version]) in-process: %8.3f ms", float64(mf.Microseconds())/1000)
	t.Logf("  registry loop (build + Add)    : %8.3f ms  (%.1f%% of the above)",
		float64(mr.Microseconds())/1000, 100*float64(mr)/float64(mf))
	t.Logf("  constructors only (no Add)     : %8.3f ms", float64(mb.Microseconds())/1000)
	t.Logf("  everything else in ExecuteArgs : %8.3f ms", float64((mf-mr).Microseconds())/1000)
}

// TestStartupCostWithoutEscapes shows what the two env escapes are worth, and
// why an in-process measurement taken without them exceeds the whole process:
// CheckVersion spawns `git --version` and passiveUpdateCheck runs.
func TestStartupCostWithoutEscapes(t *testing.T) {
	const reps = 60
	d := make([]time.Duration, 0, reps)
	for i := 0; i < reps; i++ {
		var so, se bytes.Buffer
		start := time.Now()
		ExecuteArgs([]string{"version"}, &so, &se)
		d = append(d, time.Since(start))
	}
	t.Logf("ExecuteArgs([version]) with NO escapes: %8.3f ms", float64(median(d).Microseconds())/1000)
}
