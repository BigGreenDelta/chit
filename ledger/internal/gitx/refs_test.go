package gitx

// Differential against git, like the object tests: the claim is that these
// read what `rev-parse -q --verify`, `symbolic-ref --short -q HEAD` and
// `rev-parse --short HEAD` report, not merely something plausible.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoWithCommits(t *testing.T, n int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	for _, a := range [][]string{
		{"-C", dir, "config", "user.email", "t@example.com"},
		{"-C", dir, "config", "user.name", "t"},
	} {
		exec.Command("git", a...).Run()
	}
	for i := 0; i < n; i++ {
		if out, err := exec.Command("git", "-C", dir, "commit", "-q", "--allow-empty",
			"-m", "c").CombinedOutput(); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
	}
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return strings.TrimSpace(string(out)), code
}

func TestReadRefMatchesRevParse(t *testing.T) {
	dir := repoWithCommits(t, 3)
	r := Repo{Dir: dir}
	head, _ := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "update-ref", "refs/ledger/t", head)

	for _, name := range []string{"refs/heads/main", "refs/ledger/t"} {
		want, code := gitOut(t, dir, "rev-parse", "-q", "--verify", name)
		got, ok := r.ReadRef(name)
		if (code == 0) != ok || (ok && got != want) {
			t.Errorf("%s: got (%q,%v), rev-parse says (%q, code %d)", name, got, ok, want, code)
		}
	}

	if sha, ok := r.ReadRef("refs/ledger/nope"); ok {
		t.Errorf("an absent ref must report absent, got %q", sha)
	}
}

// TestReadRefAfterPackRefs is the case a naive loose-only reader gets wrong,
// and then the case a naive packed-only reader gets wrong: pack-refs folds the
// loose file away, and a later update-ref writes a NEW loose file that must
// shadow the packed entry.
func TestReadRefAfterPackRefs(t *testing.T) {
	dir := repoWithCommits(t, 3)
	r := Repo{Dir: dir}
	head, _ := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "update-ref", "refs/ledger/t", head)
	gitOut(t, dir, "pack-refs", "--all")

	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "ledger", "t")); err == nil {
		t.Log("note: loose ref survived pack-refs on this git")
	}
	got, ok := r.ReadRef("refs/ledger/t")
	if !ok || got != head {
		t.Fatalf("packed ref: got (%q,%v), want %q", got, ok, head)
	}

	// Now move it. update-ref writes a loose ref that must win over the packed
	// entry, and the cache must not serve the stale packed value.
	older, _ := gitOut(t, dir, "rev-parse", "HEAD~1")
	gitOut(t, dir, "update-ref", "refs/ledger/t", older)
	got, ok = r.ReadRef("refs/ledger/t")
	if !ok || got != older {
		t.Fatalf("loose must shadow packed: got (%q,%v), want %q", got, ok, older)
	}
}

// TestPackedRefsCacheInvalidates guards the cache itself: a second pack-refs
// rewrites the file, and a cached parse must not survive it.
func TestPackedRefsCacheInvalidates(t *testing.T) {
	dir := repoWithCommits(t, 4)
	r := Repo{Dir: dir}
	head, _ := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "update-ref", "refs/ledger/t", head)
	gitOut(t, dir, "pack-refs", "--all")
	if got, _ := r.ReadRef("refs/ledger/t"); got != head {
		t.Fatalf("first read %q want %q", got, head)
	}

	older, _ := gitOut(t, dir, "rev-parse", "HEAD~2")
	gitOut(t, dir, "update-ref", "refs/ledger/t", older)
	gitOut(t, dir, "pack-refs", "--all")

	want, _ := gitOut(t, dir, "rev-parse", "-q", "--verify", "refs/ledger/t")
	if got, _ := r.ReadRef("refs/ledger/t"); got != want {
		t.Fatalf("cache served a stale packed ref: got %q, git says %q", got, want)
	}
}

