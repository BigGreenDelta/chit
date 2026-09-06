package gitx

// Reading refs without spawning git.
//
// A ref read is a file read: refs/<name>, falling back to packed-refs. The CAS
// is what makes this safe to do ourselves - `update-ref <ref> <new> <old>`
// performs its own locked comparison, so a stale read causes a retry, never a
// wrong write. The read is an optimisation of the CAS's first attempt, not the
// guarantee. That is categorically different from reusing an earlier FOLD as a
// precondition, which was proposed, rated high risk, and dropped: that would
// have weakened the precondition itself.
//
// THE TRAP, and it is the inverse of the one that bit the object writer.
// Objects live in the COMMON dir; a linked worktree's own gitdir holds none.
// Refs split BOTH ways:
//
//	refs/<name>, packed-refs   ->  common dir   (shared by every worktree)
//	HEAD                       ->  per-worktree gitdir
//
// So one root cannot serve both. Get it backwards and provenance silently
// reports the wrong branch inside a worktree - no error, just a wrong answer
// recorded in someone's name.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// gitDirOf resolves a repository path to that worktree's OWN gitdir - where
// HEAD lives. commonDir (objects.go) resolves to the shared one.
func gitDirOf(dir string) (string, error) {
	if st, err := os.Stat(filepath.Join(dir, "objects")); err == nil && st.IsDir() {
		return dir, nil // a bare store: its own gitdir and the common dir
	}
	gitdir := filepath.Join(dir, ".git")
	st, err := os.Stat(gitdir)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return gitdir, nil
	}
	raw, err := os.ReadFile(gitdir)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", os.ErrInvalid
	}
	return resolveRelative(dir, strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))), nil
}

// packedRefs caches one repository's packed-refs, keyed by the common dir and
// invalidated by the file's own mod time and size. Re-reading it per lookup
// would be correct but wasteful; never re-reading it would be wrong.
type packedCache struct {
	mu      sync.Mutex
	modTime int64
	size    int64
	refs    map[string]string
	loaded  bool
}

var packedCaches sync.Map // common dir -> *packedCache

// ReadRef resolves a full ref name (refs/ledger/<slug>, refs/heads/main, ...)
// to its sha. ok is false when the ref does not exist - the same answer
// `rev-parse -q --verify` gives, and callers already treat that as "absent",
// never as an error.
//
// Loose beats packed, which is git's own precedence: `update-ref` writes a
// loose ref that shadows any packed entry, and pack-refs removes the loose one
// only when it folds it in.
func (r Repo) ReadRef(name string) (string, bool) {
	common, err := commonDir(r.Dir)
	if err != nil {
		return "", false
	}
	if sha, ok := readLooseRef(common, name); ok {
		return sha, true
	}
	return readPackedRef(common, name)
}

func readLooseRef(common, name string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(common, filepath.FromSlash(name)))
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(raw))
	// A loose ref may be symbolic ("ref: refs/heads/main"). Chasing it is not
	// this function's job - callers ask for concrete ledger refs - so report
	// absent rather than return a ref name where a sha is expected.
	if line == "" || strings.HasPrefix(line, "ref:") {
		return "", false
	}
	return line, true
}

func readPackedRef(common, name string) (string, bool) {
	v, _ := packedCaches.LoadOrStore(common, &packedCache{})
	pc := v.(*packedCache)
	pc.mu.Lock()
	defer pc.mu.Unlock()

	path := filepath.Join(common, "packed-refs")
	st, err := os.Stat(path)
	if err != nil {
		pc.loaded, pc.refs = true, nil
		return "", false
	}
	if !pc.loaded || st.ModTime().UnixNano() != pc.modTime || st.Size() != pc.size {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		pc.refs = parsePackedRefs(string(raw))
		pc.modTime, pc.size, pc.loaded = st.ModTime().UnixNano(), st.Size(), true
	}
	sha, ok := pc.refs[name]
	return sha, ok
}

// parsePackedRefs reads the "<sha> <refname>" lines. A "^<sha>" line is a
// peeled annotated tag and belongs to the ref above it, never to itself.
func parsePackedRefs(s string) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		refs[strings.TrimSpace(name)] = strings.TrimSpace(sha)
	}
	return refs
}

// Head reports the checked-out branch and the short sha of HEAD, replacing
// `symbolic-ref --short -q HEAD` and `rev-parse --short HEAD` with one file
// read.
//
// HEAD is read from the PER-WORKTREE gitdir; the sha it names is then resolved
// against the COMMON dir, because that is where the ref it points at lives.
// Both halves of that sentence are load-bearing in a linked worktree.
//
// detached is true when HEAD holds a raw sha rather than a symref, which is
// how the caller reproduces `symbolic-ref -q`'s non-zero exit.
func (r Repo) Head() (branch, short string, detached bool) {
	gitdir, err := gitDirOf(r.Dir)
	if err != nil {
		return "", "", false
	}
	raw, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(string(raw))

	if rest, ok := strings.CutPrefix(line, "ref:"); ok {
		target := strings.TrimSpace(rest)
		sha, found := r.ReadRef(target)
		if !found {
			// An unborn branch: HEAD names a ref that does not exist yet.
			// git reports the branch and no sha, and so do we.
			return strings.TrimPrefix(target, "refs/heads/"), "", false
		}
		return strings.TrimPrefix(target, "refs/heads/"), shortSha(sha), false
	}
	return "", shortSha(line), true
}

// shortSha truncates to git's default abbreviation. git may lengthen this to
// stay unambiguous in a large repository; a ledger store holds one commit per
// event and nothing has ever collided, and this value is provenance rather
// than a lookup key.
func shortSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
