package board

import (
	"testing"

	"ledger/internal/model"
)

// releaseMeta is dgd's shape: closed and human are both terminal, but an
// edge releases only on status=closed AND the merged label.
func releaseMeta(rule *model.ReleaseRule) model.Meta {
	return model.Meta{
		Fields:      map[string][]string{"status": {"open", "in-progress", "closed", "human"}},
		Terminal:    map[string][]string{"status": {"closed", "human"}},
		MultiFields: []string{"labels", "blocked-by"},
		Release:     rule,
	}
}

var dgdRule = &model.ReleaseRule{Status: []string{"closed"}, Labels: []string{"merged"}}

// releaseEvs: dependent "dep" is open and blocked-by "blk", which is
// in blockerStatus with blockerLabels (empty = no labels write).
func releaseEvs(blockerStatus, blockerLabels string) []model.Event {
	evs := []model.Event{
		setEv("b1", "blk", "status", blockerStatus, func(e *model.Event) { e.Text = "blk" }),
	}
	if blockerLabels != "" {
		evs = append(evs, setEv("b2", "blk", "labels", blockerLabels, nil))
	}
	return append(evs,
		setEv("d1", "dep", "status", "open", func(e *model.Event) { e.Text = "dep" }),
		setEv("d2", "dep", "blocked-by", "blk", nil),
	)
}

func readyNames(env Envelope) map[string]bool {
	m := map[string]bool{}
	for _, r := range env.Ready {
		m[r.Key] = true
	}
	return m
}

func waitingState(t *testing.T, env Envelope, key string) string {
	t.Helper()
	for _, bl := range env.Blocked {
		if bl.Key == key {
			if len(bl.WaitingOn) != 1 {
				t.Fatalf("%s: want one waiting_on entry, got %+v", key, bl.WaitingOn)
			}
			return bl.WaitingOn[0].State
		}
	}
	t.Fatalf("%s is not in blocked: %+v", key, env)
	return ""
}

func TestReleaseRuleClosedAndMergedReleases(t *testing.T) {
	env := Build(releaseMeta(dgdRule), releaseEvs("closed", "merged")).Envelope(envNow, 50, alwaysTrue)
	if !readyNames(env)["dep"] {
		t.Fatalf("closed + merged must release the edge, got %+v", env)
	}
}

func TestReleaseRuleParkedHolds(t *testing.T) {
	env := Build(releaseMeta(dgdRule), releaseEvs("human", "merged")).Envelope(envNow, 50, alwaysTrue)
	if readyNames(env)["dep"] {
		t.Fatalf("a parked (human) blocker must not release the edge")
	}
	if got := waitingState(t, env, "dep"); got != "unreleased" {
		t.Fatalf("parked terminal blocker must report unreleased, got %q", got)
	}
}

func TestReleaseRuleClosedWithoutMergedHoldsThenReleases(t *testing.T) {
	env := Build(releaseMeta(dgdRule), releaseEvs("closed", "")).Envelope(envNow, 50, alwaysTrue)
	if readyNames(env)["dep"] {
		t.Fatalf("closed without merged must hold the dependent")
	}
	if got := waitingState(t, env, "dep"); got != "unreleased" {
		t.Fatalf("closed-unmerged blocker must report unreleased, got %q", got)
	}
	// An unrelated label does not satisfy the rule either.
	env = Build(releaseMeta(dgdRule), releaseEvs("closed", "urgent")).Envelope(envNow, 50, alwaysTrue)
	if readyNames(env)["dep"] {
		t.Fatalf("closed with only an unrelated label must hold the dependent")
	}
	// The same blocker releases once merged is added.
	evs := append(releaseEvs("closed", "urgent"), setEv("b3", "blk", "labels", "urgent,merged", nil))
	env = Build(releaseMeta(dgdRule), evs).Envelope(envNow, 50, alwaysTrue)
	if !readyNames(env)["dep"] {
		t.Fatalf("adding merged must release the dependent, got %+v", env)
	}
}

func TestReleaseRuleNoRuleAnyTerminalReleases(t *testing.T) {
	for _, st := range []string{"closed", "human"} {
		env := Build(releaseMeta(nil), releaseEvs(st, "")).Envelope(envNow, 50, alwaysTrue)
		if !readyNames(env)["dep"] {
			t.Fatalf("no rule: terminal %s must release, got %+v", st, env)
		}
	}
}

func TestReleaseRuleStatusOnlyIgnoresLabels(t *testing.T) {
	rule := &model.ReleaseRule{Status: []string{"closed"}}
	env := Build(releaseMeta(rule), releaseEvs("closed", "")).Envelope(envNow, 50, alwaysTrue)
	if !readyNames(env)["dep"] {
		t.Fatalf("status-only rule: closed must release")
	}
	env = Build(releaseMeta(rule), releaseEvs("human", "")).Envelope(envNow, 50, alwaysTrue)
	if readyNames(env)["dep"] {
		t.Fatalf("status-only rule: human must hold")
	}
}

func TestReleaseRuleCycleThroughUnmergedClosedBlocker(t *testing.T) {
	evs := []model.Event{
		setEv("a1", "a", "status", "open", func(e *model.Event) { e.Text = "a" }),
		setEv("a2", "a", "blocked-by", "b", nil),
		setEv("b1", "b", "status", "closed", nil),
		setEv("b2", "b", "blocked-by", "a", nil),
	}
	env := Build(releaseMeta(dgdRule), evs).Envelope(envNow, 50, alwaysTrue)
	if c := cycleEntries(env); len(c) != 1 {
		t.Fatalf("cycle through a closed, unmerged blocker must be reported, got %+v", env.Attention)
	}
	// With merged the chain ends at b: no cycle.
	evs = append(evs, setEv("b3", "b", "labels", "merged", nil))
	env = Build(releaseMeta(dgdRule), evs).Envelope(envNow, 50, alwaysTrue)
	if c := cycleEntries(env); len(c) != 0 {
		t.Fatalf("a released blocker's edges are moot, got cycle %+v", c)
	}
	// No rule: closed ends the chain, as today.
	env = Build(releaseMeta(nil), evs[:4]).Envelope(envNow, 50, alwaysTrue)
	if c := cycleEntries(env); len(c) != 0 {
		t.Fatalf("no rule: terminal blocker ends the chain, got cycle %+v", c)
	}
}

func TestReleaseRuleUnevidencedStillReadsTerminalEvent(t *testing.T) {
	evs := releaseEvs("closed", "merged")
	env := Build(releaseMeta(dgdRule), evs).Envelope(envNow, 50, alwaysTrue)
	if len(env.Ready) != 1 || len(env.Ready[0].UnblockedWithoutEvidence) != 1 {
		t.Fatalf("released blocker with no evidence must be reported, got %+v", env.Ready)
	}
	evs[0].Evidence = []string{"commit:abc"}
	env = Build(releaseMeta(dgdRule), evs).Envelope(envNow, 50, alwaysTrue)
	if len(env.Ready) != 1 || len(env.Ready[0].UnblockedWithoutEvidence) != 0 {
		t.Fatalf("evidence on the terminal event must clear the report, got %+v", env.Ready)
	}
}
