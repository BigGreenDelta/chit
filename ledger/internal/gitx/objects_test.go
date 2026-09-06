package gitx

// These are differential tests, on purpose. The claim objects.go makes is not
// "this writes a reasonable object" but "this writes the SAME object git would
// have written", and the only witness worth having for that is git itself.
// Every test here asks git for the id and asserts ours equals it byte for byte.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func bareRepo(t *testing.T, extraInitArgs ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "s.git")
	args := append([]string{"init", "-q", "--bare"}, extraInitArgs...)
	args = append(args, dir)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Skipf("git init %v: %v: %s", extraInitArgs, err, out)
	}
	return dir
}

func TestWriteObjectMatchesGitHashObject(t *testing.T) {
	dir := bareRepo(t)
	r := Repo{Dir: dir}

	for _, payload := range []string{
		`{"type":"set","key":"d-dgd-1-01"}`,
		"",                           // the empty blob is a real object git knows
		"trailing newline\n",         // must not be trimmed
		"a\x00b\x01\xff",             // binary, not text
		strings.Repeat("x", 100_000), // past any small-buffer boundary
		"unicode: é中\U0001f600",      // multi-byte
	} {
		want, _, code := r.Git(payload, "hash-object", "-w", "--stdin")
		if code != 0 {
			t.Fatalf("git hash-object failed for %q", trunc(payload))
		}
		got, err := r.WriteObject("blob", []byte(payload))
		if err != nil {
			t.Fatalf("WriteObject(%q): %v", trunc(payload), err)
		}
		if got != want {
			t.Errorf("blob %q: got %s, git says %s", trunc(payload), got, want)
		}
	}

	// Whatever we wrote, git must be able to read back.
	if out, _, code := r.Git("", "fsck", "--strict", "--no-progress"); code != 0 {
		t.Errorf("git fsck --strict refused our objects: %s", out)
	}
}

