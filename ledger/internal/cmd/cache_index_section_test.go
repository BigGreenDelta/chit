package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"ledger/internal/cache"
	"ledger/internal/store"
)

// ---------------------------------------------------------------------
// dgd-431: the index blob is a header line plus one line per key, so status
// reads need only the header and a per-key read needs one section.
// ---------------------------------------------------------------------

// sectionFixture is a board with notes on two keys and committers from two
// authors, its two cache refs built.
func sectionFixture(t *testing.T) cacheFixture {
	t.Helper()
	dir := initRepo(t)
	res, err := store.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := cacheFixture{"sections", dir, "board", res.Store}
	seedBoard(t, dir, "board")
	mustRun(t, dir, "set", "k-1", "status=open", "--expect", "none", "--ledger", "board", "-m", "one", "--as", "alice")
	mustRun(t, dir, "set", "k-2", "status=open", "--expect", "none", "--ledger", "board", "-m", "two", "--as", "alice")
	mustRun(t, dir, "note", "--kind", "handoff", "--key", "k-1", "--ledger", "board", "-m", "note one", "--as", "alice")
	mustRun(t, dir, "note", "--kind", "gotcha", "--key", "k-2", "--ledger", "board", "-m", "note two", "--as", "bob")
	mustRun(t, dir, "note", "--kind", "gotcha", "--ledger", "board", "-m", "ledger-wide", "--as", "bob")
	return f
}

// corruptSections overwrites the index blob's event sections with garbage of
// the same length, leaving the header line byte for byte as it was.
func corruptSections(t *testing.T, f cacheFixture) {
	t.Helper()
	raw, _, err := f.s.CacheIndexSource(f.slug).LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 || nl == len(raw)-1 {
		t.Fatalf("fixture: index blob has no sections to corrupt")
	}
	bad := append(append([]byte{}, raw[:nl+1]...), bytes.Repeat([]byte("x"), len(raw)-nl-1)...)
	sha, err := f.s.WriteObject("blob", bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.UpdateRefForce(store.CacheIndexRef(f.slug), sha); err != nil {
		t.Fatal(err)
	}
}

// TestStatusSpineReadSkipsEvents: with every event section corrupt, the
// index still reads as a cache hit and the status forms that need only the
// header answer byte-identically to a fold from root.
func TestStatusSpineReadSkipsEvents(t *testing.T) {
	f := sectionFixture(t)
	deleteBothRefs(t, f)
	wantAll := statusJSON(t, f)
	wantField := statusJSON(t, f, "--field", "status")

	resetBothRefs(t, f)
	corruptSections(t, f)
	ix, ok := f.s.CacheIndexSource(f.slug).TryRead()
	if !ok {
		t.Fatal("a blob with a good header and corrupt sections must still be a cache hit")
	}
	if _, err := ix.AllEvents(); !errors.Is(err, cache.ErrIndexSection) {
		t.Fatalf("fixture: sections are not corrupt: AllEvents err = %v", err)
	}
	if got := statusJSON(t, f); got != wantAll {
		t.Fatalf("status off a header-only read differs\n got %s\nwant %s", got, wantAll)
	}
	if got := statusJSON(t, f, "--field", "status"); got != wantField {
		t.Fatalf("status --field off a header-only read differs\n got %s\nwant %s", got, wantField)
	}
}

// TestReadVerbsCorruptSectionFallsBackToFold: a verb that needs a section
// that does not decode answers byte-identically to a fold from root and
// never errors.
func TestReadVerbsCorruptSectionFallsBackToFold(t *testing.T) {
	f := sectionFixture(t)
	cases := []struct {
		verb string
		args []string
	}{
		{"status", []string{"k-1"}}, {"status", []string{"k-2", "--field", "status"}},
		{"notes", []string{"--key", "k-1"}}, {"notes", nil}, {"notes", []string{"--kind", "gotcha"}},
		{"show", nil},
	}
	run1 := func(verb string, args []string) string {
		so, se, code := run(t, f.dir, append(append([]string{verb}, args...), "--ledger", f.slug)...)
		if code != 0 {
			t.Fatalf("%s %v: code=%d %s", verb, args, code, se)
		}
		return so
	}
	deleteBothRefs(t, f)
	want := make([]string, len(cases))
	for i, c := range cases {
		want[i] = run1(c.verb, c.args)
	}
	resetBothRefs(t, f)
	for i, c := range cases {
		corruptSections(t, f)
		if got := run1(c.verb, c.args); got != want[i] {
			t.Fatalf("%s %v with a corrupt section differs from a fold from root\n got %s\nwant %s", c.verb, c.args, got, want[i])
		}
	}
}

// TestIndexPerKeyReadMatchesAllEvents: EventsForKey is the slice of
// AllEvents for that key, for a read with and without a tail.
func TestIndexPerKeyReadMatchesAllEvents(t *testing.T) {
	f := sectionFixture(t)
	resetBothRefs(t, f)
	check := func(label string) {
		ix, ok := f.s.CacheIndexSource(f.slug).TryRead()
		if !ok {
			t.Fatalf("%s: index read missed", label)
		}
		all, err := ix.AllEvents()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != ix.Count {
			t.Fatalf("%s: %d records for a count of %d", label, len(all), ix.Count)
		}
		for i, r := range all {
			if r.Ord != i {
				t.Fatalf("%s: record %d has ordinal %d", label, i, r.Ord)
			}
		}
		for _, key := range []string{"", "k-1", "k-2", "no-such-key"} {
			var want []cache.EventRec
			for _, r := range all {
				if r.Key == key {
					want = append(want, r)
				}
			}
			got, err := ix.EventsForKey(key)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%s: key %q: %d records, want %d", label, key, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s: key %q record %d: got %+v want %+v", label, key, i, got[i], want[i])
				}
			}
		}
	}
	check("exact hit")
	mustRun(t, f.dir, "note", "--kind", "handoff", "--key", "k-1", "--ledger", "board", "-m", "tail note", "--as", "alice")
	check("with tail")
}

// TestIndexRewrittenOnFirstWriteAfterVersionBump is approval condition 1: a
// blob the new code cannot read (a v2 blob, one JSON line) is rewritten by
// the FIRST write after deploy, not after CacheEvery commits, so the runner
// never replays the chain from root on every read in between.
func TestIndexRewrittenOnFirstWriteAfterVersionBump(t *testing.T) {
	f := sectionFixture(t)
	resetBothRefs(t, f)
	v2 := []byte(`{"v":2,"slug":"board","base":"` + strings.Repeat("0", 40) + `","schema":{},"spine":{},"events":[],"committers":[]}`)
	sha, err := f.s.WriteObject("blob", v2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.UpdateRefForce(store.CacheIndexRef(f.slug), sha); err != nil {
		t.Fatal(err)
	}
	mustIndexOrigin(t, f, cache.OriginRoot)

	mustRun(t, f.dir, "note", "--kind", "handoff", "--key", "k-1", "--ledger", "board", "-m", "first write", "--as", "alice")

	mustIndexOrigin(t, f, cache.OriginCache)
	raw, _, err := f.s.CacheIndexSource(f.slug).LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(`{"v":3,`)) {
		t.Fatalf("index was not rewritten as v3 by the first write: %.60s", raw)
	}
}
