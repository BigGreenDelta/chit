package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ledger/internal/gitx"
)

// TestWalkChainVersusGitLog measures the thing the brief's 4.3 assumed away.
// Pipelining a parent frontier only helps a WIDE dag. A ledger chain is
// essentially linear, so each generation holds one commit and the walk
// degenerates to lockstep: one round trip per commit, against git log's single
// streaming pass.
func TestWalkChainVersusGitLog(t *testing.T) {
	for _, n := range []int{100, 1000, 5000} {
		s, _ := buildFixture(t, "linear", n)

		start := time.Now()
		commits, _, err := s.walkChain("refs/ledger/t")
		walk := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}

		start = time.Now()
		out, _, code := s.Repo.Git("", "log", "--format=%H%x09%P", "refs/ledger/t")
		gitlog := time.Since(start)
		if code != 0 {
			t.Fatal("git log failed")
		}
		lines := 0
		for _, c := range out {
			if c == '\n' {
				lines++
			}
		}

		t.Log(fmt.Sprintf("n=%-5d walkChain %8.1f ms (%d commits) | git log %8.1f ms | walk is %.1fx",
			n, float64(walk.Microseconds())/1000, len(commits),
			float64(gitlog.Microseconds())/1000,
			float64(walk)/float64(gitlog)))
	}
}

// --- measured and rejected, kept runnable ---------------------------------

// commitParents pulls the parent shas out of a raw commit object. Headers run
// until the first blank line; everything after it is the message, which may
// itself contain a line starting with "parent ".
func commitParents(body string) []string {
	var ps []string
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "parent "); ok {
			ps = append(ps, strings.TrimSpace(rest))
		}
	}
	return ps
}

// walkChain replaces `git log --format=%H%x09%P` with a traversal over the
// persistent reader: the commit list and every commit's parents, no spawn.
//
// Breadth-first by generation, and the whole frontier goes out as one batch.
// That is what makes a wide DAG safe: round-trip latency amortises across the
// frontier, so the merge-heavy chain that would be lockstep's worst case is
// pipelining's best one. Traversal order does not matter - the fold order
// comes from dag.Sort, never from the order the chain was read in.
func (s Store) walkChain(refName string) ([]string, [][]string, error) {
	head, err := s.Repo.Batch([]string{refName})
	if err != nil {
		return nil, nil, err
	}
	if len(head) == 0 || !head[0].Present || head[0].Type != "commit" {
		return nil, nil, fmt.Errorf("unresolved")
	}

	var commits []string
	var parents [][]string
	seen := map[string]bool{}

	record := func(o gitx.Object) []string {
		if seen[o.OID] {
			return nil
		}
		seen[o.OID] = true
		ps := commitParents(o.Content)
		commits = append(commits, o.OID)
		parents = append(parents, ps)
		return ps
	}

	frontier := record(head[0])
	for len(frontier) > 0 {
		want := make([]string, 0, len(frontier))
		queued := map[string]bool{}
		for _, sha := range frontier {
			if !seen[sha] && !queued[sha] {
				queued[sha] = true
				want = append(want, sha)
			}
		}
		if len(want) == 0 {
			break
		}
		objs, err := s.Repo.Batch(want)
		if err != nil {
			return nil, nil, err
		}
		var next []string
		for _, o := range objs {
			if !o.Present || o.Type != "commit" {
				continue
			}
			next = append(next, record(o)...)
		}
		frontier = next
	}
	return commits, parents, nil
}
