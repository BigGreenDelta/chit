package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ledger/internal/cache"
	"ledger/internal/gitx"
	"ledger/internal/model"
	"ledger/internal/scaletest"
	"ledger/internal/store"
)

// ---------------------------------------------------------------------
// The fold cache (dgd-237). The load-bearing test here is the
// DIFFERENTIAL one: `ready --json` answered off refs/ledger-cache/<slug>
// must be byte-identical to `ready --json` with that ref deleted, across
// every DAG shape chit has fixtures for. A unit test of the cache would
// only prove the cache agrees with itself.
// ---------------------------------------------------------------------

// fixedAt pins the evaluation clock so two `ready` runs are comparable at
// all: the envelope renders ages and staleness, and those move with the
// wall clock between two invocations.
const fixedAt = "2026-09-01T00:00:00.000"

// cacheFixture is one DAG shape, seeded, ready to read.
type cacheFixture struct {
	name string
	dir  string
	slug string
	s    store.Store
}

// cacheFixtures builds the five shapes the task names: linear, merge,
// sentinel-bearing, packed and loose. Merge and sentinel-bearing are
// distinct on purpose - a sync sentinel with ONE parent (a contracted
// commit on a linear chain) exercises different code from the two-parent
// sentinel a real `chit sync` mints.
func cacheFixtures(t *testing.T) []cacheFixture {
	t.Helper()
	metaJSON := func(slug string) map[string]string {
		m := scaletest.Meta()
		m.Slug, m.Scope, m.Created, m.CreatedBy = slug, "cache test", "2026-08-01T00:00:00.000", "t"
		m.FieldOrder = []string{"status"}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{"meta.json": string(b)}
	}
	newStore := func(t *testing.T) (string, store.Store) {
		dir := initRepo(t)
		res, err := store.Resolve(dir)
		if err != nil {
			t.Fatal(err)
		}
		return dir, res.Store
	}

	var out []cacheFixture

	// linear: one plain chain, no merges, no sentinels.
	dir, s := newStore(t)
	scaletest.Seed(t, s.Repo, "board", scaletest.Churn(600), metaJSON("board"))
	out = append(out, cacheFixture{"linear", dir, "board", s})

	// merge: two branches off a base, joined by a real two-parent sync
	// sentinel - the only shape a contest can exist in, so this fixture is
	// also the one that proves cached contests survive a tail fold.
	dir, s = newStore(t)
	scaletest.SeedMerged(t, s.Repo, "board", scaletest.Churn(400),
		scaletest.Branch(500, 60, "alice"), scaletest.Branch(500, 60, "bob"), metaJSON("board"))
	out = append(out, cacheFixture{"merge", dir, "board", s})

	// sentinel-bearing: a LINEAR chain carrying single-parent sentinels  -
	// contracted out of the fold, never delivered, and the tail walk has to
	// step straight through them.
	dir, s = newStore(t)
	evs := scaletest.Churn(400)
	withSentinels := make([]model.Event, 0, len(evs)+4)
	for i, ev := range evs {
		if i > 0 && i%120 == 0 {
			withSentinels = append(withSentinels, model.Event{TS: ev.TS, Type: "sync", Author: "sync"})
		}
		withSentinels = append(withSentinels, ev)
	}
	scaletest.Seed(t, s.Repo, "board", withSentinels, metaJSON("board"))
	out = append(out, cacheFixture{"sentinel-bearing", dir, "board", s})

	// packed: the same linear chain, then `git gc` - refs in packed-refs,
	// objects in a packfile. The cache blob and its ref have to survive
	// being packed like anything else.
	dir, s = newStore(t)
	scaletest.Seed(t, s.Repo, "board", scaletest.Churn(300), metaJSON("board"))
	s.Repo.Git("", "gc", "--quiet")
	s.Repo.Git("", "pack-refs", "--all")
	out = append(out, cacheFixture{"packed", dir, "board", s})

	// loose: the same chain with no gc at all, so every object and ref is
	// loose on disk.
	dir, s = newStore(t)
	scaletest.Seed(t, s.Repo, "board", scaletest.Churn(300), metaJSON("board"))
	out = append(out, cacheFixture{"loose", dir, "board", s})

	return out
}

// readyJSON runs `ready --json` at the pinned clock and returns the raw
// stdout bytes - the comparison is on BYTES, not on a decoded document,
// because a re-encode would hide exactly the ordering differences the
// envelope's determinism rules exist to prevent.
func readyJSON(t *testing.T, f cacheFixture) string {
	t.Helper()
	so, se, code := run(t, f.dir, "ready", "--ledger", f.slug, "--at", fixedAt, "--limit", "1000")
	if code != 0 {
		t.Fatalf("%s: ready: code=%d %s", f.name, code, se)
	}
	return so
}

