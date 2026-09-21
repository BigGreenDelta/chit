package cmd

import (
	"encoding/json"
	"testing"

	"ledger/internal/cache"
	"ledger/internal/store"
)

// ---------------------------------------------------------------------
// dgd-265 criterion 2 for status/show/show --id/notes: cached output must
// be byte-identical to the same command with BOTH cache refs deleted.
// Reuses cacheFixtures (cache_test.go) - the same five DAG shapes dgd-237's
// own differential test runs against - plus a couple of notes/kinds seeded
// on each so the notes/show-recent-notes/status-drilldown paths have real
// content to differ on.
// ---------------------------------------------------------------------

// enrichFixture adds a note (so notes/show have a body-bearing event to
// render) and a second status write on a fresh key (so --where and the
// per-key drill-down have something beyond Churn's own seeded keys) to an
// already-seeded fixture.
func enrichFixture(t *testing.T, f cacheFixture) {
	t.Helper()
	mustRun(t, f.dir, "set", "cache-test-key", "status=open", "--expect", "none",
		"--ledger", f.slug, "-m", "seeded for dgd-265", "--as", "alice")
	mustRun(t, f.dir, "note", "--kind", "handoff", "--key", "cache-test-key",
		"--ledger", f.slug, "-m", "a handoff note", "--as", "alice")
	mustRun(t, f.dir, "note", "--kind", "gotcha",
		"--ledger", f.slug, "-m", "a ledger-wide gotcha", "--as", "bob")
}

func deleteBothRefs(t *testing.T, f cacheFixture) {
	t.Helper()
	deleteCacheRef(t, f)
	deleteIndexRef(t, f)
}

func resetBothRefs(t *testing.T, f cacheFixture) {
	t.Helper()
	mustReset(t, f)
	mustResetIndex(t, f)
}

func statusJSON(t *testing.T, f cacheFixture, args ...string) string {
	t.Helper()
	full := append([]string{"status"}, args...)
	full = append(full, "--ledger", f.slug)
	so, se, code := run(t, f.dir, full...)
	if code != 0 {
		t.Fatalf("%s: status %v: code=%d %s", f.name, args, code, se)
	}
	return so
}

func showJSON(t *testing.T, f cacheFixture, args ...string) string {
	t.Helper()
	full := append([]string{"show"}, args...)
	full = append(full, "--ledger", f.slug)
	so, se, code := run(t, f.dir, full...)
	if code != 0 {
		t.Fatalf("%s: show %v: code=%d %s", f.name, args, code, se)
	}
	return so
}

func notesJSON(t *testing.T, f cacheFixture, args ...string) string {
	t.Helper()
	full := append([]string{"notes"}, args...)
	full = append(full, "--ledger", f.slug)
	so, se, code := run(t, f.dir, full...)
	if code != 0 {
		t.Fatalf("%s: notes %v: code=%d %s", f.name, args, code, se)
	}
	return so
}

// firstNoteID pulls one note id off a fresh, uncached notes read - used to
// exercise show --id / notes --id against a real event without assuming
// which fixture produced which id.
func firstNoteID(t *testing.T, f cacheFixture) string {
	t.Helper()
	so := notesJSON(t, f, "--limit", "1")
	var doc struct {
		Notes []struct {
			ID string `json:"id"`
		} `json:"notes"`
	}
	if err := json.Unmarshal([]byte(so), &doc); err != nil {
		t.Fatalf("%s: notes payload not JSON: %v\n%s", f.name, err, so)
	}
	if len(doc.Notes) == 0 {
		t.Fatalf("%s: fixture has no notes to pick an id from", f.name)
	}
	return doc.Notes[0].ID
}

