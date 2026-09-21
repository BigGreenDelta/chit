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