func TestHeadMatchesGit(t *testing.T) {
	dir := repoWithCommits(t, 2)
	r := Repo{Dir: dir}

	wantBranch, bcode := gitOut(t, dir, "symbolic-ref", "--short", "-q", "HEAD")
	wantShort, _ := gitOut(t, dir, "rev-parse", "--short", "HEAD")
	branch, short, detached := r.Head()
	if bcode != 0 {
		t.Fatalf("fixture should be on a branch")
	}
	if detached || branch != wantBranch {
		t.Errorf("branch: got %q detached=%v, git says %q", branch, detached, wantBranch)
	}
	if short != wantShort {
		t.Errorf("short sha: got %q, git says %q", short, wantShort)
	}

	// Detached HEAD: symbolic-ref must fail and ours must say detached.
	sha, _ := gitOut(t, dir, "rev-parse", "HEAD~1")
	gitOut(t, dir, "checkout", "-q", "--detach", sha)
	_, bcode = gitOut(t, dir, "symbolic-ref", "--short", "-q", "HEAD")
	wantShort, _ = gitOut(t, dir, "rev-parse", "--short", "HEAD")
	branch, short, detached = r.Head()
	if bcode == 0 {
		t.Fatal("fixture should be detached")
	}
	if !detached {
		t.Errorf("must report detached, got branch %q", branch)
	}
	if short != wantShort {
		t.Errorf("detached short sha: got %q, git says %q", short, wantShort)
	}
}

// TestHeadAndRefsSplitAcrossWorktree is the trap the brief names, and the
// inverse of the one that bit the object writer. In a linked worktree HEAD is
// per-worktree while refs and packed-refs are shared. One root cannot serve
// both, and getting it backwards reports the WRONG BRANCH in provenance with
// no error at all.
func TestHeadAndRefsSplitAcrossWorktree(t *testing.T) {
	main := repoWithCommits(t, 3)
	head, _ := gitOut(t, main, "rev-parse", "HEAD")
	gitOut(t, main, "update-ref", "refs/ledger/t", head)

	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "-b", "feat", wt).CombinedOutput(); err != nil {
		t.Skipf("git worktree add: %v: %s", err, out)
	}
	if st, err := os.Stat(filepath.Join(wt, ".git")); err != nil || st.IsDir() {
		t.Skip("this git does not use a .git file for linked worktrees")
	}

	r := Repo{Dir: wt}

	// HEAD is the worktree's own: branch "feat", not "main".
	wantBranch, _ := gitOut(t, wt, "symbolic-ref", "--short", "-q", "HEAD")
	wantShort, _ := gitOut(t, wt, "rev-parse", "--short", "HEAD")
	branch, short, detached := r.Head()
	if detached || branch != wantBranch {
		t.Errorf("worktree branch: got %q, git says %q", branch, wantBranch)
	}
	if wantBranch != "feat" {
		t.Fatalf("fixture broken: worktree should be on feat, git says %q", wantBranch)
	}
	if short != wantShort {
		t.Errorf("worktree short sha: got %q, git says %q", short, wantShort)
	}

	// The ledger ref is SHARED, and must be visible from inside the worktree.
	want, code := gitOut(t, wt, "rev-parse", "-q", "--verify", "refs/ledger/t")
	got, ok := r.ReadRef("refs/ledger/t")
	if code != 0 {
		t.Fatal("fixture broken: the ledger ref should be visible from the worktree")
	}
	if !ok || got != want {
		t.Fatalf("shared ref from worktree: got (%q,%v), git says %q", got, ok, want)
	}
}

func TestParsePackedRefs(t *testing.T) {
	in := "# pack-refs with: peeled fully-peeled sorted \n" +
		"aaaa1111 refs/heads/main\n" +
		"bbbb2222 refs/tags/v1\n" +
		"^cccc3333\n" + // peeled tag: belongs to v1, never a ref of its own
		"dddd4444 refs/ledger/t\r\n"
	got := parsePackedRefs(in)
	want := map[string]string{
		"refs/heads/main": "aaaa1111",
		"refs/tags/v1":    "bbbb2222",
		"refs/ledger/t":   "dddd4444",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d refs, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}