func deleteCacheRef(t *testing.T, f cacheFixture) {
	t.Helper()
	if err := f.s.DeleteRef(store.CacheRef(f.slug)); err != nil {
		t.Fatal(err)
	}
}

func mustReset(t *testing.T, f cacheFixture) {
	t.Helper()
	if _, err := f.s.CacheSource(f.slug).Reset(); err != nil {
		t.Fatalf("%s: cache reset: %v", f.name, err)
	}
}

func mustOrigin(t *testing.T, f cacheFixture, want cache.Origin) cache.Projection {
	t.Helper()
	p, err := f.s.Projection(f.slug)
	if err != nil {
		t.Fatalf("%s: projection: %v", f.name, err)
	}
	if p.Origin != want {
		t.Fatalf("%s: read came from %q, want %q", f.name, p.Origin, want)
	}
	return p
}

// appendTail writes n ordinary events through the real store append path  -
// the write path, cache hook included.
func appendTail(t *testing.T, f cacheFixture, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ev := scaletest.Event(900000+i, "k-1", "status", "in-progress", "tailwriter")
		if _, err := f.s.Append(f.slug, ev, nil, store.ExpectPresent); err != nil {
			t.Fatalf("%s: append: %v", f.name, err)
		}
	}
}

// TestReadyOffCacheIsByteIdenticalToFoldFromRoot is criterion 3: the
// differential test, over every DAG shape, in all three read branches the
// cache can answer from.
func TestReadyOffCacheIsByteIdenticalToFoldFromRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("cache differential")
	}
	for _, f := range cacheFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			// Reference: no cache ref at all.
			deleteCacheRef(t, f)
			mustOrigin(t, f, cache.OriginRoot)
			want := readyJSON(t, f)

			// Branch 1: the blob's base IS the head.
			mustReset(t, f)
			mustOrigin(t, f, cache.OriginCache)
			if got := readyJSON(t, f); got != want {
				t.Fatalf("%s: exact-hit read differs from a fold from root\n got %s\nwant %s", f.name, got, want)
			}

			// The merge fixture must actually CARRY contests, or the
			// cached-contest path below is untested and this subtest is
			// quietly weaker than it looks.
			if f.name == "merge" && !strings.Contains(want, `"contested"`) {
				t.Fatalf("merge fixture carries no contested entry - the cached-contest "+
					"path is not being exercised: %s", want)
			}

			// Branch 2: the blob's base is behind head by a bounded,
			// merge-free run - the tail fold.
			appendTail(t, f, 5)
			p := mustOrigin(t, f, cache.OriginTail)
			if p.Count == 0 {
				t.Fatalf("%s: tail read folded no events", f.name)
			}
			withTail := readyJSON(t, f)
			deleteCacheRef(t, f)
			mustOrigin(t, f, cache.OriginRoot)
			if fromRoot := readyJSON(t, f); withTail != fromRoot {
				t.Fatalf("%s: tail-folded read differs from a fold from root\n got %s\nwant %s",
					f.name, withTail, fromRoot)
			}
		})
	}
}

// TestCacheIgnoredWhenBaseIsForeign is criterion 4 - the branch that makes
// a PUSHED cache safe without a handshake. A blob whose base names an
// unrelated root is not a stale cache to be repaired; it is a cache
// describing a different history, and the only sound answer is to ignore it
// entirely.
func TestCacheIgnoredWhenBaseIsForeign(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	seedBoard(t, dir, "board")
	seedBoard(t, dir, "other")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	mustRun(t, dir, "set", "k-2", "status=open", "--expect", "none", "--ledger", "other", "-m", "two", "--as", "alice")

	f := cacheFixture{"foreign", dir, "board", s}
	deleteCacheRef(t, f)
	want := readyJSON(t, f)

	// Forge a cache blob for `board` whose base is `other`'s root - a
	// commit this ledger's history has never heard of, which is exactly
	// what a cache pushed from a foreign store looks like.
	otherRoots := s.Roots(store.Ref("other"))
	if len(otherRoots) != 1 {
		t.Fatalf("fixture: want one root on 'other', got %v", otherRoots)
	}
	forged := forgeCacheBlob(t, s, "board", otherRoots[0])
	if err := s.UpdateRefForce(store.CacheRef("board"), forged); err != nil {
		t.Fatal(err)
	}

	mustOrigin(t, f, cache.OriginRoot)
	if got := readyJSON(t, f); got != want {
		t.Fatalf("a foreign-base cache must be ignored, not trusted\n got %s\nwant %s", got, want)
	}
}