func TestWriteObjectIsIdempotent(t *testing.T) {
	r := Repo{Dir: bareRepo(t)}
	first, err := r.WriteObject("blob", []byte("same bytes"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.WriteObject("blob", []byte("same bytes"))
	if err != nil {
		t.Fatalf("re-writing an existing object must be a no-op, got %v", err)
	}
	if first != second {
		t.Fatalf("%s != %s", first, second)
	}
}

// TestEncodeTreeSortsLikeMktree is the one that guards the trap: buildCommit
// assembles its files in a Go map, so the entries arrive in randomised order.
// git mktree used to sort them for us and no longer does.
func TestEncodeTreeSortsLikeMktree(t *testing.T) {
	r := Repo{Dir: bareRepo(t)}

	// Deliberately not in name order, and chosen so a naive concatenation
	// differs from the sorted one.
	names := []string{"zeta.json", "event.json", "alpha.json", "meta.json"}
	var entries []TreeEntry
	var mktree []string
	for _, n := range names {
		id, err := r.WriteObject("blob", []byte("content of "+n))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, TreeEntry{Name: n, ID: id})
		mktree = append(mktree, "100644 blob "+id+"\t"+n)
	}

	want, _, code := r.Git(strings.Join(mktree, "\n")+"\n", "mktree")
	if code != 0 {
		t.Fatal("git mktree failed")
	}
	payload, err := EncodeTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.WriteObject("tree", payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("tree id %s, git mktree says %s (entries out of order?)", got, want)
	}

	// And the order must not depend on the order they were handed to us.
	shuffled := []TreeEntry{entries[2], entries[0], entries[3], entries[1]}
	p2, err := EncodeTree(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.WriteObject("tree", p2)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != want {
		t.Fatalf("EncodeTree is order-dependent: %s vs %s", id2, want)
	}
}

func TestCommitPayloadMatchesCommitTree(t *testing.T) {
	r := Repo{Dir: bareRepo(t)}
	blob, err := r.WriteObject("blob", []byte(`{"event":1}`))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeTree([]TreeEntry{{Name: "event.json", ID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.WriteObject("tree", payload)
	if err != nil {
		t.Fatal(err)
	}

	const when = "1757180000 +0000"
	const author, committer, msg = "triage", "claude-code", "set:d-dgd-1-01"

	// Pin git's clock so the comparison is about our encoding, not the time.
	pinned := r.WithEnv(
		"GIT_AUTHOR_DATE=@"+when, "GIT_COMMITTER_DATE=@"+when)

	// Root commit, then a child, then a two-parent merge: the three shapes a
	// ledger actually mints (an ordinary append, and sync's sentinel merge).
	args := append(IdentityArgs(author, committer), "commit-tree", tree, "-m", msg)
	root, se, code := pinned.Git("", args...)
	if code != 0 {
		t.Fatalf("commit-tree: %s", se)
	}
	got, err := r.WriteObject("commit", CommitPayload(tree, nil, author, committer, when, msg))
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("root commit %s, git says %s", got, root)
	}

	args = append(IdentityArgs(author, committer), "commit-tree", tree, "-m", msg, "-p", root)
	child, se, code := pinned.Git("", args...)
	if code != 0 {
		t.Fatalf("commit-tree -p: %s", se)
	}
	got, err = r.WriteObject("commit", CommitPayload(tree, []string{root}, author, committer, when, msg))
	if err != nil {
		t.Fatal(err)
	}
	if got != child {
		t.Fatalf("child commit %s, git says %s", got, child)
	}

	args = append(IdentityArgs(author, committer), "commit-tree", tree, "-m", msg, "-p", root, "-p", child)
	merge, se, code := pinned.Git("", args...)
	if code != 0 {
		t.Fatalf("commit-tree merge: %s", se)
	}
	got, err = r.WriteObject("commit", CommitPayload(tree, []string{root, child}, author, committer, when, msg))
	if err != nil {
		t.Fatal(err)
	}
	if got != merge {
		t.Fatalf("merge commit %s, git says %s", got, merge)
	}

	if out, _, code := r.Git("", "fsck", "--strict", "--no-progress"); code != 0 {
		t.Errorf("fsck refused: %s", out)
	}
}

// TestSHA256RepoMatchesGit is the "just in case" case: a store created with
// --object-format=sha256 must get sha256 ids, not correct-looking sha1 ones.
func TestSHA256RepoMatchesGit(t *testing.T) {
	dir := bareRepo(t, "--object-format=sha256")
	r := Repo{Dir: dir}

	format, err := r.ObjectFormatOf()
	if err != nil {
		t.Fatal(err)
	}
	if format != SHA256 {
		t.Fatalf("detected %q, want sha256 - the config read is wrong", format)
	}

	for _, payload := range []string{"", `{"event":1}`, strings.Repeat("y", 50_000)} {
		want, _, code := r.Git(payload, "hash-object", "-w", "--stdin")
		if code != 0 {
			t.Fatalf("git hash-object failed")
		}
		got, err := r.WriteObject("blob", []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("sha256 blob: got %s, git says %s", got, want)
		}
		if len(got) != 64 {
			t.Errorf("sha256 id must be 64 hex chars, got %d (%s)", len(got), got)
		}
	}

	// Trees carry raw ids inline, so their width changes with the format too.
	blob, err := r.WriteObject("blob", []byte("tree member"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeTree([]TreeEntry{{Name: "event.json", ID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.WriteObject("tree", payload)
	if err != nil {
		t.Fatal(err)
	}
	want, _, code := r.Git("100644 blob "+blob+"\tevent.json\n", "mktree")
	if code != 0 {
		t.Fatal("git mktree failed under sha256")
	}
	if got != want {
		t.Fatalf("sha256 tree %s, git says %s", got, want)
	}

	if out, _, code := r.Git("", "fsck", "--strict", "--no-progress"); code != 0 {
		t.Errorf("fsck refused sha256 objects: %s", out)
	}
}

func TestSHA1IsTheDefaultAndUnknownFormatsRefuse(t *testing.T) {
	r := Repo{Dir: bareRepo(t)}
	f, err := r.ObjectFormatOf()
	if err != nil || f != SHA1 {
		t.Fatalf("a plain repo must read as sha1, got %q err=%v", f, err)
	}

	for _, tc := range []struct {
		name, config, want string
		ok                 bool
	}{
		{"absent", "[core]\n\tbare = true\n", "", false},
		{"sha256", "[extensions]\n\tobjectFormat = sha256\n", "sha256", true},
		{"case insensitive key", "[EXTENSIONS]\n\tOBJECTFORMAT = sha256\n", "sha256", true},
		{"commented out", "[extensions]\n\t# objectFormat = sha256\n", "", false},
		{"other section", "[core]\n\tobjectFormat = sha256\n", "", false},
		{"crlf", "[extensions]\r\n\tobjectFormat = sha256\r\n", "sha256", true},
		{"unknown", "[extensions]\n\tobjectFormat = blake3\n", "blake3", true},
	} {
		got, ok := objectFormatFromConfig(tc.config)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func trunc(s string) string {
	if len(s) > 30 {
		return fmt.Sprintf("%s...(%d bytes)", s[:30], len(s))
	}
	return s
}

// TestWriteObjectFromLinkedWorktree pins the shape that broke first: in a
// linked work tree, `.git` is a FILE naming a per-worktree gitdir, and that
// gitdir does NOT hold objects - they live in the shared common dir it names.
//
// Getting this wrong does not fail loudly. MkdirAll cheerfully creates an
// objects/ directory in the wrong place, the write reports success, and the
// object is invisible to every reader. chit's TestByBranchFold caught it only
// because it sets a field from inside a worktree and reads from the main
// store. This test states it directly.
func TestWriteObjectFromLinkedWorktree(t *testing.T) {
	main := filepath.Join(t.TempDir(), "main")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", main).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"-C", main, "config", "user.email", "t@example.com"},
		{"-C", main, "config", "user.name", "t"},
		{"-C", main, "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "-b", "feat", wt).CombinedOutput(); err != nil {
		t.Skipf("git worktree add: %v: %s", err, out)
	}
	if st, err := os.Stat(filepath.Join(wt, ".git")); err != nil || st.IsDir() {
		t.Skip("this git does not use a .git file for linked worktrees")
	}

	// Write through the WORKTREE path...
	fromWT := Repo{Dir: wt}
	id, err := fromWT.WriteObject("blob", []byte("written from a linked worktree"))
	if err != nil {
		t.Fatalf("WriteObject from worktree: %v", err)
	}

	// ...and it must be visible to git in the MAIN repository.
	out, _, code := Repo{Dir: main}.Git("", "cat-file", "-p", id)
	if code != 0 {
		t.Fatalf("object %s written from the worktree is invisible to the main repo", id)
	}
	if out != "written from a linked worktree" {
		t.Fatalf("content mismatch: %q", out)
	}

	// And it must be the id git itself would have minted.
	want, _, code := fromWT.Git("written from a linked worktree", "hash-object", "-w", "--stdin")
	if code != 0 || want != id {
		t.Fatalf("id %s, git says %s (code %d)", id, want, code)
	}

	// Nothing may have been created inside the per-worktree gitdir.
	if _, err := os.Stat(filepath.Join(wt, ".git", "objects")); err == nil {
		t.Errorf("created an objects dir inside the worktree gitdir - writes are going nowhere")
	}
}
