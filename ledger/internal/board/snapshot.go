package board

import (
	"sort"

	"ledger/internal/model"
)

// Snapshot/Restore/ApplyTail are the board's half of the ledger fold cache
// (refs/ledger-cache/<slug>): a serializable projection of Build's result,
// and a way to resume that fold over a tail of later events instead of
// re-folding the whole chain.
//
// They live HERE, not in internal/cache, for one reason: Key carries two
// unexported fields (statusSeq, blockedBySeq) that are part of the answer
// and cannot be written or read from outside this package. Exporting them
// on Key would invite readers to compare positions across two different
// Build() calls, which is exactly the mistake their doc comments forbid.

// KeyState is one Key's whole state in a form another package can
// serialize. It mirrors Key field for field, INCLUDING the two unexported
// sequence positions - TestKeyStateMirrorsKey fails if Key grows a field
// this does not carry, because a silently dropped field would make a cached
// read disagree with a from-root read in a way only the differential test
// would catch.
type KeyState struct {
	Name         string                 `json:"name"`
	Title        string                 `json:"title"`
	SeedTitle    string                 `json:"seed_title"`
	Renames      []RenameRecord         `json:"renames"`
	Status       *FieldState            `json:"status"`
	LabelsID     string                 `json:"labels_id"`
	BlockedByID  string                 `json:"blocked_by_id"`
	BlockedByTS  string                 `json:"blocked_by_ts"`
	BlockedBySeq int                    `json:"blocked_by_seq"`
	Multi        map[string][]string    `json:"multi"`
	Fields       map[string]*FieldState `json:"fields"`
	StatusSeq    int                    `json:"status_seq"`
}

// ContestState is Contest in serializable form. Contest itself carries
// `Key` as json:"-" (the envelope entry that renders a contest already
// names its key), but the cache is keyed BY pair and must carry it.
type ContestState struct {
	Key     string   `json:"key"`
	Field   string   `json:"field"`
	IDs     []string `json:"ids"`
	Authors []string `json:"authors"`
	Expect  string   `json:"expect"`
	Human   bool     `json:"human"`
}

// Snapshot is the board's cacheable projection: every key's state, plus the
// live contests flattened into one (key, field)-sorted slice. A slice, not
// the by-key map Board carries, so the bytes are ordered by construction
// rather than by encoding/json's map-key sort.
type Snapshot struct {
	Keys     map[string]KeyState `json:"keys"`
	Contests []ContestState      `json:"contests"`
}

// Snapshot captures the board. Contests are whatever ComputeContests last
// stored; a board that never ran it snapshots none, which is the same thing
// it would render.
func (b *Board) Snapshot() Snapshot {
	s := Snapshot{Keys: make(map[string]KeyState, len(b.Keys)), Contests: []ContestState{}}
	for name, k := range b.Keys {
		s.Keys[name] = KeyState{
			Name: k.Name, Title: k.Title, SeedTitle: k.SeedTitle, Renames: k.Renames,
			Status: k.Status, LabelsID: k.LabelsID, BlockedByID: k.BlockedByID,
			BlockedByTS: k.BlockedByTS, BlockedBySeq: k.blockedBySeq,
			Multi: k.Multi, Fields: k.Fields, StatusSeq: k.statusSeq,
		}
	}
	for key, cs := range b.Contests {
		for _, c := range cs {
			s.Contests = append(s.Contests, ContestState{Key: key, Field: c.Field,
				IDs: c.IDs, Authors: c.Authors, Expect: c.Expect, Human: c.Human})
		}
	}
	sortContestStates(s.Contests)
	return s
}

// Restore rebuilds a Board from a snapshot. The result is
// indistinguishable from the Build (plus ComputeContests) it came from  -
// TestSnapshotRoundTripsBuild pins that, and the differential test in
// internal/cmd pins the rendered consequence.
func Restore(meta model.Meta, s Snapshot) *Board {
	b := &Board{Meta: meta, Keys: make(map[string]*Key, len(s.Keys))}
	for name, ks := range s.Keys {
		b.Keys[name] = &Key{
			Name: ks.Name, Title: ks.Title, SeedTitle: ks.SeedTitle, Renames: ks.Renames,
			Status: ks.Status, LabelsID: ks.LabelsID, BlockedByID: ks.BlockedByID,
			BlockedByTS: ks.BlockedByTS, blockedBySeq: ks.BlockedBySeq,
			Multi: ks.Multi, Fields: ks.Fields, statusSeq: ks.StatusSeq,
		}
	}
	if len(s.Contests) > 0 {
		byKey := make(map[string][]Contest, len(s.Contests))
		for _, c := range s.Contests {
			byKey[c.Key] = append(byKey[c.Key], Contest{Key: c.Key, Field: c.Field,
				IDs: c.IDs, Authors: c.Authors, Expect: c.Expect, Human: c.Human})
		}
		b.Contests = byKey
	}
	return b
}

