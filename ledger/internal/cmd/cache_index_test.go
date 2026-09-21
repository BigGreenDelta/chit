package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"ledger/internal/cache"
	"ledger/internal/scaletest"
	"ledger/internal/store"
)

// ---------------------------------------------------------------------
// dgd-265's index cache: refs/ledger-cache-index/<slug>, alongside dgd-237's
// refs/ledger-cache/<slug>. Reuses cacheFixtures (cache_test.go) so every
// test here runs over the same five DAG shapes the projection cache proved
// itself against.
// ---------------------------------------------------------------------

func deleteIndexRef(t *testing.T, f cacheFixture) {
	t.Helper()
	if err := f.s.DeleteRef(store.CacheIndexRef(f.slug)); err != nil {
		t.Fatal(err)
	}
}

func mustResetIndex(t *testing.T, f cacheFixture) {
	t.Helper()
	if _, err := f.s.CacheIndexSource(f.slug).Reset(); err != nil {
		t.Fatalf("%s: index reset: %v", f.name, err)
	}
}

func mustIndexOrigin(t *testing.T, f cacheFixture, want cache.Origin) cache.Index {
	t.Helper()
	ix, err := f.s.Index(f.slug)
	if err != nil {
		t.Fatalf("%s: index: %v", f.name, err)
	}
	if ix.Origin != want {
		t.Fatalf("%s: index read came from %q, want %q", f.name, ix.Origin, want)
	}
	return ix
}

// lsJSON runs `ls --json`-shaped output (ls has no --at: nothing it renders
// in JSON depends on an evaluation clock finer than "idle", which the
// existing ls tests already accept comparing across two in-process calls).
func lsJSON(t *testing.T, dir string) string {
	t.Helper()
	so, se, code := run(t, dir, "ls")
	if code != 0 {
		t.Fatalf("ls: code=%d %s", code, se)
	}
	return so
}

// TestLsOffCacheIsByteIdenticalToFoldFromRoot is criterion 2 for `ls`:
// dgd-265's claim that `ls` needs nothing beyond dgd-237's own projection
// blob, proved the same way ready's cache was - byte-identical output with
// the ref present versus deleted, across every DAG shape.
func TestLsOffCacheIsByteIdenticalToFoldFromRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("cache differential")
	}
	for _, f := range cacheFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			deleteCacheRef(t, f)
			want := lsJSON(t, f.dir)

			mustReset(t, f)
			mustOrigin(t, f, cache.OriginCache)
			if got := lsJSON(t, f.dir); got != want {
				t.Fatalf("%s: ls off cache differs from a fold from root\n got %s\nwant %s", f.name, got, want)
			}

			appendTail(t, f, 5)
			mustOrigin(t, f, cache.OriginTail)
			withTail := lsJSON(t, f.dir)
			deleteCacheRef(t, f)
			if fromRoot := lsJSON(t, f.dir); withTail != fromRoot {
				t.Fatalf("%s: ls off a tail-folded cache differs from a fold from root\n got %s\nwant %s",
					f.name, withTail, fromRoot)
			}
		})
	}
}

// TestIndexBlobIsByteDeterministic is criterion 3 for the index: the same
// sha folds to the same bytes twice in one process, and again in a separate
// store directory whose commit shas are identical because the fixture is -
// the same claim TestCacheBlobIsByteDeterministic makes for the v1 blob.
func TestIndexBlobIsByteDeterministic(t *testing.T) {
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
		mj, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
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
		ix, err := s.CacheIndexSource("board").FoldIndex(rev)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := cache.EncodeIndex(ix)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	s1, head1 := build(t)
	first := encode(t, s1, head1)
	second := encode(t, s1, head1)
	if first != second {
		t.Fatal("two index folds of the same sha in one process produced different bytes")
	}

	s2, head2 := build(t)
	if head2 != head1 {
		t.Fatalf("fixture is not reproducible across store dirs: %s vs %s - "+
			"the determinism claim cannot be tested this way", head1, head2)
	}
	if third := encode(t, s2, head2); third != first {
		t.Fatalf("an index fold in a separate store dir produced different bytes\n first %s\n third %s", first, third)
	}
}

