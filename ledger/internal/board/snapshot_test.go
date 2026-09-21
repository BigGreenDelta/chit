package board

import (
	"reflect"
	"sort"
	"testing"

	"ledger/internal/dag"
	"ledger/internal/model"
)

// keyStateMirror maps every Key field to the KeyState field that carries
// it. The mapping is spelled out rather than derived so that ADDING a field
// to Key is a compile-and-test failure here, not a silently stale cache:
// a Key field this snapshot drops makes a cached read disagree with a
// from-root read, and only the differential test in internal/cmd would
// notice.
var keyStateMirror = map[string]string{
	"Name": "Name", "Title": "Title", "SeedTitle": "SeedTitle", "Renames": "Renames",
	"Status": "Status", "LabelsID": "LabelsID", "BlockedByID": "BlockedByID",
	"BlockedByTS": "BlockedByTS", "blockedBySeq": "BlockedBySeq", "Multi": "Multi",
	"Fields": "Fields", "statusSeq": "StatusSeq",
}

func TestKeyStateMirrorsKey(t *testing.T) {
	kt := reflect.TypeOf(Key{})
	var missing []string
	for i := 0; i < kt.NumField(); i++ {
		name := kt.Field(i).Name
		if _, ok := keyStateMirror[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("board.Key grew field(s) %v that KeyState does not carry - "+
			"add them to KeyState, Snapshot and Restore, or the fold cache will "+
			"answer with a board missing them", missing)
	}
	st := reflect.TypeOf(KeyState{})
	for keyField, stateField := range keyStateMirror {
		f, ok := st.FieldByName(stateField)
		if !ok {
			t.Fatalf("KeyState has no field %s (mirrors Key.%s)", stateField, keyField)
		}
		kf, _ := kt.FieldByName(keyField)
		if f.Type != kf.Type {
			t.Fatalf("KeyState.%s is %s but Key.%s is %s", stateField, f.Type, keyField, kf.Type)
		}
	}
}

// fixture is a small board with everything a Key can hold: a seed title, a
// rename, a guarded status write, labels (including the reserved human
// token), blocked-by, a second declared enum field, and an extra
// multi-field.
func fixture() (model.Meta, []model.Event) {
	meta := model.Meta{
		Slug: "demo", Created: "2026-01-01T00:00:00.000",
		Fields:      map[string][]string{"status": {"open", "in-progress", "closed"}, "priority": {"p0", "p1"}},
		Terminal:    map[string][]string{"status": {"closed"}},
		MultiFields: []string{"labels", "blocked-by", "reviewers"},
		Guard:       []string{"status", "blocked-by"},
	}
	ev := func(id, key string, f map[string]string, rename, text string) model.Event {
		return model.Event{ID: id, TS: "2026-01-0" + id[:1] + "T00:00:00.000", Type: "set",
			Key: key, Fields: f, Rename: rename, Text: text, Author: "a" + id}
	}
	evs := []model.Event{
		ev("1", "k-1", map[string]string{"status": "open"}, "", "seed one"),
		ev("2", "k-1", map[string]string{"labels": "human,urgent"}, "", ""),
		ev("3", "k-1", map[string]string{"status": "in-progress"}, "", "claimed"),
		ev("4", "k-1", nil, "renamed one", ""),
		ev("5", "k-2", map[string]string{"status": "open"}, "", "seed two"),
		ev("6", "k-2", map[string]string{"blocked-by": "k-1", "priority": "p0"}, "", ""),
		ev("7", "k-2", map[string]string{"reviewers": "x,y"}, "", ""),
	}
	return meta, evs
}

func TestSnapshotRoundTripsBuild(t *testing.T) {
	meta, evs := fixture()
	want := Build(meta, evs)
	want.ComputeContests(evs, dag.Result{})
	got := Restore(meta, want.Snapshot())
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("restore differs from build:\n want %#v\n  got %#v", want.Keys, got.Keys)
	}
}

// TestApplyTailEqualsWholeFold is the in-package half of the differential
// property: folding a prefix, snapshotting it, restoring it and applying
// the tail must produce the same board as folding everything at once  -
// sequence positions included, which is the part a naive resume gets wrong.
func TestApplyTailEqualsWholeFold(t *testing.T) {
	meta, evs := fixture()
	for split := 0; split <= len(evs); split++ {
		whole := Build(meta, evs)
		resumed := Restore(meta, Build(meta, evs[:split]).Snapshot())
		resumed.ApplyTail(evs[split:], split)
		if !reflect.DeepEqual(whole, resumed) {
			t.Fatalf("split %d: resumed board differs from whole fold", split)
		}
		for name, k := range whole.Keys {
			r := resumed.Keys[name]
			if k.statusSeq != r.statusSeq || k.blockedBySeq != r.blockedBySeq {
				t.Fatalf("split %d key %s: seq drift (status %d/%d, blocked-by %d/%d)",
					split, name, k.statusSeq, r.statusSeq, k.blockedBySeq, r.blockedBySeq)
			}
		}
	}
}

// TestAdvanceContestsDropsPairsTheTailWrites pins AdvanceContests' whole
// rule directly, without needing a merged git fixture: a cached contest on
// a pair the tail writes collapses (the tail write dominates every cached
// head), and one on a pair the tail leaves alone survives untouched.
func TestAdvanceContestsDropsPairsTheTailWrites(t *testing.T) {
	meta, evs := fixture()
	b := Build(meta, evs)
	b.Contests = map[string][]Contest{
		"k-1": {{Key: "k-1", Field: "status", IDs: []string{"3", "9"}, Expect: "9"}},
		// Human deliberately stale: k-2 carries no labels at all, so a
		// recompute must clear it. A folded-forward flag would keep it.
		"k-2": {{Key: "k-2", Field: "blocked-by", IDs: []string{"6", "8"}, Expect: "8", Human: true}},
	}
	tail := []model.Event{{ID: "10", TS: "2026-02-01T00:00:00.000", Type: "set", Key: "k-1",
		Fields: map[string]string{"status": "closed"}, Author: "z"}}
	b.ApplyTail(tail, len(evs))
	b.AdvanceContests(tail)
	if _, still := b.Contests["k-1"]; still {
		t.Fatal("a contest on a pair the tail wrote must collapse")
	}
	left := b.Contests["k-2"]
	if len(left) != 1 || left[0].Field != "blocked-by" || left[0].Expect != "8" {
		t.Fatalf("a contest on an untouched pair must survive verbatim: %#v", left)
	}
	if left[0].Human {
		t.Fatal("Human must be recomputed off the board's labels projection, not carried forward")
	}
}
