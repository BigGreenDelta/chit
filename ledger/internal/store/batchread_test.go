package store

// The claim the persistent reader makes is not "it returns something
// reasonable". It is that it returns EXACTLY what `git log` plus a fresh
// `cat-file --batch` returned: same events, same order, same meta, same roots.
// So every test here is differential against that path, kept alive below as
// eventsDAGViaSpawn.
//
// Fixture shapes are chosen from the failure history, not for coverage:
//
//   packed         a chain built by writing loose objects and never running gc
//                  contains no packfiles at all, which is exactly how the
//                  packfile problem hid the first time.
//   merges         every earlier benchmark chain was linear and the prototype
//                  followed first parents only. chit's chains carry sync merge
//                  commits and the fold contracts the DAG.
//   sentinels      a commit whose event.json is missing or unparseable is
//                  contracted out rather than crashing a read.
//   linked worktree `.git` is a FILE there; getting the layout wrong was silent
//                  data loss last time (TestByBranchFold).

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ledger/internal/dag"
	"ledger/internal/gitx"
	"ledger/internal/model"
)

// eventsDAGViaSpawn is the pre-Phase-1 read, preserved verbatim as the oracle:
// one `git log` for topology, one fresh `cat-file --batch` for contents.
func (s Store) eventsDAGViaSpawn(refName, label string) ([]model.Event, model.Meta, dag.Result, error) {
	var meta model.Meta
	out, _, code := s.Repo.Git("", "log", "--format=%H%x09%P", refName)
	if code != 0 || out == "" {
		return nil, meta, dag.Result{}, fmt.Errorf("%w: %s", ErrUnknownLedger, label)
	}
	lines := strings.Split(out, "\n")
	commits := make([]string, len(lines))
	parents := make([][]string, len(lines))
	for i, line := range lines {
		c, p, _ := strings.Cut(line, "\t")
		commits[i] = c
		if p = strings.TrimSpace(p); p != "" {
			parents[i] = strings.Fields(p)
		}
	}
	reqs := make([]string, 0, len(commits)*2)
	for _, c := range commits {
		reqs = append(reqs, c+":event.json", c+":meta.json")
	}
	contents, present := s.catBatchViaSpawn(reqs)

	nodes := make([]dag.Node, len(commits))
	byEvent := make(map[string]model.Event, len(commits))
	for i, c := range commits {
		evIdx, metaIdx := 2*i, 2*i+1
		if present[metaIdx] {
			json.Unmarshal([]byte(contents[metaIdx]), &meta)
		}
		node := dag.Node{SHA: c, Parents: parents[i]}
		var ev model.Event
		if !present[evIdx] {
			node.IsSentinel = true
		} else if err := json.Unmarshal([]byte(contents[evIdx]), &ev); err != nil {
			node.IsSentinel = true
		} else {
			ev.ID = c[:10]
			node.TS = ev.TS
			node.IsSentinel = ev.Type == "sync"
			byEvent[c] = ev
		}
		nodes[i] = node
	}
	result := dag.Sort(nodes)
	evs := make([]model.Event, 0, len(result.Order))
	for _, sha := range result.Order {
		evs = append(evs, byEvent[sha])
	}
	return evs, meta, result, nil
}

func (s Store) catBatchViaSpawn(ids []string) (contents []string, present []bool) {
	contents = make([]string, len(ids))
	present = make([]bool, len(ids))
	if len(ids) == 0 {
		return
	}
	out, _, _ := s.Repo.GitRaw(strings.Join(ids, "\n"), "cat-file", "--batch")
	rest := out
	for i := range ids {
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		hdr := strings.Fields(rest[:nl])
		rest = rest[nl+1:]
		if len(hdr) >= 2 && hdr[len(hdr)-1] == "missing" {
			continue
		}
		if len(hdr) < 3 {
			continue
		}
		size := 0
		fmt.Sscanf(hdr[2], "%d", &size)
		if size > len(rest) {
			size = len(rest)
		}
		contents[i] = rest[:size]
		present[i] = true
		rest = strings.TrimPrefix(rest[size:], "\n")
	}
	return contents, present
}

// --- fixtures -------------------------------------------------------------