// TestCacheIgnoredWhenBaseIsAheadOfHead is the third refusal in the same
// family: a peer's cache arriving before the commits it describes. The
// walk runs from head BACKWARD, so a base ahead of head is simply never
// reached - no ancestry query, no extra subprocess.
func TestCacheIgnoredWhenBaseIsAheadOfHead(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	behind, ok := s.FullHead("board")
	if !ok {
		t.Fatal("no head")
	}
	mustRun(t, dir, "set", "k-2", "status=open", "--expect", "none", "--ledger", "board", "-m", "two", "--as", "alice")

	f := cacheFixture{"ahead", dir, "board", s}
	mustReset(t, f) // cache base is now the newer head
	if err := s.UpdateRefForce(store.Ref("board"), behind); err != nil {
		t.Fatal(err)
	}
	mustOrigin(t, f, cache.OriginRoot)
}

// TestCacheIgnoredWhenRangeHoldsAMerge is the soundness refusal the whole
// tail fold rests on. A merge inside base..head attaches to the cached
// prefix's INTERIOR, so dag.Sort could interleave new commits into the
// cached order and the prefix would no longer be a prefix. The walk refuses
// it and the read folds from root.
func TestCacheIgnoredWhenRangeHoldsAMerge(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	m := scaletest.Meta()
	m.Slug, m.Scope, m.Created, m.CreatedBy = "board", "cache test", "2026-08-01T00:00:00.000", "t"
	m.FieldOrder = []string{"status"}
	mj, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	base := scaletest.Churn(60)
	scaletest.SeedMerged(t, s.Repo, "board", base,
		scaletest.Branch(200, 20, "alice"), scaletest.Branch(200, 20, "bob"),
		map[string]string{"meta.json": string(mj)})

	f := cacheFixture{"merge-in-range", dir, "board", s}
	// Base the cache on the commit the two branches diverged from - the
	// merge-base of the sentinel's two parents. Every later commit descends
	// from it, and the merge sits strictly between it and the head, which is
	// exactly the range the walk must refuse.
	merge := strings.TrimSpace(gitOutput(t, s, "rev-list", "--merges", "--max-count=1", store.Ref("board")))
	if merge == "" {
		t.Fatal("fixture: no merge commit in the seeded chain")
	}
	parents := strings.Fields(strings.TrimSpace(gitOutput(t, s, "rev-list", "--parents", "--max-count=1", merge)))
	if len(parents) != 3 {
		t.Fatalf("fixture: merge %s has parents %v", merge, parents)
	}
	baseTip := strings.TrimSpace(gitOutput(t, s, "merge-base", parents[1], parents[2]))
	if baseTip == "" {
		t.Fatal("fixture: could not locate the pre-merge commit")
	}
	forged := forgeCacheBlob(t, s, "board", baseTip)
	if err := s.UpdateRefForce(store.CacheRef("board"), forged); err != nil {
		t.Fatal(err)
	}
	mustOrigin(t, f, cache.OriginRoot)
}

// TestCacheIgnoredWhenTailExceedsTheBound: past WalkBound the cache is
// ignored, so a stale cache can never turn a cheap read into a long walk.
func TestCacheIgnoredWhenTailExceedsTheBound(t *testing.T) {
	if testing.Short() {
		t.Skip("cache bound")
	}
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	m := scaletest.Meta()
	m.Slug, m.Scope, m.Created, m.CreatedBy = "board", "cache test", "2026-08-01T00:00:00.000", "t"
	m.FieldOrder = []string{"status"}
	mj, _ := json.Marshal(m)
	all := scaletest.Churn(cache.WalkBound + 60)
	scaletest.Seed(t, s.Repo, "board", all, map[string]string{"meta.json": string(mj)})

	f := cacheFixture{"over-bound", dir, "board", s}
	root := s.Roots(store.Ref("board"))
	if len(root) != 1 {
		t.Fatalf("fixture: want one root, got %v", root)
	}
	forged := forgeCacheBlob(t, s, "board", root[0])
	if err := s.UpdateRefForce(store.CacheRef("board"), forged); err != nil {
		t.Fatal(err)
	}
	mustOrigin(t, f, cache.OriginRoot)
}