// TestStatusOffCacheIsByteIdenticalToFoldFromRoot is criterion 2 for
// `status`, both the global spine and the per-key drill-down, across every
// DAG shape and both cache-read branches (exact-hit, tail fold).
func TestStatusOffCacheIsByteIdenticalToFoldFromRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("cache differential")
	}
	for _, f := range cacheFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			enrichFixture(t, f)
			cases := [][]string{{}, {"--field", "status"}, {"cache-test-key"}}

			deleteBothRefs(t, f)
			want := make([]string, len(cases))
			for i, args := range cases {
				want[i] = statusJSON(t, f, args...)
			}

			resetBothRefs(t, f)
			mustOrigin(t, f, cache.OriginCache)
			mustIndexOrigin(t, f, cache.OriginCache)
			for i, args := range cases {
				if got := statusJSON(t, f, args...); got != want[i] {
					t.Fatalf("%s: status %v off cache (exact hit) differs\n got %s\nwant %s", f.name, args, got, want[i])
				}
			}

			appendTail(t, f, 5)
			mustOrigin(t, f, cache.OriginTail)
			mustIndexOrigin(t, f, cache.OriginTail)
			for _, args := range cases {
				withTail := statusJSON(t, f, args...)
				deleteBothRefs(t, f)
				fromRoot := statusJSON(t, f, args...)
				if withTail != fromRoot {
					t.Fatalf("%s: status %v off a tail-folded cache differs\n got %s\nwant %s", f.name, args, withTail, fromRoot)
				}
				resetBothRefs(t, f)
				appendTail(t, f, 5)
			}
		})
	}
}

// TestShowOffCacheIsByteIdenticalToFoldFromRoot is criterion 2 for `show`,
// bare and with --where.
func TestShowOffCacheIsByteIdenticalToFoldFromRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("cache differential")
	}
	for _, f := range cacheFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			enrichFixture(t, f)
			cases := [][]string{{}, {"--where", "status=open"}}

			deleteBothRefs(t, f)
			want := make([]string, len(cases))
			for i, args := range cases {
				want[i] = showJSON(t, f, args...)
			}

			resetBothRefs(t, f)
			mustOrigin(t, f, cache.OriginCache)
			mustIndexOrigin(t, f, cache.OriginCache)
			for i, args := range cases {
				if got := showJSON(t, f, args...); got != want[i] {
					t.Fatalf("%s: show %v off cache (exact hit) differs\n got %s\nwant %s", f.name, args, got, want[i])
				}
			}

			appendTail(t, f, 5)
			mustOrigin(t, f, cache.OriginTail)
			mustIndexOrigin(t, f, cache.OriginTail)
			for _, args := range cases {
				withTail := showJSON(t, f, args...)
				deleteBothRefs(t, f)
				fromRoot := showJSON(t, f, args...)
				if withTail != fromRoot {
					t.Fatalf("%s: show %v off a tail-folded cache differs\n got %s\nwant %s", f.name, args, withTail, fromRoot)
				}
				resetBothRefs(t, f)
				appendTail(t, f, 5)
			}
		})
	}
}

// TestShowIDOffCacheIsByteIdenticalToFoldFromRoot is criterion 2 for
// `show --id`, the entry point findByIDInIndex replaces findByID's full
// scan for.
func TestShowIDOffCacheIsByteIdenticalToFoldFromRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("cache differential")
	}
	for _, f := range cacheFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			enrichFixture(t, f)
			id := firstNoteID(t, f)

			deleteBothRefs(t, f)
			want := showJSON(t, f, "--id", id)

			resetBothRefs(t, f)
			mustOrigin(t, f, cache.OriginCache)
			mustIndexOrigin(t, f, cache.OriginCache)
			if got := showJSON(t, f, "--id", id); got != want {
				t.Fatalf("%s: show --id off cache (exact hit) differs\n got %s\nwant %s", f.name, got, want)
			}

			appendTail(t, f, 5)
			mustOrigin(t, f, cache.OriginTail)
			mustIndexOrigin(t, f, cache.OriginTail)
			withTail := showJSON(t, f, "--id", id)
			deleteBothRefs(t, f)
			fromRoot := showJSON(t, f, "--id", id)
			if withTail != fromRoot {
				t.Fatalf("%s: show --id off a tail-folded cache differs\n got %s\nwant %s", f.name, withTail, fromRoot)
			}
		})
	}
}

