// dgd-265's second cache blob: refs/ledger-cache-index/<slug>, force-updated
// like refs/ledger-cache/<slug> and sharing every safety property this
// package's doc comment claims for that one - never authoritative, every
// validation failure degrades to a full fold, never to an error or a wrong
// answer.
//
// What it carries, that the v1 projection blob deliberately does not: a
// key-and-field-indexed spine of the latest SET event per (key, field) -
// exactly what `rowOf` (internal/cmd/read.go) reads to render a row - and
// one lightweight record per non-sync event, {id, blob_sha, type, kind,
// key, ts, author}, with no body. `status`, `show` and `notes` need rows
// and need to select events by kind/key/id-prefix; neither need the whole
// event list's TEXT, which is what made dgd-237's blob unable to serve them
// without carrying 38x more bytes than the projection it already sizes.
//
// Schema also rides along, resolved: meta.Fields as of the index's own
// base, plus every `vocab` event's addition folded in, in the same order
// and under the same containment rule fold.Fold applies (see BuildIndex and
// ApplyTail). This is the one piece of derived state beyond spine/events
// this blob carries, and it exists for the same reason State/SupersededBy
// live in the v1 blob: without it, `show`'s schema field cannot answer at
// all off the cache, and a store using `chit vocab` would render silently
// wrong output the day the write happened to a key nothing else touched.
//
// A verb wanting a full body - a note's text, an event's fields and
// evidence - reads this index for WHICH events to render, then does one
// batch fetch of blob_sha for exactly those events. It never has to fetch a
// body it will not render.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"

	"ledger/internal/model"
)

// IndexVersion is the index blob's schema version, independent of the
// projection blob's Version: the two evolve separately and either can
// change without invalidating the other. A blob carrying anything else is
// treated exactly like an absent ref.
const IndexVersion = 1

// SpineEntry is one (key, field) cell: the latest SET event's value, plus
// everything rowOf renders alongside it. Key and field are not repeated
// here - they are IndexBlob.Spine's two map keys.
type SpineEntry struct {
	Value    string   `json:"value"`
	Note     string   `json:"note"`
	By       string   `json:"by"`
	Branch   string   `json:"branch"`
	TS       string   `json:"ts"`
	ID       string   `json:"id"`
	Evidence []string `json:"evidence"`
}

// EventRec is one non-sync event's lightweight record: enough to select on
// (kind, key, an id prefix) and enough to fetch the body if selected
// (BlobSha), never the body itself.
type EventRec struct {
	ID      string `json:"id"`
	BlobSha string `json:"blob_sha"`
	Type    string `json:"type"`
	Kind    string `json:"kind,omitempty"`
	Key     string `json:"key,omitempty"`
	TS      string `json:"ts"`
	Author  string `json:"author"`
}

// IndexBlob is the index's on-disk schema. Field order here is the byte
// order (see Blob's own comment - the same rule, the same reason): sorted
// map keys plus a fixed struct field order gives exactly one encoding for
// one value.
type IndexBlob struct {
	V      int                              `json:"v"`
	Slug   string                           `json:"slug"`
	Base   string                           `json:"base"`
	Schema map[string][]string              `json:"schema"`
	Spine  map[string]map[string]SpineEntry `json:"spine"`
	Events []EventRec                       `json:"events"`
}

// Index is IndexBlob plus the read-time bookkeeping (Origin) that makes it
// interchangeable, by convention, with the from-root build the same way
// Projection is.
type Index struct {
	Slug   string
	Base   string
	Schema map[string][]string
	Spine  map[string]map[string]SpineEntry
	Events []EventRec
	Origin Origin
}

// cloneSchema copies meta.Fields the way fold.Fold's own schema init does:
// a nil declared field stays nil (unrestricted, never vocab-extended); a
// non-nil one is copied so later appends never alias the caller's slice.
func cloneSchema(fields map[string][]string) map[string][]string {
	out := make(map[string][]string, len(fields))
	for f, v := range fields {
		if v == nil {
			out[f] = nil
		} else {
			out[f] = append([]string{}, v...)
		}
	}
	return out
}

// applyIndexEvent is BuildIndex and ApplyTail's shared per-event step:
// fold.Fold's own set/vocab rules, replayed onto an index's Schema and
// Spine, plus the lightweight event record every non-sync event gets
// regardless of type. blobSha names the event.json blob this event's
// record points at, for a later body fetch.
func applyIndexEvent(schema map[string][]string, spine map[string]map[string]SpineEntry, ev model.Event, blobSha string) EventRec {
	switch ev.Type {
	case "set":
		for f, v := range ev.Fields {
			if spine[ev.Key] == nil {
				spine[ev.Key] = map[string]SpineEntry{}
			}
			spine[ev.Key][f] = SpineEntry{
				Value: v, Note: ev.Text, By: ev.Author, Branch: ev.Origin.Branch,
				TS: ev.TS, ID: ev.ID, Evidence: ev.Evidence,
			}
		}
	case "vocab":
		if cur, ok := schema[ev.Field]; ok && cur != nil && !model.Contains(cur, ev.Value) {
			schema[ev.Field] = append(schema[ev.Field], ev.Value)
		}
	}
	return EventRec{ID: ev.ID, BlobSha: blobSha, Type: ev.Type, Kind: ev.Kind, Key: ev.Key, TS: ev.TS, Author: ev.Author}
}