// TestIndexIgnoredWhenBaseIsForeign is the index's counterpart to
// TestCacheIgnoredWhenBaseIsForeign - a forged index blob whose base names
// an unrelated root must be ignored, never trusted, exactly like the
// projection's.
func TestIndexIgnoredWhenBaseIsForeign(t *testing.T) {
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
	deleteIndexRef(t, f)

	otherRoots := s.Roots(store.Ref("other"))
	if len(otherRoots) != 1 {
		t.Fatalf("fixture: want one root on 'other', got %v", otherRoots)
	}
	ix, err := s.CacheIndexSource("board").FoldIndex(otherRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	ix.Slug = "board"
	raw, err := cache.EncodeIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := s.WriteObject("blob", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefForce(store.CacheIndexRef("board"), sha); err != nil {
		t.Fatal(err)
	}
	mustIndexOrigin(t, f, cache.OriginRoot)
}

// TestIndexResetAndVerifyRoundTrip: `cache reset`/`cache verify` cover the
// index ref, not only the projection's.
func TestIndexResetAndVerifyRoundTrip(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")

	so, se, code := run(t, dir, "cache", "reset", "board")
	if code != 0 {
		t.Fatalf("cache reset: %d %s %s", code, so, se)
	}
	if _, ok := s.RevParse(store.CacheIndexRef("board")); !ok {
		t.Fatal("cache reset left no index ref")
	}
	so, se, code = run(t, dir, "cache", "verify", "board")
	if code != 0 {
		t.Fatalf("a freshly reset index must verify clean: %d %s %s", code, so, se)
	}

	// Corrupt only the INDEX ref, leaving the projection clean - verify must
	// still catch it. This is the shape that would otherwise slip through if
	// verify only ever checked the v1 blob.
	head, _ := s.FullHead("board")
	ix, err := s.CacheIndexSource("board").FoldIndex(head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.EncodeIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(raw), `"schema":`, `"schema_x":`, 1)
	if corrupt == string(raw) {
		t.Fatalf("fixture: nothing was corrupted in %s", raw)
	}
	forged, err := s.WriteObject("blob", []byte(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefForce(store.CacheIndexRef("board"), forged); err != nil {
		t.Fatal(err)
	}

	so, _, code = run(t, dir, "cache", "verify", "board")
	if code == 0 {
		t.Fatalf("a corrupted index ref must fail verify even with a clean projection: %s", so)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(so), &doc); err != nil {
		t.Fatalf("verify payload is not JSON: %v\n%s", err, so)
	}
	diffs, _ := doc["differences"].([]any)
	if len(diffs) == 0 {
		t.Fatalf("verify must name the index difference: %s", so)
	}
}

// TestCacheVerifyFailsOnAnIndexRefPointingAtANonBlob is the index's
// counterpart to TestCacheVerifyFailsOnARefPointingAtANonBlob: a ref
// pointing at a commit sha, not a blob at all, is a difference, never a
// clean "no cache" answer.
func TestCacheVerifyFailsOnAnIndexRefPointingAtANonBlob(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	f := cacheFixture{"index-non-blob", dir, "board", res.Store}
	resetBothRefs(t, f)

	head, ok := res.Store.FullHead("board")
	if !ok {
		t.Fatal("no head")
	}
	if err := res.Store.UpdateRefForce(store.CacheIndexRef("board"), head); err != nil {
		t.Fatal(err)
	}

	so, _, code := run(t, dir, "cache", "verify", "board")
	if code == 0 {
		t.Fatalf("an index ref pointing at a commit must not verify clean: %s", so)
	}
	if !strings.Contains(so, "cache_unreadable") || !strings.Contains(so, "not a blob") {
		t.Fatalf("verify must say the index ref is not a cache blob: %s", so)
	}
}

// ---------------------------------------------------------------------
// Criterion 7's degradation matrix, at the index level: mirrors
// TestCacheIgnoredWhenBaseIsAheadOfHead / RangeHoldsAMerge / TailExceedsBound
// / SchemaVersionDiffers (cache_test.go) for the index ref. Foreign base and
// plain absence are already covered above
// (TestIndexIgnoredWhenBaseIsForeign, TestIndexResetAndVerifyRoundTrip's
// reset-from-absent path).
// ---------------------------------------------------------------------

// TestIndexIgnoredWhenBaseIsAheadOfHead: a peer's index cache arriving
// before the commits it describes must be ignored, not walked toward - the
// walk runs from head BACKWARD, so a base ahead of head is simply never
// reached.
func TestIndexIgnoredWhenBaseIsAheadOfHead(t *testing.T) {
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

	f := cacheFixture{"index-ahead", dir, "board", s}
	mustResetIndex(t, f) // index base is now the newer head
	if err := s.UpdateRefForce(store.Ref("board"), behind); err != nil {
		t.Fatal(err)
	}
	mustIndexOrigin(t, f, cache.OriginRoot)
}

// TestIndexIgnoredWhenRangeHoldsAMerge is the index's counterpart to the
// same-named projection test: a merge inside base..head attaches to the
// cached prefix's interior, and the walk refuses it exactly the same way
// regardless of which blob schema is behind it.
func TestIndexIgnoredWhenRangeHoldsAMerge(t *testing.T) {
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

	f := cacheFixture{"index-merge-in-range", dir, "board", s}
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
	ix, err := s.CacheIndexSource("board").FoldIndex(baseTip)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.EncodeIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := s.WriteObject("blob", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefForce(store.CacheIndexRef("board"), sha); err != nil {
		t.Fatal(err)
	}
	mustIndexOrigin(t, f, cache.OriginRoot)
}

// TestIndexIgnoredWhenTailExceedsTheBound: past WalkBound the index cache
// is ignored, so a stale index can never turn a cheap read into a long one.
func TestIndexIgnoredWhenTailExceedsTheBound(t *testing.T) {
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

	f := cacheFixture{"index-over-bound", dir, "board", s}
	root := s.Roots(store.Ref("board"))
	if len(root) != 1 {
		t.Fatalf("fixture: want one root, got %v", root)
	}
	ix, err := s.CacheIndexSource("board").FoldIndex(root[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.EncodeIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := s.WriteObject("blob", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefForce(store.CacheIndexRef("board"), sha); err != nil {
		t.Fatal(err)
	}
	mustIndexOrigin(t, f, cache.OriginRoot)
}

// TestIndexIgnoredWhenSchemaVersionDiffers: a future index version is
// ignored exactly like an absent ref, never misread.
func TestIndexIgnoredWhenSchemaVersionDiffers(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Store
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	f := cacheFixture{"index-version", dir, "board", s}
	mustResetIndex(t, f)

	head, _ := s.FullHead("board")
	ix, err := s.CacheIndexSource("board").FoldIndex(head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.EncodeIndex(ix)
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
	if err := s.UpdateRefForce(store.CacheIndexRef("board"), sha); err != nil {
		t.Fatal(err)
	}
	mustIndexOrigin(t, f, cache.OriginRoot)
}
