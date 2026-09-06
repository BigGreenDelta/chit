package gitx

// Writing git objects without spawning git.
//
// A loose object is a file and nothing more: zlib-deflate of
// "<type> <bytelen>\x00<payload>", stored at objects/<id[:2]>/<id[2:]>, where
// id is the hash of those same bytes UNCOMPRESSED. hash-object, mktree and
// commit-tree each do about fifteen lines of work in a separate process, and
// on Windows a process is ~22ms of CreateProcess. Measured 2026-09-06: the
// three of them cost 64.79ms per write against 1.37ms done here, a 47x gap
// that does not shrink as a ledger grows, because a write is always exactly
// three objects however long the chain is.
//
// This stays in gitx so the package doc keeps its meaning: gitx is still the
// only place the ledger knows anything about git. It is no longer only an
// exec layer.
//
// Deliberately NOT done here: reading. A whole-chain read in this style is
// one file open and one inflate per commit, which measured FASTER than git
// below about a hundred commits and three times SLOWER at two thousand -
// `git log` plus `cat-file --batch` amortises two processes over the whole
// chain, and a real ledger lives well past that crossover. Reads stay on git.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ObjectFormat is a repository's hash algorithm. A store declares it at
// creation and can never change it, so it is read once per directory.
type ObjectFormat string

const (
	SHA1   ObjectFormat = "sha1"
	SHA256 ObjectFormat = "sha256"
)

func (f ObjectFormat) new() hash.Hash {
	if f == SHA256 {
		return sha256.New()
	}
	return sha1.New()
}

// layout is what a repository's on-disk shape resolves to, cached per Repo.Dir
// because neither half can change under a live store.
type layout struct {
	objects string       // absolute path to the objects directory
	format  ObjectFormat // the hash the ids are made with
	err     error
}

var layouts sync.Map // Repo.Dir -> *layout

// resolve finds the objects directory and the hash algorithm.
//
// The objects directory is probed rather than assumed: chit's own stores are
// bare (`.ledger.git`), but a caller may hand us a work tree, where the same
// directory sits under .git.
//
// The hash comes from `extensions.objectFormat` in the repository's own config
// file, read directly rather than through `git config`, since a subprocess
// here would defeat the entire point. Absent the key, git's default is sha1 -
// that is the format's own rule, not a guess: repositoryformatversion 0 has no
// extensions at all, and a sha256 repository is REQUIRED to declare itself.
// Anything declared but unrecognised is refused rather than silently treated
// as sha1, which would mint correct-looking ids that are wrong.
func resolve(dir string) (*layout, error) {
	if v, ok := layouts.Load(dir); ok {
		l := v.(*layout)
		return l, l.err
	}
	l := &layout{format: SHA1}

	common, err := commonDir(dir)
	if err != nil {
		l.err = err
		layouts.Store(dir, l)
		return l, l.err
	}
	l.objects = filepath.Join(common, "objects")
	if od := os.Getenv("GIT_OBJECT_DIRECTORY"); od != "" {
		l.objects = od
	}

	cfg := filepath.Join(common, "config")
	if raw, err := os.ReadFile(cfg); err == nil {
		if f, ok := objectFormatFromConfig(string(raw)); ok {
			switch ObjectFormat(f) {
			case SHA1, SHA256:
				l.format = ObjectFormat(f)
			default:
				l.err = fmt.Errorf("unknown object format %q in %s", f, cfg)
			}
		}
	}

	layouts.Store(dir, l)
	return l, l.err
}

// commonDir resolves a repository path to the directory that actually holds
// objects/ and config - git's "common dir".
//
// Three shapes, and the second and third are why this function exists rather
// than a filepath.Join:
//
//   - A bare store (chit's own `.ledger.git`): dir itself.
//   - A work tree: dir/.git, an ordinary directory.
//   - A LINKED WORK TREE (`git worktree add`), a submodule, or
//     `--separate-git-dir`: dir/.git is a FILE reading "gitdir: <path>". That
//     gitdir is per-worktree and holds HEAD and its own refs, but NOT objects;
//     objects are shared, and the gitdir names their home in a `commondir`
//     file beside it.
//
// Assuming the second shape is a silent data-loss bug, not a lookup failure:
// MkdirAll would happily create an objects/ directory inside the worktree's
// gitdir and write there, git would never look, and the write would appear to
// succeed while vanishing. chit's own TestByBranchFold caught exactly this -
// it sets a field from inside a linked worktree and expects the main store to
// see it.
func commonDir(dir string) (string, error) {
	if st, err := os.Stat(filepath.Join(dir, "objects")); err == nil && st.IsDir() {
		return dir, nil // already a git dir, bare or otherwise
	}

	gitdir := filepath.Join(dir, ".git")
	st, err := os.Stat(gitdir)
	if err != nil {
		return "", fmt.Errorf("no git directory at %s", dir)
	}
	if !st.IsDir() {
		raw, err := os.ReadFile(gitdir)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", gitdir, err)
		}
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "gitdir:") {
			return "", fmt.Errorf("%s is not a gitdir pointer", gitdir)
		}
		gitdir = resolveRelative(dir, strings.TrimSpace(strings.TrimPrefix(line, "gitdir:")))
	}

	// A linked worktree's gitdir points at the shared objects via commondir.
	if raw, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		return resolveRelative(gitdir, strings.TrimSpace(string(raw))), nil
	}
	return gitdir, nil
}