// forgeCacheBlob writes a syntactically valid cache blob for slug whose
// base is `base`, and returns its sha. Used to plant the three untrusted
// states (foreign base, merge in range, over-bound) that no ordinary
// sequence of writes can produce locally.
func forgeCacheBlob(t *testing.T, s store.Store, slug, base string) string {
	t.Helper()
	p, err := s.CacheSource(slug).Fold(base)
	if err != nil {
		t.Fatalf("fold at %s: %v", base, err)
	}
	p.Slug = slug
	raw, err := cache.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := s.WriteObject("blob", raw)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func gitOutput(t *testing.T, s store.Store, args ...string) string {
	t.Helper()
	out, stderr, code := s.Repo.Git("", args...)
	if code != 0 {
		t.Fatalf("git %v: %s", args, stderr)
	}
	return out
}

// TestCacheBlobIsByteDeterministic is criterion 2: the same sha folds to
// the same bytes twice in one process, and again in a SEPARATE store
// directory whose commit shas are identical because the fixture is. No
// clock, no host, no generated_at.
func TestCacheBlobIsByteDeterministic(t *testing.T) {
	build := func(t *testing.T) (store.Store, string) {
		t.Helper()
		dir := initRepo(t)
		res, err := store.Resolve(dir)
		if err != nil {
			t.Fatal(err)
		}
		m := scaletest.Meta()
		m.Slug, m.Scope, m.Created, m.CreatedBy = "board", "cache test", "2026-08-01T00:00:00.000", "t"
		m.FieldOrder = []string{"status"}
		mj, _ := json.Marshal(m)
		scaletest.SeedMerged(t, res.Store.Repo, "board", scaletest.Churn(120),
			scaletest.Branch(300, 30, "alice"), scaletest.Branch(300, 30, "bob"),
			map[string]string{"meta.json": string(mj)})
		head, ok := res.Store.FullHead("board")
		if !ok {
			t.Fatal("no head")
		}
		return res.Store, head
	}
	encode := func(t *testing.T, s store.Store, rev string) string {
		t.Helper()
		p, err := s.CacheSource("board").Fold(rev)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := cache.Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	s1, head1 := build(t)
	first := encode(t, s1, head1)
	second := encode(t, s1, head1)
	if first != second {
		t.Fatal("two folds of the same sha in one process produced different bytes")
	}

	s2, head2 := build(t)
	if head2 != head1 {
		t.Fatalf("fixture is not reproducible across store dirs: %s vs %s - "+
			"the determinism claim cannot be tested this way", head1, head2)
	}
	if third := encode(t, s2, head2); third != first {
		t.Fatalf("a fold in a separate store dir produced different bytes\n first %s\n third %s", first, third)
	}
	if strings.Contains(first, "generated_at") {
		t.Fatal("the blob must carry no clock field")
	}
}

// TestCacheVerifyCatchesAHandCorruptedRef is criterion 5: corrupt the ref
// with git update-ref, and `chit cache verify` must exit non-zero and name
// the difference. It must NOT pass just because the read path already
// degrades safely - the stored blob is a claim, and verify checks the
// claim, not only the answer.
func TestCacheVerifyCatchesAHandCorruptedRef(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	f := cacheFixture{"verify", dir, "board", s}
	mustReset(t, f)

	so, _, code := run(t, dir, "cache", "verify", "board")
	if code != 0 {
		t.Fatalf("a freshly reset cache must verify clean: code=%d %s", code, so)
	}

	// Corrupt it, by hand, with git update-ref: a blob that still decodes as
	// a v1 cache and still claims the head as its base, but whose CONTENT is
	// not what folding that base produces. This is the only corruption worth
	// testing - a blob that merely names an older base is a legitimately
	// stale cache, and verify is right to pass it.
	head, ok := s.FullHead("board")
	if !ok {
		t.Fatal("no head")
	}
	p, err := s.CacheSource("board").Fold(head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(raw), `"state":"open"`, `"state":"closed:hand-edited"`, 1)
	if corrupt == string(raw) {
		t.Fatalf("fixture: nothing was corrupted in %s", raw)
	}
	forged, err := s.WriteObject("blob", []byte(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "update-ref", store.CacheRef("board"), forged)

	so, _, code = run(t, dir, "cache", "verify", "board")
	if code == 0 {
		t.Fatalf("verify must fail on a hand-corrupted ref: %s", so)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(so), &doc); err != nil {
		t.Fatalf("verify payload is not JSON: %v\n%s", err, so)
	}
	diffs, _ := doc["differences"].([]any)
	if len(diffs) == 0 {
		t.Fatalf("verify must NAME the difference, not just exit non-zero: %s", so)
	}
	if !strings.Contains(so, "byte") && !strings.Contains(so, "base") {
		t.Fatalf("verify must say where the difference is: %s", so)
	}

	// And the read is still correct: a corrupt cache degrades to a full
	// fold, never to a wrong or failed answer.
	want := readyJSON(t, f)
	deleteCacheRef(t, f)
	if got := readyJSON(t, f); got != want {
		t.Fatal("a corrupt cache must not change what ready answers")
	}
}

// TestCacheVerifyOnAStoreWithNoCacheRefIsClean: absence is not corruption.
func TestCacheVerifyOnAStoreWithNoCacheRefIsClean(t *testing.T) {
	dir := initRepo(t)
	seedBoard(t, dir, "board")
	res, _ := store.Resolve(dir)
	if err := res.Store.DeleteRef(store.CacheRef("board")); err != nil {
		t.Fatal(err)
	}
	so, se, code := run(t, dir, "cache", "verify", "board")
	if code != 0 {
		t.Fatalf("no cache ref is not a difference: code=%d %s %s", code, so, se)
	}
}

// TestCacheVerifyFailsOnARefPointingAtANonBlob is the second corruption
// shape of criterion 5, and the first one anybody actually tries: point
// the ref at a commit sha with git update-ref. It used to report
// `"cache": null, "differences": []` and exit 0 - the same answer a store
// that has never written a cache gives - so a script could not tell a
// hand-written ref from a clean one. Absence is the ONLY thing that exits
// 0 now; a ref that exists and is not a cache blob is a difference.
func TestCacheVerifyFailsOnARefPointingAtANonBlob(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := cacheFixture{"non-blob", dir, "board", res.Store}
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	mustReset(t, f)

	head, ok := res.Store.FullHead("board")
	if !ok {
		t.Fatal("no head")
	}
	git(t, dir, "update-ref", store.CacheRef("board"), head)

	so, _, code := run(t, dir, "cache", "verify", "board")
	if code == 0 {
		t.Fatalf("a ref pointing at a commit must not verify clean: %s", so)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(so), &doc); err != nil {
		t.Fatalf("verify payload is not JSON: %v\n%s", err, so)
	}
	diffs, _ := doc["differences"].([]any)
	if len(diffs) == 0 {
		t.Fatalf("verify must NAME what is wrong, not only exit non-zero: %s", so)
	}
	if !strings.Contains(so, "cache_unreadable") || !strings.Contains(so, "not a blob") {
		t.Fatalf("verify must say the ref is not a cache blob: %s", so)
	}

	// And the safety property survives this shape too: a forged ref makes
	// the read slower, never different.
	want := readyJSON(t, f)
	deleteCacheRef(t, f)
	if got := readyJSON(t, f); got != want {
		t.Fatal("a ref pointing at a commit must not change what ready answers")
	}
}

// TestCacheVerifyExitStatusThroughTheBuiltBinary runs the three outcomes
// through os/exec rather than in-process ExecuteArgs. The verb's whole
// point is to be runnable by something that is not a human reading JSON,
// and what that caller sees is a process exit status - which is one
// os.Exit plumbing step further out than every other test here measures.
func TestCacheVerifyExitStatusThroughTheBuiltBinary(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := cacheFixture{"exec", dir, "board", res.Store}
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")

	// 1. no ref at all: clean, exit 0.
	deleteCacheRef(t, f)
	if so, se, code := execLedger(t, dir, "cache", "verify", "board"); code != 0 {
		t.Fatalf("no cache ref must exit 0, got %d\n%s%s", code, so, se)
	}

	// 2. a cache this store just built: clean, exit 0.
	if so, se, code := execLedger(t, dir, "cache", "reset", "board"); code != 0 {
		t.Fatalf("cache reset: %d\n%s%s", code, so, se)
	}
	if so, se, code := execLedger(t, dir, "cache", "verify", "board"); code != 0 {
		t.Fatalf("a freshly reset cache must exit 0, got %d\n%s%s", code, so, se)
	}

	// 3. readable, decodable, still claiming the head - and wrong in one
	// field. The shape a `sed` over a dumped blob produces.
	head, ok := res.Store.FullHead("board")
	if !ok {
		t.Fatal("no head")
	}
	p, err := res.Store.CacheSource("board").Fold(head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(raw), `"count":`, `"count": 999999, "count_orig":`, 1)
	if corrupt == string(raw) {
		t.Fatalf("fixture: nothing was corrupted in %s", raw)
	}
	forged, err := res.Store.WriteObject("blob", []byte(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "update-ref", store.CacheRef("board"), forged)

	so, se, code := execLedger(t, dir, "cache", "verify", "board")
	if code != 5 {
		t.Fatalf("a readable-but-wrong blob must exit 5 as a PROCESS, got %d\n%s%s", code, so, se)
	}
	if !strings.Contains(so, "cache_differs") {
		t.Fatalf("verify must name cache_differs: %s", so)
	}

	// 4. and the shape that used to exit 0.
	git(t, dir, "update-ref", store.CacheRef("board"), head)
	if so, se, code := execLedger(t, dir, "cache", "verify", "board"); code != 5 {
		t.Fatalf("a ref pointing at a commit must exit 5 as a PROCESS, got %d\n%s%s", code, so, se)
	}
}

// TestCacheResetRebuildsFromRoot: `chit cache reset` drops the ref and
// mints a fresh one, and the result verifies.
func TestCacheResetRebuildsFromRoot(t *testing.T) {
	dir := initRepo(t)
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	res, _ := store.Resolve(dir)
	if err := res.Store.DeleteRef(store.CacheRef("board")); err != nil {
		t.Fatal(err)
	}
	so, se, code := run(t, dir, "cache", "reset", "board")
	if code != 0 {
		t.Fatalf("cache reset: %d %s %s", code, so, se)
	}
	if _, ok := res.Store.RevParse(store.CacheRef("board")); !ok {
		t.Fatal("cache reset left no ref")
	}
	if _, _, code := run(t, dir, "cache", "verify", "board"); code != 0 {
		t.Fatal("a reset cache must verify clean")
	}
}

// TestPushReplicatesTheCacheRef is criterion 6, and it counts refs on the
// REMOTE rather than trusting an exit code. refs/ledger-cache/* sits
// outside refs/heads/*, exactly like refs/ledger/* does, so a push can
// transfer zero refs and still exit 0 - which is how this would have
// shipped broken.
func TestPushReplicatesTheCacheRef(t *testing.T) {
	root := t.TempDir()
	remoteDir := root + "/remote.git"
	git(t, "", "init", "--bare", "-q", remoteDir)
	a := root + "/a"
	git(t, "", "clone", "-q", remoteDir, a)
	git(t, a, "config", "user.name", "t")
	git(t, a, "config", "user.email", "t@t")
	git(t, a, "commit", "-q", "--allow-empty", "-m", "init")

	seedBoard(t, a, "board")
	mustRun(t, a, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	res, _ := store.Resolve(a)
	s := res.Store
	if _, err := s.CacheSource("board").Reset(); err != nil {
		t.Fatal(err)
	}
	localBlob, ok := s.RevParse(store.CacheRef("board"))
	if !ok {
		t.Fatal("no local cache ref to push")
	}

	if _, se, code := run(t, a, "push", "--remote", "origin"); code != 0 {
		t.Fatalf("push: %d %s", code, se)
	}

	// Count refs on the remote. Not the exit code.
	listed := git(t, remoteDir, "for-each-ref", "--format=%(refname)", "refs/ledger-cache/")
	n := 0
	for _, l := range strings.Split(listed, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	if n < 1 {
		t.Fatalf("push transferred ZERO cache refs and still exited 0 - "+
			"the +refs/ledger-cache/<slug> refspec is missing or lost its '+'; remote refs: %q", listed)
	}
	if got := git(t, remoteDir, "rev-parse", store.CacheRef("board")); got != localBlob {
		t.Fatalf("remote cache ref is %s, local is %s", got, localBlob)
	}
	if typ := git(t, remoteDir, "cat-file", "-t", localBlob); typ != "blob" {
		t.Fatalf("the cache object did not transfer as a blob: %q", typ)
	}

	// The force carve-out: a SECOND push after the cache moved must still
	// replicate, and that is the whole reason the refspec carries a '+'.
	// Without it git rejects the non-fast-forward and replication stops
	// silently while push keeps exiting 0.
	mustRun(t, a, "set", "k-2", "status=open", "--expect", "none", "--ledger", "board", "-m", "two", "--as", "alice")
	if _, err := s.CacheSource("board").Reset(); err != nil {
		t.Fatal(err)
	}
	moved, _ := s.RevParse(store.CacheRef("board"))
	if moved == localBlob {
		t.Fatal("fixture: the cache blob did not move, so this proves nothing")
	}
	if _, se, code := run(t, a, "push", "--remote", "origin"); code != 0 {
		t.Fatalf("second push: %d %s", code, se)
	}
	if got := git(t, remoteDir, "rev-parse", store.CacheRef("board")); got != moved {
		t.Fatalf("a force-updated cache ref did not replicate on the second push: remote %s, local %s", got, moved)
	}
}

// TestPushNeverForcesALedgerRef pins the other half of the carve-out: the
// exception is exactly one namespace wide.
func TestPushNeverForcesALedgerRef(t *testing.T) {
	root := t.TempDir()
	remoteDir := root + "/remote.git"
	git(t, "", "init", "--bare", "-q", remoteDir)
	a := root + "/a"
	git(t, "", "clone", "-q", remoteDir, a)
	git(t, a, "config", "user.name", "t")
	git(t, a, "config", "user.email", "t@t")
	git(t, a, "commit", "-q", "--allow-empty", "-m", "init")
	seedBoard(t, a, "board")

	var calls int64
	counted := store.Store{Repo: gitx.Repo{Dir: a, Calls: &calls}}
	c := &Ctx{Store: counted}
	refspecs := c.cacheAwareRefspecs([]string{"board"})
	for _, r := range refspecs {
		if strings.HasPrefix(r, "+"+store.Ref("")) {
			t.Fatalf("a ledger refspec must never be forced: %q", r)
		}
		if strings.HasPrefix(r, store.CacheRef("")) {
			t.Fatalf("a cache refspec must always be forced: %q", r)
		}
	}
}

// ---------------------------------------------------------------------
// Criterion 7: the timings. Recorded with t.Logf and asserted as ratios,
// against a 16,000-event store built by internal/scaletest - never a
// hand-written fixture.
// ---------------------------------------------------------------------

func TestReadyOnALiveSizedLedgerIsDominatedByTheRefRead(t *testing.T) {
	if testing.Short() {
		t.Skip("scale timing")
	}
	const events = 16000
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	m := scaletest.Meta()
	m.Slug, m.Scope, m.Created, m.CreatedBy = "board", "cache scale", "2026-08-01T00:00:00.000", "t"
	m.FieldOrder = []string{"status"}
	mj, _ := json.Marshal(m)
	scaletest.Seed(t, s.Repo, "board", scaletest.Churn(events), map[string]string{"meta.json": string(mj)})
	s.Repo.Git("", "gc", "--quiet")
	f := cacheFixture{"scale", dir, "board", s}
	mustReset(t, f)

	timeReady := func(label string) float64 {
		best := 0.0
		for i := 0; i < 3; i++ {
			d := timeIt(func() { readyJSON(t, f) })
			if best == 0 || d < best {
				best = d
			}
		}
		t.Logf("chit ready on %d events, %s: %.1f ms", events, label, best)
		return best
	}

	mustOrigin(t, f, cache.OriginCache)
	withCache := timeReady("cache present")
	deleteCacheRef(t, f)
	mustOrigin(t, f, cache.OriginRoot)
	fromRoot := timeReady("cache ref deleted")

	ratio := fromRoot / withCache
	t.Logf("ready speedup from the fold cache: %.1fx (%.1f ms -> %.1f ms)", ratio, fromRoot, withCache)
	if ratio < 5 {
		t.Fatalf("ready off the cache must be at least 5x faster than folding from root, got %.1fx "+
			"(%.1f ms from root, %.1f ms cached)", ratio, fromRoot, withCache)
	}
}

func TestIdleWatchTickNoLongerFoldsTheWholeChain(t *testing.T) {
	if testing.Short() {
		t.Skip("scale timing")
	}
	const events = 16000
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	m := scaletest.Meta()
	m.Slug, m.Scope, m.Created, m.CreatedBy = "board", "cache scale", "2026-08-01T00:00:00.000", "t"
	m.FieldOrder = []string{"status"}
	mj, _ := json.Marshal(m)
	scaletest.Seed(t, s.Repo, "board", scaletest.Churn(events), map[string]string{"meta.json": string(mj)})
	s.Repo.Git("", "gc", "--quiet")

	c := &Ctx{Store: s, Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	tip, err := s.HeadSHA("board")
	if err != nil {
		t.Fatal(err)
	}

	// An IDLE tick: the cursor is already the tip, so the range is empty and
	// the poll delivers nothing. This is the shape a `chit watch` loop runs
	// every 200 ms.
	best := func(f func()) float64 {
		b := 0.0
		for i := 0; i < 3; i++ {
			if d := timeIt(f); b == 0 || d < b {
				b = d
			}
		}
		return b
	}
	now := best(func() {
		if evs, _, err := deliverRange(c, "board", tip, 0, ""); err != nil || len(evs) != 0 {
			t.Fatalf("idle tick delivered %d events: %v", len(evs), err)
		}
	})
	// The pre-dgd-237 shape, reproduced exactly: deliverRange built its
	// lookup table from a WHOLE-CHAIN Store.Events before listing the
	// range. Timing that pair is timing the old code, not an estimate of it.
	before := best(func() {
		if _, _, err := s.Events("board"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RangeNodes(tip, tip); err != nil {
			t.Fatal(err)
		}
	})
	ratio := before / now
	t.Logf("idle watch tick on %d events: %.1f ms before (whole-chain Events + RangeNodes), %.1f ms now (range only) - %.1fx",
		events, before, now, ratio)
	if ratio < 10 {
		t.Fatalf("an idle watch tick must be at least 10x faster than the whole-chain read it replaced, got %.1fx "+
			"(%.1f ms before, %.1f ms now)", ratio, before, now)
	}
}

// timeIt returns f's wall-clock cost in milliseconds. Timings are LOGGED
// as well as asserted: a 5x or 10x claim with no numbers beside it is not a
// measurement.
func timeIt(f func()) float64 {
	start := time.Now()
	f()
	return float64(time.Since(start).Microseconds()) / 1000
}

// TestCacheRefreshIsBatchedNotPerWrite pins the write-path cadence: the
// cache ref must NOT move on every append. It moves once the tail since its
// base reaches cache.CacheEvery commits - which is the whole reason the
// loose-object growth stays at roughly 2 MB/day instead of 136 MB/day (see
// cache.CacheEvery for the arithmetic).
func TestCacheRefreshIsBatchedNotPerWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("cache cadence")
	}
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	m := scaletest.Meta()
	m.Slug, m.Scope, m.Created, m.CreatedBy = "board", "cache test", "2026-08-01T00:00:00.000", "t"
	m.FieldOrder = []string{"status"}
	mj, _ := json.Marshal(m)
	scaletest.Seed(t, s.Repo, "board", scaletest.Churn(120), map[string]string{"meta.json": string(mj)})
	f := cacheFixture{"cadence", dir, "board", s}
	mustReset(t, f)
	start, ok := s.RevParse(store.CacheRef("board"))
	if !ok {
		t.Fatal("no cache ref after reset")
	}

	// One short of the cadence: the ref must still be untouched.
	appendTail(t, f, cache.CacheEvery-1)
	mid, _ := s.RevParse(store.CacheRef("board"))
	if mid != start {
		t.Fatalf("the cache ref moved after only %d writes - the refresh is not batched", cache.CacheEvery-1)
	}
	mustOrigin(t, f, cache.OriginTail)

	// Crossing it refreshes exactly once, and the new blob's base is head.
	appendTail(t, f, 1)
	after, _ := s.RevParse(store.CacheRef("board"))
	if after == start {
		t.Fatalf("the cache ref did not refresh after %d writes past its base", cache.CacheEvery)
	}
	p := mustOrigin(t, f, cache.OriginCache)
	head, _ := s.FullHead("board")
	if p.Base != head {
		t.Fatalf("a refreshed cache must be based on head: base %s, head %s", p.Base, head)
	}
}

// TestCacheIgnoredWhenSchemaVersionDiffers: a version bump IS the cache
// flush. A blob carrying any other v is ignored exactly like an absent
// one, so a schema change never needs a migration and a newer peer's blob
// never confuses an older reader.
func TestCacheIgnoredWhenSchemaVersionDiffers(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	f := cacheFixture{"version", dir, "board", s}
	mustReset(t, f)
	want := readyJSON(t, f)

	head, _ := s.FullHead("board")
	p, err := s.CacheSource("board").Fold(head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(raw), `{"v":1,`, `{"v":99,`, 1)
	if bumped == string(raw) {
		t.Fatalf("fixture: the version field is not where this test expects it: %s", raw[:40])
	}
	sha, err := s.WriteObject("blob", []byte(bumped))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefForce(store.CacheRef("board"), sha); err != nil {
		t.Fatal(err)
	}
	mustOrigin(t, f, cache.OriginRoot)
	if got := readyJSON(t, f); got != want {
		t.Fatal("a future-version cache must be ignored, not misread")
	}
}
