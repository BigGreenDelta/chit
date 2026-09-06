package gitx

// One long-lived `git cat-file --batch`, held open and reused.
//
// Reads stay on git deliberately. A hand-rolled loose-object walk measured
// faster than git below about a hundred commits and CANNOT READ A PACKED STORE
// AT ALL, and chit packs its own stores. What was expensive was never git's
// speed - it was paying CreateProcess once per read. So keep git, stop
// respawning it. Packfiles, delta chains and alternates stay git's problem,
// which is the whole point.
//
// Measured 2026-09-06 against a packed 2000-commit chain: 45.72ms through one
// persistent child against 71.91ms for `git log` plus a fresh `cat-file
// --batch`, and 184x at ten commits, where the two spawns dominate everything.
// Per object after warmup is ~23us, one pipe round trip.

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Object is one response from the batch stream. Missing is a legitimate
// answer, not an error: a torn or foreign commit carrying no event.json is
// contracted out of a fold rather than crashing the read.
type Object struct {
	OID     string
	Type    string
	Content string
	Present bool
}

type batchReader struct {
	// The batch protocol is a single interleaved request/response stream, so
	// one exchange must hold the lock from the first write to the last read.
	mu       sync.Mutex
	dir      string
	cmd      *exec.Cmd
	in       *bufio.Writer
	out      *bufio.Reader
	stdin    io.Closer
	lastRead int // bytes returned by the most recent exchange, for Repo.Bytes
	alive    bool
}

var (
	readersMu sync.Mutex
	readers   = map[string]*batchReader{}
)

// batchFor returns the reader for a directory, creating (but not spawning)
// it on first use. Repo is a value type, so the child cannot live on it.
func batchFor(dir string) *batchReader {
	readersMu.Lock()
	defer readersMu.Unlock()
	br, ok := readers[dir]
	if !ok {
		br = &batchReader{dir: dir}
		readers[dir] = br
	}
	return br
}

// CloseBatchReaders terminates every child. chit is a short-lived CLI and a
// child dies on stdin EOF anyway, but nothing should outlive the process by
// accident, and a long-running caller needs a way to say so.
func CloseBatchReaders() {
	readersMu.Lock()
	defer readersMu.Unlock()
	for _, br := range readers {
		br.mu.Lock()
		br.stop()
		br.mu.Unlock()
	}
}