func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// buildFixture writes a chain directly through gitx's in-process writer, then
// points refs/ledger/t at the tip. shape selects the DAG.
func buildFixture(t *testing.T, shape string, n int) (Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "s.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	s := Store{Repo: gitx.Repo{Dir: dir}}
	// The child holds handles on the store; Windows refuses to remove a
	// directory while it does. Cleanups run LIFO and t.TempDir registered
	// its removal first, so this one runs before it.
	t.Cleanup(gitx.CloseBatchReaders)

	commit := func(parents []string, ev model.Event, extra map[string]string) string {
		sha, err := s.buildCommit(parents, ev, extra)
		if err != nil {
			t.Fatalf("buildCommit: %v", err)
		}
		return sha
	}

	meta := map[string]string{"meta.json": `{"guard":["status"],"field_order":["status"]}`}
	var tip string
	var all []string
	for i := 0; i < n; i++ {
		ev := model.Event{TS: fmt.Sprintf("2026-09-06T12:00:%02d.000", i%60),
			Type: "set", Author: "triage", Key: fmt.Sprintf("d-t-%d-01", i),
			Fields: map[string]string{"status": "open"}}
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		var extra map[string]string
		if i == 0 {
			extra = meta
		}
		tip = commit(parents, ev, extra)
		all = append(all, tip)
	}

	switch shape {
	case "merges":
		// A sync sentinel joining two chains, which is the shape the fold
		// contracts out and the shape no earlier benchmark exercised.
		side := commit([]string{all[0]}, model.Event{TS: "2026-09-06T12:30:00.000",
			Type: "set", Author: "other", Key: "d-t-side-01",
			Fields: map[string]string{"status": "open"}}, nil)
		tip = commit([]string{tip, side}, model.Event{TS: "2026-09-06T12:31:00.000",
			Type: "sync", Author: "sync"}, nil)
		side2 := commit([]string{side}, model.Event{TS: "2026-09-06T12:32:00.000",
			Type: "set", Author: "other", Key: "d-t-side-02",
			Fields: map[string]string{"status": "closed"}}, nil)
		tip = commit([]string{tip, side2}, model.Event{TS: "2026-09-06T12:33:00.000",
			Type: "sync", Author: "sync"}, nil)
	case "sentinels":
		// A commit whose event.json is unparseable, and one carrying no
		// event.json at all.
		tip = commitRaw(t, s, []string{tip}, map[string]string{"event.json": "{not json"})
		tip = commitRaw(t, s, []string{tip}, map[string]string{"other.json": "{}"})
	}

	rawGit(t, dir, "update-ref", "refs/ledger/t", tip)
	return s, dir
}

// commitRaw writes a commit with arbitrary files, bypassing the event shape.
func commitRaw(t *testing.T, s Store, parents []string, files map[string]string) string {
	t.Helper()
	var entries []gitx.TreeEntry
	for name, content := range files {
		blob, err := s.Repo.WriteObject("blob", []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, gitx.TreeEntry{Name: name, ID: blob})
	}
	payload, err := gitx.EncodeTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.Repo.WriteObject("tree", payload)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := s.Repo.WriteObject("commit", gitx.CommitPayload(
		tree, parents, "t", "t", "1757180000 +0000", "raw"))
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// --- the differential assertion -------------------------------------------

func assertSameRead(t *testing.T, s Store, refName, label string) {
	t.Helper()
	wantEvs, wantMeta, wantDAG, wantErr := s.eventsDAGViaSpawn(refName, label)
	gotEvs, gotMeta, gotDAG, gotErr := s.eventsDAG(refName, label)

	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("error disagreement: spawn=%v persistent=%v", wantErr, gotErr)
	}
	if wantErr != nil {
		return
	}
	if len(gotEvs) != len(wantEvs) {
		t.Fatalf("event count: persistent %d, spawn %d", len(gotEvs), len(wantEvs))
	}
	for i := range wantEvs {
		if !reflect.DeepEqual(gotEvs[i], wantEvs[i]) {
			t.Fatalf("event %d differs:\n persistent %+v\n spawn      %+v", i, gotEvs[i], wantEvs[i])
		}
	}
	if !reflect.DeepEqual(gotMeta, wantMeta) {
		t.Fatalf("meta differs:\n persistent %+v\n spawn      %+v", gotMeta, wantMeta)
	}
	if !reflect.DeepEqual(gotDAG.Order, wantDAG.Order) {
		t.Fatalf("dag order differs (%d vs %d)", len(gotDAG.Order), len(wantDAG.Order))
	}
	if !reflect.DeepEqual(gotDAG.Roots, wantDAG.Roots) {
		t.Fatalf("dag roots differ:\n persistent %v\n spawn      %v", gotDAG.Roots, wantDAG.Roots)
	}
}