// BuildIndex folds evs (already sentinel-contracted, chronological - the
// same slice fold.Fold and cache.Project consume) into an Index. blobSha
// names the event.json blob sha for event id i - the caller already has it
// off the same cat-file batch that decoded evs, at eventsDAG or at
// scratch-fold time, so no second read is spent getting it.
func BuildIndex(slug, head string, evs []model.Event, meta model.Meta, blobSha func(id string) string) Index {
	schema := cloneSchema(meta.Fields)
	spine := map[string]map[string]SpineEntry{}
	events := make([]EventRec, 0, len(evs))
	for _, ev := range evs {
		events = append(events, applyIndexEvent(schema, spine, ev, blobSha(ev.ID)))
	}
	return Index{Slug: slug, Base: head, Schema: schema, Spine: spine, Events: events, Origin: OriginRoot}
}

// ApplyTail extends ix in place with a bounded tail of new events, the
// index's counterpart to board.Board.ApplyTail: same rule (fold.Fold's
// set/vocab logic), same replay order, over the (event, blob sha) pairs the
// walk already fetched.
func (ix *Index) ApplyTail(tail []tailEvent) {
	for _, te := range tail {
		ix.Events = append(ix.Events, applyIndexEvent(ix.Schema, ix.Spine, te.Event, te.BlobSha))
	}
}

// EncodeIndex is the index blob's bytes: deterministic by construction,
// exactly like Encode - sorted-key maps, fixed field order, no clock.
func EncodeIndex(ix Index) ([]byte, error) {
	schema := ix.Schema
	if schema == nil {
		schema = map[string][]string{}
	}
	spine := ix.Spine
	if spine == nil {
		spine = map[string]map[string]SpineEntry{}
	}
	events := ix.Events
	if events == nil {
		events = []EventRec{}
	}
	return json.Marshal(IndexBlob{V: IndexVersion, Slug: ix.Slug, Base: ix.Base, Schema: schema, Spine: spine, Events: events})
}

// decodeIndex turns blob bytes back into an Index. Origin is the caller's
// to set, exactly like decode.
func decodeIndex(raw []byte) (Index, error) {
	var b IndexBlob
	if err := json.Unmarshal(raw, &b); err != nil {
		return Index{}, err
	}
	if b.V != IndexVersion {
		return Index{}, fmt.Errorf("%w: schema v%d", ErrNoCache, b.V)
	}
	if len(b.Base) != 40 && len(b.Base) != 64 {
		return Index{}, fmt.Errorf("%w: base %q is not a full sha", ErrNoCache, b.Base)
	}
	return Index{Slug: b.Slug, Base: b.Base, Schema: b.Schema, Spine: b.Spine, Events: b.Events}, nil
}

// IndexSource binds one slug's index cache to a store - the same shape as
// Source, over the second ref and the index schema instead of the
// projection's. FoldIndex is this slug's from-root index build, the
// fallback every validation failure degrades to.
type IndexSource struct {
	Git       Git
	Slug      string
	LedgerRef string
	IndexRef  string
	FoldIndex func(rev string) (Index, error)
}

// LoadRaw reads the index ref's blob bytes, unvalidated.
func (s IndexSource) LoadRaw() ([]byte, string, error) { return loadRawBlob(s.Git, s.IndexRef) }

// Read is Source.Read's counterpart: the same three branches, over the
// index schema.
func (s IndexSource) Read() (Index, error) {
	if ix, ok := s.TryRead(); ok {
		return ix, nil
	}
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return s.FoldIndex(s.LedgerRef)
	}
	return s.FoldIndex(head)
}

// TryRead is Source.TryRead's counterpart, over the index schema - the
// cache-only half a dual-ref reader (store.CachedRead) needs to check
// "would this hit" without paying for a root fold on a miss.
func (s IndexSource) TryRead() (Index, bool) {
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return Index{}, false
	}
	raw, _, err := s.LoadRaw()
	if err != nil {
		return Index{}, false
	}
	ix, err := decodeIndex(raw)
	if err != nil || ix.Slug != s.Slug {
		return Index{}, false
	}
	if ix.Base == head {
		ix.Origin = OriginCache
		return ix, true
	}
	tailSHAs, ok := walkChain(s.Git, head, ix.Base, WalkBound)
	if !ok {
		return Index{}, false
	}
	tail, err := fetchTailEvents(s.Git, tailSHAs)
	if err != nil {
		return Index{}, false
	}
	ix.ApplyTail(tail)
	ix.Base = head
	ix.Origin = OriginTail
	return ix, true
}