// start spawns the child. Caller holds mu.
//
// No --buffer: it flushes only at stdin EOF, which for a persistent child
// never comes.
func (br *batchReader) start() error {
	// --git-dir, NOT -C. `-C dir` makes the child's working directory the
	// store, and Windows refuses to delete a directory that is any live
	// process's CWD - so a long-lived child pins the store for as long as it
	// runs. Short-lived calls never noticed; this one turned every test that
	// uses t.TempDir into a cleanup failure. Resolving the git dir ourselves
	// also keeps a linked worktree correct: HEAD and refs come from the
	// per-worktree gitdir, and git finds the shared objects through its own
	// commondir file.
	args := []string{}
	if br.dir != "" {
		gitdir, err := gitDirOf(br.dir)
		if err != nil {
			gitdir = br.dir
		}
		args = append(args, "--git-dir="+gitdir)
	}
	args = append(args, "cat-file", "--batch")
	cmd := exec.Command("git", args...)
	if len(br.envOf()) > 0 {
		cmd.Env = br.envOf()
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	br.cmd, br.stdin = cmd, stdin
	br.in, br.out = bufio.NewWriter(stdin), bufio.NewReaderSize(stdout, 64*1024)
	br.alive = true
	return nil
}

func (br *batchReader) envOf() []string { return nil }

// stop kills the child AND drops the buffered reader with it. Caller holds mu.
//
// Dropping the reader is not tidiness. After a torn payload the stream
// position is unknowable, so a retained reader would silently misalign every
// later response - returning one object's bytes under another's name, which no
// caller could detect.
func (br *batchReader) stop() {
	if !br.alive {
		return
	}
	if br.stdin != nil {
		br.stdin.Close()
	}
	if br.cmd != nil && br.cmd.Process != nil {
		br.cmd.Process.Kill()
		br.cmd.Wait()
	}
	br.cmd, br.in, br.out, br.stdin, br.alive = nil, nil, nil, nil, false
}

// Batch resolves every spec through the persistent child and returns the
// results positionally.
//
// Positional, never keyed by the echoed id: cat-file replies in request order
// and the same id may legitimately repeat within one request.
//
// The whole batch is written and flushed before any response is read, so
// round-trip latency amortises over the request rather than being paid per
// object. A caller walking a DAG should therefore send an entire parent
// frontier at once: the wide DAG that would be lockstep's worst case becomes
// pipelining's best one.
func (r Repo) Batch(specs []string) ([]Object, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	br := batchFor(r.Dir)
	br.mu.Lock()
	defer br.mu.Unlock()

	// Keep Repo's test-only counters honest. They live on GitRaw, and a reader
	// that bypasses GitRaw would make them BLIND rather than wrong: the scale
	// tests assert on bytes moved to prove a guarded write reads the whole
	// chain, and an uncounted 2.8MB read looks exactly like a narrow window
	// they exist to forbid. One Batch is one logical git invocation's worth of
	// work, which is what the old single spawn counted.
	defer func() {
		if r.Calls != nil {
			atomic.AddInt64(r.Calls, 1)
		}
		if r.Bytes != nil {
			n := 0
			for _, s := range specs {
				n += len(s) + 1
			}
			atomic.AddInt64(r.Bytes, int64(n+br.lastRead))
		}
	}()

	out, err := br.exchange(specs)
	if err == nil {
		return out, nil
	}
	// A stream-level failure takes the whole child with it, so every later
	// read would fail identically. Respawn once; failing after that keeps a
	// broken git from looking like a hang.
	br.stop()
	if serr := br.start(); serr != nil {
		return nil, fmt.Errorf("git_failed: cat-file respawn: %w (after %v)", serr, err)
	}
	return br.exchange(specs)
}

// exchange performs one write-all-then-read-all round. Caller holds mu.
func (br *batchReader) exchange(specs []string) ([]Object, error) {
	if !br.alive {
		if err := br.start(); err != nil {
			return nil, fmt.Errorf("git_failed: cat-file --batch: %w", err)
		}
	}
	// Write and read CONCURRENTLY. Writing the whole request first and only
	// then reading deadlocks on any batch large enough to matter: a pipe
	// buffer is about 64KB, so at 5000 events - 10,000 specs in, ~2.8MB of
	// objects back - the child blocks writing stdout while we block writing
	// stdin, and neither can proceed. It reached this codebase as a scale test
	// that went from four minutes to not finishing.
	//
	// The mutex is still held across the whole exchange by the caller, so this
	// is one writer and one reader on a stream that stays strictly ordered.
	werr := make(chan error, 1)
	go func() {
		for _, s := range specs {
			if _, err := br.in.WriteString(s + "\n"); err != nil {
				werr <- err
				return
			}
		}
		werr <- br.in.Flush()
	}()

	br.lastRead = 0
	objs := make([]Object, len(specs))
	var readErr error
	for i := range specs {
		o, err := br.readOne()
		if err != nil {
			readErr = err
			break
		}
		objs[i] = o
	}

	// Always collect the writer, even on a read failure: leaving it blocked on
	// a pipe nobody drains would leak a goroutine per failed batch.
	if err := <-werr; err != nil && readErr == nil {
		readErr = err
	}
	if readErr != nil {
		return nil, readErr
	}
	return objs, nil
}

// readOne reads a single response. Caller holds mu.
func (br *batchReader) readOne() (Object, error) {
	header, err := br.out.ReadString('\n')
	if err != nil {
		return Object{}, fmt.Errorf("cat-file header: %w", err)
	}
	br.lastRead += len(header)
	fields := strings.Fields(strings.TrimRight(header, "\r\n"))
	if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
		return Object{OID: fields[0]}, nil
	}
	if len(fields) < 3 {
		return Object{}, fmt.Errorf("bad cat-file header %q", header)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil {
		return Object{}, fmt.Errorf("bad size in cat-file header %q", header)
	}

	// Read exactly size bytes. A payload is arbitrary binary and may itself end
	// in "\n", so the trailing newline is a delimiter to consume, never a
	// terminator to scan for. Getting this wrong truncates any event whose
	// JSON happens to end in a newline.
	buf := make([]byte, size)
	if _, err := io.ReadFull(br.out, buf); err != nil {
		return Object{}, fmt.Errorf("cat-file payload (%d bytes): %w", size, err)
	}
	br.lastRead += size + 1
	if _, err := br.out.ReadByte(); err != nil {
		return Object{}, fmt.Errorf("cat-file trailer: %w", err)
	}
	return Object{OID: fields[0], Type: fields[1], Content: string(buf), Present: true}, nil
}
