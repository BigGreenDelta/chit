package cmd

import (
	"strings"
	"testing"

	"ledger/internal/gitx"
	"ledger/internal/store"
)

// TestCreateReadyCapableShape: the spec's canonical board declaration
// succeeds end to end; a hand-broken all-or-nothing shape (--terminal on
// status without --guard status) is rejected at create time, exit 4,
// bad_value, naming the exact fix.
func TestCreateReadyCapableShape(t *testing.T) {
	dir := initRepo(t)
	_, se, code := run(t, dir, "create", "issues", "--scope", "s",
		"--field", "status=open,in-progress,closed,wontfix",
		"--terminal", "status=closed,wontfix",
		"--multi-field", "labels", "--multi-field", "blocked-by",
		"--guard", "status", "--guard", "blocked-by",
		"--require-evidence", "status=closed", "--stale-after", "2h")
	if code != 0 {
		t.Fatal(se)
	}
	_, se, code = run(t, dir, "create", "broken", "--scope", "s",
		"--field", "status=open,in-progress,closed", "--terminal", "status=closed")
	if code != 4 || !strings.Contains(se, "bad_value") || !strings.Contains(se, "--guard status") {
		t.Fatalf("all-or-nothing shape must be enforced: %s", se)
	}
}

// TestCreateDeclarationsRoundTrip: every new Meta field the CLI accepts
// makes it into the persisted meta.json unchanged.
func TestCreateDeclarationsRoundTrip(t *testing.T) {
	dir := initRepo(t)
	_, se, code := run(t, dir, "create", "issues", "--scope", "s",
		"--field", "status=open,in-progress,closed,wontfix",
		"--terminal", "status=closed,wontfix",
		"--multi-field", "labels", "--multi-field", "blocked-by",
		"--guard", "status", "--guard", "blocked-by",
		"--require-evidence", "status=closed", "--stale-after", "2h")
	if code != 0 {
		t.Fatal(se)
	}
	st := store.Store{Repo: gitx.Repo{Dir: dir}}
	_, meta, err := st.Events("issues")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.MultiFields) != 2 || meta.MultiFields[0] != "labels" || meta.MultiFields[1] != "blocked-by" {
		t.Fatalf("multi_fields round-trip: %v", meta.MultiFields)
	}
	if len(meta.Terminal["status"]) != 2 || !strings.Contains(strings.Join(meta.Terminal["status"], ","), "closed") {
		t.Fatalf("terminal round-trip: %v", meta.Terminal)
	}
	if len(meta.Guard) != 2 {
		t.Fatalf("guard round-trip: %v", meta.Guard)
	}
	if meta.StaleAfter != "2h" {
		t.Fatalf("stale_after round-trip: %v", meta.StaleAfter)
	}
}

// TestCreatePlainGuardedBoard: --guard without --terminal is a plain
// guarded board — ready-capability never opts in, so none of the
// all-or-nothing shape rules apply.
func TestCreatePlainGuardedBoard(t *testing.T) {
	dir := initRepo(t)
	_, se, code := run(t, dir, "create", "plain", "--scope", "s",
		"--field", "status=open,done", "--guard", "status")
	if code != 0 {
		t.Fatal(se)
	}
}

// TestCreateDeclarationRejections exercises each bad_value declaration
// rule through the real CLI flags (not just the model-level unit test).
func TestCreateDeclarationRejections(t *testing.T) {
	dir := initRepo(t)
	cases := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"terminal not subset",
			[]string{"create", "c1", "--scope", "s", "--field", "status=open,in-progress,closed",
				"--terminal", "status=nope"},
			"subset"},
		{"guard undeclared field",
			[]string{"create", "c2", "--scope", "s", "--field", "status=open,in-progress,closed",
				"--guard", "priority"},
			"not a declared field"},
		{"bad stale-after",
			[]string{"create", "c3", "--scope", "s", "--field", "status=open,in-progress,closed",
				"--stale-after", "2 hours"},
			"ParseDuration"},
		{"multi-field collides with enum field",
			[]string{"create", "c4", "--scope", "s", "--field", "status=open,in-progress,closed",
				"--multi-field", "status"},
			"collides"},
	}
	for _, c := range cases {
		_, se, code := run(t, dir, c.args...)
		if code != 4 || !strings.Contains(se, "bad_value") || !strings.Contains(se, c.wantMsg) {
			t.Fatalf("%s: want bad_value mentioning %q: %s", c.name, c.wantMsg, se)
		}
	}
}

// TestCreateReleaseRule: dgd's shape is accepted and persisted; a rule on a
// board that is not ready-capable, a status outside the terminal set, labels
// without the labels multi-field, and a malformed spec are all refused.
func TestCreateReleaseRule(t *testing.T) {
	dir := initRepo(t)
	base := []string{"--scope", "s", "--field", "status=open,in-progress,closed,human",
		"--terminal", "status=closed,human", "--guard", "status", "--guard", "blocked-by"}
	create := func(slug string, extra ...string) (string, int) {
		_, se, code := run(t, dir, append(append([]string{"create", slug}, base...), extra...)...)
		return se, code
	}
	if se, code := create("dgd", "--multi-field", "labels", "--multi-field", "blocked-by",
		"--release", "status=closed", "--release", "labels=merged"); code != 0 {
		t.Fatalf("dgd's shape must be accepted: %s", se)
	}
	meta, err := execGit(dir, "show", "refs/ledger/dgd:meta.json")
	if err != nil || !strings.Contains(meta, `"release"`) || !strings.Contains(meta, "merged") {
		t.Fatalf("rule must persist in meta.json (err %v): %s", err, meta)
	}
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"status-outside-terminal", []string{"--multi-field", "labels", "--multi-field", "blocked-by", "--release", "status=open"}, "terminal"},
		{"labels-without-multifield", []string{"--multi-field", "blocked-by", "--release", "status=closed", "--release", "labels=merged"}, "labels"},
		{"no-blocked-by", []string{"--multi-field", "labels", "--release", "status=closed"}, "blocked-by"},
		{"labels-only", []string{"--multi-field", "labels", "--multi-field", "blocked-by", "--release", "labels=merged"}, "at least one status"},
		{"bad-key", []string{"--multi-field", "labels", "--multi-field", "blocked-by", "--release", "owner=x"}, "only status and labels"},
		{"bad-spec", []string{"--multi-field", "labels", "--multi-field", "blocked-by", "--release", "closed"}, "must look like"},
	}
	for _, tc := range cases {
		se, code := create("bad-"+tc.name, tc.extra...)
		if code != 4 || !strings.Contains(se, "bad_value") || !strings.Contains(se, tc.want) {
			t.Errorf("%s: want exit 4 bad_value mentioning %q, got %d: %s", tc.name, tc.want, code, se)
		}
	}
	// Not ready-capable: a plain board.
	_, se, code := run(t, dir, "create", "plain", "--scope", "s", "--field", "status=open,done",
		"--multi-field", "blocked-by", "--release", "status=done")
	if code != 4 || !strings.Contains(se, "ready-capable") {
		t.Errorf("a rule on a non-ready-capable board must be refused: %d %s", code, se)
	}
}