// TestNotesOffCacheIsByteIdenticalToFoldFromRoot is criterion 2 for
// `notes`, across the selection dimensions the design gate's correction was
// about (--kind sweeping the whole ledger, no --key) plus --key, --id,
// --limit and --latest.
func TestNotesOffCacheIsByteIdenticalToFoldFromRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("cache differential")
	}
	for _, f := range cacheFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			enrichFixture(t, f)
			id := firstNoteID(t, f)
			cases := [][]string{
				{}, {"--kind", "handoff"}, {"--key", "cache-test-key"},
				{"--limit", "1"}, {"--latest"}, {"--id", id},
			}

			deleteBothRefs(t, f)
			want := make([]string, len(cases))
			for i, args := range cases {
				want[i] = notesJSON(t, f, args...)
			}

			resetBothRefs(t, f)
			mustOrigin(t, f, cache.OriginCache)
			mustIndexOrigin(t, f, cache.OriginCache)
			for i, args := range cases {
				if got := notesJSON(t, f, args...); got != want[i] {
					t.Fatalf("%s: notes %v off cache (exact hit) differs\n got %s\nwant %s", f.name, args, got, want[i])
				}
			}

			appendTail(t, f, 5)
			mustOrigin(t, f, cache.OriginTail)
			mustIndexOrigin(t, f, cache.OriginTail)
			for _, args := range cases {
				withTail := notesJSON(t, f, args...)
				deleteBothRefs(t, f)
				fromRoot := notesJSON(t, f, args...)
				if withTail != fromRoot {
					t.Fatalf("%s: notes %v off a tail-folded cache differs\n got %s\nwant %s", f.name, args, withTail, fromRoot)
				}
				resetBothRefs(t, f)
				appendTail(t, f, 5)
			}
		})
	}
}

// TestReadVerbsFallBackWhenEitherRefAloneIsMissing is criterion 7's
// two-refs-can-disagree case: status/show/notes must fall back to a full
// fold - never mix a cache answer from one head with an index answer from
// another - when only ONE of the two refs is present, in both directions.
func TestReadVerbsFallBackWhenEitherRefAloneIsMissing(t *testing.T) {
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := cacheFixture{"asymmetric", dir, "board", res.Store}
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	mustRun(t, dir, "note", "--kind", "handoff", "--ledger", "board", "-m", "a note", "--as", "alice")

	deleteBothRefs(t, f)
	wantStatus := statusJSON(t, f)
	wantShow := showJSON(t, f)
	wantNotes := notesJSON(t, f)

	// Only the projection ref present.
	mustReset(t, f)
	deleteIndexRef(t, f)
	if got := statusJSON(t, f); got != wantStatus {
		t.Fatalf("status differs with only the projection ref present\n got %s\nwant %s", got, wantStatus)
	}
	if got := showJSON(t, f); got != wantShow {
		t.Fatalf("show differs with only the projection ref present\n got %s\nwant %s", got, wantShow)
	}
	if got := notesJSON(t, f); got != wantNotes {
		t.Fatalf("notes differs with only the projection ref present\n got %s\nwant %s", got, wantNotes)
	}

	// Only the index ref present.
	deleteCacheRef(t, f)
	mustResetIndex(t, f)
	if got := statusJSON(t, f); got != wantStatus {
		t.Fatalf("status differs with only the index ref present\n got %s\nwant %s", got, wantStatus)
	}
	if got := showJSON(t, f); got != wantShow {
		t.Fatalf("show differs with only the index ref present\n got %s\nwant %s", got, wantShow)
	}
	if got := notesJSON(t, f); got != wantNotes {
		t.Fatalf("notes differs with only the index ref present\n got %s\nwant %s", got, wantNotes)
	}
}
