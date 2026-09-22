// Package perf holds the chain-scale performance probe.
//
// It is NOT part of the ordinary suite. Every test here is skipped unless
// CHIT_PERF is set, because a full run seeds and folds millions of events and
// takes tens of minutes. See README.md for how to run it and how to read the
// output.
//
// What it answers, and why it is committed rather than run ad hoc: chit's read
// cost is linear in chain length, so every number anyone quotes about this
// ledger is only true at the size it was measured. Re-running this is how a
// claim stays honest as the chain grows.
package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"ledger/internal/model"
	"ledger/internal/scaletest"
	"ledger/internal/store"
)

const slug = "perf"

// verbs is every read chit offers that a consumer actually calls. `ready` and
// `ls` read the PROJECTION blob; `status`, `show` and `notes` read the INDEX.
// Keeping both groups here is the point: the two blobs scale differently and a
// summary that mixes them hides which one is growing.
var verbs = []struct {
	name string
	args []string
}{
	{"ready", []string{"ready", "--limit", "500"}},
	{"ls", []string{"ls"}},
	{"status", []string{"status"}},
	{"show", []string{"show"}},
	{"notes --kind", []string{"notes", "--kind", "framing", "--limit", "500"}},
	{"rollup", []string{"rollup", slug}},
	{"where", []string{"where"}},
	{"tail", []string{"tail"}},
}

// boardRender mirrors what one dgd board render actually costs: src/dgd/view.py
// makes 14 chit calls per render - one `ready` (view.py:838), one `show`
// (1040), and twelve kind-filtered `notes` sweeps (1052-1067 and 3083-3274).
// The per-verb ratios above flatter the cache; this is the number an operator
// feels, and it stays dominated by 14 process starts no matter how fast each
// call becomes.
func boardRender() [][]string {
	calls := [][]string{
		{"ready", "--limit", "500"},
		{"show"},
	}
	for i := 0; i < 12; i++ {
		calls = append(calls, []string{"notes", "--kind", "framing", "--limit", "500"})
	}
	return calls
}

// Default sizes are geometric - 12.5k, 25k, 50k, 200k, 1M - so curvature reads
// at a glance: if cost is linear in chain length then a doubling of events
// should double the cold numbers, and a step that does not is the finding. Two
// points can only ever draw a straight line.
//
// The low end is deliberately dense. Three 2x steps in a row (12.5k, 25k, 50k)
// is what separates the INTERCEPT from the SLOPE: fixed cost per invocation -
// process start, store open - against cost that grows with the chain. Spread
// the points out and the two are indistinguishable, which is how "chit is
// slow" gets believed when the truth is "there are fourteen of them".
//
// 12,500 is NOT a real test. It is the baseline: a chain that size is nothing,
// so whatever a verb costs there is very nearly all fixed cost, and that row
// is the intercept every other row is read against. Treat it as the zero mark
// on the ruler rather than a workload anyone cares about.
//
// It happens to sit just under the real dgd ledger - 17,177 events on
// 2026-09-22 - which is useful for a different reason: it says plainly that
// today's store is still in the region where chain length barely matters, and
// that the interesting rows are 200k and 1M.
func sizes(t *testing.T) []int {
	raw := os.Getenv("CHIT_PERF_SIZES")
	if raw == "" {
		raw = "12500,25000,50000,200000,1000000"
	}
	var out []int
	for _, f := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			t.Fatalf("CHIT_PERF_SIZES: %q is not a positive integer", f)
		}
		out = append(out, n)
	}
	return out
}

func TestChainScale(t *testing.T) {
	if os.Getenv("CHIT_PERF") == "" {
		t.Skip("perf probe: set CHIT_PERF=1 (see internal/perf/README.md)")
	}
	bin := os.Getenv("CHIT_PERF_BIN")
	if bin == "" {
		t.Fatal("set CHIT_PERF_BIN to a built chit binary")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("CHIT_PERF_BIN %q: %v", bin, err)
	}

	for _, n := range sizes(t) {
		runOneSize(t, bin, n)
	}
}