// resolveRelative joins a git pointer path against its base, leaving an
// absolute pointer alone. git writes these relative as often as not.
func resolveRelative(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(base, p))
}

// objectFormatFromConfig pulls extensions.objectFormat out of a git config.
//
// A deliberately small INI reader: it recognises the one key that matters and
// ignores everything else, so a config it does not understand degrades to
// git's documented default rather than to an error. Section and key names are
// case-insensitive in git config; values are not.
func objectFormatFromConfig(s string) (string, bool) {
	section := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if end := strings.IndexByte(line, ']'); end > 0 {
				section = strings.ToLower(strings.TrimSpace(line[1:end]))
			}
			continue
		}
		if section != "extensions" {
			continue
		}
		k, v, cut := strings.Cut(line, "=")
		if !cut || strings.ToLower(strings.TrimSpace(k)) != "objectformat" {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, true
		}
	}
	return "", false
}

// ObjectFormatOf reports the hash a store's ids are made with.
func (r Repo) ObjectFormatOf() (ObjectFormat, error) {
	l, err := resolve(r.Dir)
	if err != nil {
		return "", err
	}
	return l.format, nil
}

// WriteObject writes one loose object and returns its id, exactly as
// `git hash-object -w --stdin` would for a blob.
//
// Writing is temp-file-then-rename, which is what git does and for the same
// reason: an object's NAME is a claim about its contents, so a torn write
// would leave a file that every later reader trusts and no later writer
// rewrites. An id that is already present is left alone - objects are
// immutable, so a second write of identical bytes is a no-op, not a conflict.
func (r Repo) WriteObject(typ string, payload []byte) (string, error) {
	l, err := resolve(r.Dir)
	if err != nil {
		return "", err
	}

	hdr := fmt.Sprintf("%s %d\x00", typ, len(payload))
	h := l.format.new()
	h.Write([]byte(hdr))
	h.Write(payload)
	id := hex.EncodeToString(h.Sum(nil))

	dir := filepath.Join(l.objects, id[:2])
	final := filepath.Join(dir, id[2:])
	if _, err := os.Stat(final); err == nil {
		return id, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("git_failed: %w", err)
	}

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write([]byte(hdr)); err != nil {
		return "", fmt.Errorf("git_failed: %w", err)
	}
	if _, err := zw.Write(payload); err != nil {
		return "", fmt.Errorf("git_failed: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("git_failed: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "tmp_obj_*")
	if err != nil {
		return "", fmt.Errorf("git_failed: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", fmt.Errorf("git_failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("git_failed: %w", err)
	}
	if err := os.Rename(name, final); err != nil {
		os.Remove(name)
		// Losing the rename to a concurrent writer is success, not failure:
		// the bytes are identical by construction, since the name IS the hash.
		if _, statErr := os.Stat(final); statErr == nil {
			return id, nil
		}
		return "", fmt.Errorf("git_failed: %w", err)
	}
	return id, nil
}

// TreeEntry is one regular file in a tree. Only blobs: the ledger stores flat
// event trees, never a subdirectory, which is what lets EncodeTree sort by
// raw name (git orders a directory as though its name ended in "/", a rule
// that only bites when trees nest).
type TreeEntry struct {
	Name string
	ID   string // hex object id of the blob
}

// EncodeTree renders a tree object's payload: "<mode> <name>\x00<raw id>" per
// entry, concatenated in name order.
//
// The sort is load-bearing, and it is the one thing `git mktree` was silently
// doing for us. git REQUIRES tree entries in name order and will call an
// unsorted tree corrupt under fsck; worse, store.buildCommit assembles its
// files in a Go map, whose iteration order is randomised per run. Unsorted,
// a multi-file event would hash differently from one run to the next while
// staying perfectly readable - intermittent, silent, and invisible until two
// stores that should agree do not.
func EncodeTree(entries []TreeEntry) ([]byte, error) {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b bytes.Buffer
	for _, e := range sorted {
		raw, err := hex.DecodeString(e.ID)
		if err != nil {
			return nil, fmt.Errorf("git_failed: bad object id %q: %w", e.ID, err)
		}
		b.WriteString("100644 ")
		b.WriteString(e.Name)
		b.WriteByte(0)
		b.Write(raw)
	}
	return b.Bytes(), nil
}

// CommitPayload renders a commit object's body.
//
// It reproduces `commit-tree`'s output byte for byte, including the identity
// shape IdentityArgs feeds it and git's own "<unix seconds> <+hhmm>" stamp in
// the local offset. Reproducing rather than improving is the point: `git log`
// ranks a DAG by commit date, so a stamp of a different resolution or zone
// could reorder a chain that carries sync merges.
func CommitPayload(tree string, parents []string, author, committer, when, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + " <" + AuthorEmail + "> " + when + "\n")
	b.WriteString("committer " + committer + " <" + CommitterEmail + "> " + when + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}