func TestPersistentReadMatchesSpawnedRead(t *testing.T) {
	for _, shape := range []string{"linear", "merges", "sentinels"} {
		for _, packed := range []bool{false, true} {
			name := shape
			if packed {
				name += "/packed"
			}
			t.Run(name, func(t *testing.T) {
				s, dir := buildFixture(t, shape, 12)
				if packed {
					rawGit(t, dir, "gc", "--aggressive", "--prune=now", "--quiet")
				}
				assertSameRead(t, s, "refs/ledger/t", "t")
			})
		}
	}
}

func TestPersistentReadUnknownRef(t *testing.T) {
	s, _ := buildFixture(t, "linear", 3)
	assertSameRead(t, s, "refs/ledger/nope", "nope")
	if _, _, _, err := s.eventsDAG("refs/ledger/nope", "nope"); err == nil {
		t.Fatal("an absent ref must still be unknown_ledger")
	}
}

// TestPersistentReadFromLinkedWorktree mirrors the writer's worktree test. The
// reader resolves through `-C dir`, so git does the layout work, but the
// fixture keeps that honest rather than assumed.
func TestPersistentReadFromLinkedWorktree(t *testing.T) {
	main := filepath.Join(t.TempDir(), "main")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", main).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	rawGit(t, main, "config", "user.email", "t@example.com")
	rawGit(t, main, "config", "user.name", "t")
	rawGit(t, main, "commit", "-q", "--allow-empty", "-m", "first")

	s := Store{Repo: gitx.Repo{Dir: main}}
	t.Cleanup(gitx.CloseBatchReaders)
	tip := ""
	for i := 0; i < 4; i++ {
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		ev := model.Event{TS: fmt.Sprintf("2026-09-06T12:00:%02d.000", i),
			Type: "set", Author: "t", Key: "d-t-1-01",
			Fields: map[string]string{"status": "open"}}
		var extra map[string]string
		if i == 0 {
			extra = map[string]string{"meta.json": `{"guard":["status"]}`}
		}
		sha, err := s.buildCommit(parents, ev, extra)
		if err != nil {
			t.Fatal(err)
		}
		tip = sha
	}
	rawGit(t, main, "update-ref", "refs/ledger/t", tip)

	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "-b", "feat", wt).CombinedOutput(); err != nil {
		t.Skipf("git worktree add: %v: %s", err, out)
	}
	if st, err := os.Stat(filepath.Join(wt, ".git")); err != nil || st.IsDir() {
		t.Skip("this git does not use a .git file for linked worktrees")
	}

	fromWT := Store{Repo: gitx.Repo{Dir: wt}}
	assertSameRead(t, fromWT, "refs/ledger/t", "t")
}

// --- failure modes, exercised rather than asserted ------------------------

func TestBatchSurvivesMissingObjectAndChildDeath(t *testing.T) {
	s, _ := buildFixture(t, "linear", 4)

	objs, err := s.Repo.Batch([]string{strings.Repeat("0", 40)})
	if err != nil {
		t.Fatalf("a missing object must be a typed result, not a stream error: %v", err)
	}
	if objs[0].Present {
		t.Fatal("the all-zero oid must report missing")
	}

	// The stream must still serve the next read.
	if _, _, _, err := s.eventsDAG("refs/ledger/t", "t"); err != nil {
		t.Fatalf("stream did not survive a missing object: %v", err)
	}

	// Kill the child underneath ourselves; the next read must respawn.
	gitx.CloseBatchReaders()
	if _, _, _, err := s.eventsDAG("refs/ledger/t", "t"); err != nil {
		t.Fatalf("read after the child was closed must respawn and succeed: %v", err)
	}
	assertSameRead(t, s, "refs/ledger/t", "t")
}