func runOneSize(t *testing.T, bin string, n int) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v: %s", dir, err, out)
	}
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}

	evs := scaletest.Churn(n)
	meta := model.Meta{
		Slug: slug, Scope: "perf probe", Created: evs[0].TS, CreatedBy: "perf",
		Fields:      map[string][]string{"status": {"open", "in-progress", "closed", "wontfix"}},
		FieldOrder:  []string{"status"},
		MultiFields: []string{"labels", "blocked-by"},
		Terminal:    map[string][]string{"status": {"closed", "wontfix"}},
		Guard:       []string{"status", "blocked-by"},
		StaleAfter:  "2h",
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}

	seedStart := time.Now()
	scaletest.Seed(t, res.Store.Repo, slug, evs, map[string]string{"meta.json": string(mj)})
	res.Store.Repo.Git("", "gc", "--quiet")
	seeded := time.Since(seedStart)

	gitDir := filepath.Join(dir, ".ledger.git")
	if _, err := os.Stat(gitDir); err != nil {
		gitDir = filepath.Join(dir, ".git")
	}

	dropRefs := func() {
		for _, r := range []string{"refs/ledger-cache/" + slug, "refs/ledger-cache-index/" + slug} {
			_ = exec.Command("git", "--git-dir", gitDir, "update-ref", "-d", r).Run()
		}
	}
	run := func(args []string) time.Duration {
		c := exec.Command(bin, append([]string{"--store", dir}, args...)...)
		c.Env = append(os.Environ(), "LEDGER_NO_UPDATE_CHECK=1")
		st := time.Now()
		_, _ = c.CombinedOutput()
		return time.Since(st)
	}
	// Cold is measured ONCE: a cold call at 1,000,000 events is tens of
	// seconds and the variance between repeats is far smaller than the
	// number itself. Warm is cheap, so take a median of three.
	median := func(args []string, reps int, cold bool) time.Duration {
		ds := make([]time.Duration, 0, reps)
		for i := 0; i < reps; i++ {
			if cold {
				dropRefs()
			}
			ds = append(ds, run(args))
		}
		sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
		return ds[len(ds)/2]
	}
	gitOut := func(a ...string) string {
		out, _ := exec.Command("git", append([]string{"--git-dir", gitDir}, a...)...).Output()
		return strings.TrimSpace(string(out))
	}
	blobSize := func(ref string) int {
		sha := gitOut("rev-parse", ref)
		if sha == "" {
			return -1
		}
		v, _ := strconv.Atoi(gitOut("cat-file", "-s", sha))
		return v
	}

	fmt.Printf("\n===== chain = %d events =====\n", n)
	fmt.Printf("  seeded in %s\n", seeded.Round(time.Millisecond))

	cold := make([]time.Duration, len(verbs))
	for i, v := range verbs {
		cold[i] = median(v.args, 1, true)
	}

	dropRefs()
	reset := run([]string{"cache", "reset", slug})
	proj := blobSize("refs/ledger-cache/" + slug)
	index := blobSize("refs/ledger-cache-index/" + slug)

	warm := make([]time.Duration, len(verbs))
	for i, v := range verbs {
		warm[i] = median(v.args, 3, false)
	}

	fmt.Printf("  cache reset (builds both refs): %s\n", reset.Round(time.Millisecond))
	fmt.Printf("  projection blob %d bytes   index blob %d bytes\n", proj, index)
	fmt.Printf("  %-14s %11s %11s %9s\n", "verb", "cold", "warm", "ratio")
	for i, v := range verbs {
		ratio := 0.0
		if warm[i] > 0 {
			ratio = float64(cold[i]) / float64(warm[i])
		}
		fmt.Printf("  %-14s %11s %11s %8.1fx\n", v.name,
			cold[i].Round(time.Millisecond), warm[i].Round(time.Millisecond), ratio)
	}

	calls := boardRender()
	dropRefs()
	var bCold time.Duration
	for _, c := range calls {
		bCold += run(c)
	}
	run([]string{"cache", "reset", slug})
	var bWarm time.Duration
	for _, c := range calls {
		bWarm += run(c)
	}
	ratio := 0.0
	if bWarm > 0 {
		ratio = float64(bCold) / float64(bWarm)
	}
	fmt.Printf("  BOARD RENDER, %d calls: cold %s   warm %s   %.1fx\n",
		len(calls), bCold.Round(time.Millisecond), bWarm.Round(time.Millisecond), ratio)
}