// ApplyTail resumes the fold: it runs Build's own pass over `events` (the
// commits that landed after the snapshot's base, in fold order) against the
// restored board, numbering them from `offset` - the cached prefix's event
// count - so every sequence position stays a position in the whole chain.
//
// SOUNDNESS, and this is the whole argument the cache rests on. The caller
// must have proven that the tail attaches to the cached prefix ONLY at its
// base: a single-parent walk from head back to base, no merge in between.
// Given that, three things hold, and each one is load-bearing:
//
//   - The prefix IS a prefix of the whole chain's fold order. Every cached
//     node is an ancestor-or-self of base, so dag.Sort cannot emit it after
//     base; every tail node descends from base, so it cannot be emitted
//     before. The boundary is exactly base.
//   - The prefix's INTERNAL order is unchanged. Kahn's ready-set can only
//     hold nodes whose parents are all emitted, so no tail node is ever a
//     heap candidate until base pops - at which point the prefix is done.
//     A merge inside the range would break this (its second parent attaches
//     to the prefix's interior, and dag.Sort could then interleave), which
//     is why merge ranges re-fold from root instead.
//   - The tail's own order is its chain order. A single-parent run has one
//     topological order, so the tail needs no sort of its own.
//
// Titles re-resolve over the merged rename lists, not the tail's alone.
func (b *Board) ApplyTail(events []model.Event, offset int) {
	b.apply(events, offset)
	b.resolveTitles()
}

// AdvanceContests folds a tail of events into the restored contests.
//
// It never has to run the cover-set pass again, because the attachment
// condition ApplyTail documents settles the question outright: every tail
// write descends from base, and base descends from every cached node, so a
// tail write to (key, field) dominates EVERY earlier write to that pair.
// Two consequences, and together they are the whole update:
//
//   - A pair written in the tail collapses to exactly one write-head (the
//     tail's last write to it), so it is not contested. Drop it.
//   - A pair the tail does not touch keeps precisely the head set the cached
//     pass computed. Leave it.
//
// No tail can CREATE a contest: the run is single-parent, so within it each
// write to a pair descends from the previous one.
//
// The Human flag is recomputed rather than folded, off the same labels
// projection AllContests derives it from (latest labels write wins, "human"
// membership) - the board's own Multi["labels"], which apply has already
// advanced.
func (b *Board) AdvanceContests(events []model.Event) {
	if len(b.Contests) == 0 {
		return
	}
	type pair struct{ key, field string }
	written := map[pair]bool{}
	for _, e := range events {
		if e.Type != "set" || e.Key == "" {
			continue
		}
		for f := range e.Fields {
			if model.Contains(b.Meta.Guard, f) {
				written[pair{e.Key, f}] = true
			}
		}
		if writesField(e, TitleField) {
			written[pair{e.Key, TitleField}] = true
		}
	}
	for key, cs := range b.Contests {
		kept := make([]Contest, 0, len(cs))
		for _, c := range cs {
			if written[pair{key, c.Field}] {
				continue
			}
			if k := b.Keys[key]; k != nil {
				c.Human = k.HasHuman()
			} else {
				c.Human = false
			}
			kept = append(kept, c)
		}
		if len(kept) == 0 {
			delete(b.Contests, key)
			continue
		}
		b.Contests[key] = kept
	}
	// ComputeContests leaves the map nil when there is nothing to render, and
	// so must this: an empty non-nil map would serialize and compare as a
	// different board even though it renders identically.
	if len(b.Contests) == 0 {
		b.Contests = nil
	}
}

func sortContestStates(cs []ContestState) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Key != cs[j].Key {
			return cs[i].Key < cs[j].Key
		}
		return cs[i].Field < cs[j].Field
	})
}