// Write force-updates the index ref to a fresh blob of ix. Unlike
// Source.Write there is no HasMeta gate - the index carries no meta, so
// nothing here can be "unready to cache soundly" the way a metaless
// projection is.
func (s IndexSource) Write(ix Index) error {
	raw, err := EncodeIndex(ix)
	if err != nil {
		return err
	}
	sha, err := s.Git.WriteObject("blob", raw)
	if err != nil {
		return err
	}
	return s.Git.UpdateRefForce(s.IndexRef, sha)
}

// MaybeRefresh is Source.MaybeRefresh's counterpart: refresh only once the
// tail since this ref's own base has reached CacheEvery commits. The two
// refs refresh independently - see Source.MaybeRefresh's own comment for
// why cadence is measured per ref rather than shared, and the package doc's
// "same-head rule" note for what that costs a dual-ref read on the write
// path.
func (s IndexSource) MaybeRefresh() error {
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return nil
	}
	raw, _, err := s.LoadRaw()
	if err != nil {
		return s.refreshFromRoot(head)
	}
	ix, derr := decodeIndex(raw)
	if derr != nil || ix.Slug != s.Slug {
		return s.refreshFromRoot(head)
	}
	if ix.Base == head {
		return nil
	}
	if _, reached := walkChain(s.Git, head, ix.Base, CacheEvery); reached {
		return nil
	}
	refreshed, err := s.Read()
	if err != nil {
		return err
	}
	return s.Write(refreshed)
}

func (s IndexSource) refreshFromRoot(head string) error {
	ix, err := s.FoldIndex(head)
	if err != nil {
		return err
	}
	return s.Write(ix)
}

// Reset drops the ref and rebuilds it eagerly from root - the index half of
// `chit cache reset <slug>`.
func (s IndexSource) Reset() (Index, error) {
	if err := s.Git.DeleteRef(s.IndexRef); err != nil {
		return Index{}, err
	}
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return Index{}, fmt.Errorf("unknown_ledger: %s", s.Slug)
	}
	ix, err := s.FoldIndex(head)
	if err != nil {
		return Index{}, err
	}
	return ix, s.Write(ix)
}

// Verify is Source.Verify's counterpart, over the index schema - the same
// two comparisons (stored blob against a from-root fold of its own base;
// cached read at head against a from-root fold at head), the same exit
// contract (no differences iff the ref is absent, or a fold from root
// reproduces it byte for byte).
func (s IndexSource) Verify() ([]Diff, error) {
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return nil, fmt.Errorf("unknown_ledger: %s", s.Slug)
	}
	var diffs []Diff
	raw, sha, err := s.LoadRaw()
	if err != nil {
		var ue *UnreadableCacheError
		if errors.As(err, &ue) {
			return []Diff{{What: "cache_unreadable",
				Detail: fmt.Sprintf("%s points at an object that is not a cache blob: %s", s.IndexRef, ue.Reason)}}, nil
		}
		if errors.Is(err, ErrNoCache) {
			return nil, ErrNoCache
		}
		return []Diff{{What: "blob_unreadable", Detail: err.Error()}}, nil
	}
	stored, derr := decodeIndex(raw)
	if derr != nil {
		return []Diff{{What: "blob_undecodable", Detail: fmt.Sprintf("%s: %s", sha, derr)}}, nil
	}
	if stored.Slug != s.Slug {
		diffs = append(diffs, Diff{What: "slug_mismatch",
			Detail: fmt.Sprintf("blob names slug %q, ref is %s", stored.Slug, s.IndexRef)})
	}
	atBase, berr := s.FoldIndex(stored.Base)
	if berr != nil {
		diffs = append(diffs, Diff{What: "base_not_on_ledger",
			Detail: fmt.Sprintf("base %s does not fold on this store: %s", stored.Base, berr)})
	} else {
		atBase.Slug = s.Slug
		want, eerr := EncodeIndex(atBase)
		if eerr != nil {
			return nil, eerr
		}
		if string(want) != string(raw) {
			diffs = append(diffs, Diff{What: "stored_blob_differs",
				Detail: firstByteDiff(raw, want, fmt.Sprintf("stored index blob at base %s", stored.Base))})
		}
	}
	cached, cerr := s.Read()
	if cerr != nil {
		return nil, cerr
	}
	fromRoot, rerr := s.FoldIndex(head)
	if rerr != nil {
		return nil, rerr
	}
	gotBytes, e1 := EncodeIndex(cached)
	wantBytes, e2 := EncodeIndex(fromRoot)
	if e1 != nil || e2 != nil {
		return nil, errors.Join(e1, e2)
	}
	if string(gotBytes) != string(wantBytes) {
		diffs = append(diffs, Diff{What: "cached_read_differs",
			Detail: firstByteDiff(gotBytes, wantBytes, fmt.Sprintf("cached index read at head %s (origin %s)", head, cached.Origin))})
	}
	return diffs, nil
}
