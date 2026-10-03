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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"ledger/internal/model"
)

// IndexVersion is the index blob's schema version, independent of the
// projection blob's Version: the two evolve separately and either can
// change without invalidating the other. A blob carrying anything else is
// treated exactly like an absent ref.
//
// v2 (dgd-272) adds Committers/EventRec.CI: the committer name dgd-272 moved
// off Store.Committers' per-call `git log` and into the blob itself, so a
// v1 blob (no Committers, no CI slots) is unreadable under v2 and degrades
// like any other version mismatch - never misread as an all-zero CI.
//
// v3 (dgd-431) frames the blob as separately decodable sections: one JSON
// header line (v, slug, base, count, schema, spine, committers, and a sorted
// key directory of byte offsets), then one JSON line per key holding that
// key's EventRecs. A verb that needs rows decodes the header alone; one that
// needs a single key's events decodes the header plus that key's line. A v2
// blob is one JSON document, so under v3 it reads as a miss.
const IndexVersion = 3

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

// EventRec is one non-sync event's lightweight record (Key is not encoded: a
// section's line is already that key's, and decode restores it): enough to select on
// (kind, key, an id prefix) and enough to fetch the body if selected
// (BlobSha), never the body itself. CI is this event's committer's slot in
// the blob's Committers list (dgd-272) - interned rather than a name per
// record, so a chain with few distinct committers stays near flat as the
// event count grows instead of repeating the same string thousands of
// times.
type EventRec struct {
	ID      string `json:"id"`
	BlobSha string `json:"blob_sha"`
	Type    string `json:"type"`
	Kind    string `json:"kind,omitempty"`
	Key     string `json:"-"`
	TS      string `json:"ts"`
	Author  string `json:"author"`
	CI      int    `json:"ci"`
	// Ord is this event's chain ordinal: its index in the whole non-sync
	// event list, dense from 0. Sections are per key, so Ord is what puts
	// the keys' records back into chain order for AllEvents.
	Ord int `json:"o"`
}

// indexHeader is the v3 blob's first line. Field order here is the byte
// order (see Blob's own comment - the same rule, the same reason): sorted
// map keys plus a fixed struct field order gives exactly one encoding for
// one value.
//
// Committers is the dgd-272 intern table, first-appearance order across the
// chain: EventRec.CI indexes into it. First-appearance order rather than
// sorted is what keeps this deterministic without a second sort key - the
// chain is already in the one chronological order every fold produces, so
// walking it once assigns each name the same slot every time.
//
// Count is the number of event records across every section. Keys is the
// section directory, sorted by key; Off and Len locate a section's JSON
// array in the bytes after the header line's newline.
type indexHeader struct {
	V          int                              `json:"v"`
	Slug       string                           `json:"slug"`
	Base       string                           `json:"base"`
	Count      int                              `json:"count"`
	Schema     map[string][]string              `json:"schema"`
	Spine      map[string]map[string]SpineEntry `json:"spine"`
	Committers []string                         `json:"committers"`
	Keys       []sectionRef                     `json:"keys"`
}

type sectionRef struct {
	Key string `json:"k"`
	Off int    `json:"off"`
	Len int    `json:"len"`
}

// ErrIndexSection marks a lazily read event section that does not decode.
// The header decoded, so the blob is not a miss, but a verb that needs the
// section must fall back to the root fold rather than error.
var ErrIndexSection = errors.New("index section unreadable")

// lazySections is a decoded header's view of the blob body: the raw bytes
// after the header line and where each key's section sits in them.
type lazySections struct {
	body []byte
	dir  map[string]sectionRef
	keys []string
}

// section decodes one key's records, or none when the key has no section.
func (l *lazySections) section(key string) ([]EventRec, error) {
	ref, ok := l.dir[key]
	if !ok {
		return nil, nil
	}
	var recs []EventRec
	if err := json.Unmarshal(l.body[ref.Off:ref.Off+ref.Len], &recs); err != nil {
		return nil, fmt.Errorf("%w: key %q: %v", ErrIndexSection, key, err)
	}
	for i := range recs {
		recs[i].Key = key
	}
	return recs, nil
}

// Index is the header's state plus the read-time bookkeeping (Origin) that
// makes it interchangeable, by convention, with the from-root build the same
// way Projection is. Event records are not a field: a header-only read never
// decodes them, so they sit behind EventsForKey and AllEvents. Count is the
// number of records the chain holds, tail included.
type Index struct {
	Slug       string
	Base       string
	Count      int
	Schema     map[string][]string
	Spine      map[string]map[string]SpineEntry
	Committers []string
	Origin     Origin

	lazy *lazySections         // records in the blob, still undecoded; nil for a from-root build
	mem  map[string][]EventRec // records held decoded, by key: a from-root build's all, a read's tail
}

// EventsForKey returns key's records in chain order: the blob's section for
// key (decoded now, this key only) followed by any tail records for it.
func (ix Index) EventsForKey(key string) ([]EventRec, error) {
	var recs []EventRec
	if ix.lazy != nil {
		var err error
		if recs, err = ix.lazy.section(key); err != nil {
			return nil, err
		}
	}
	return append(recs, ix.mem[key]...), nil
}

// AllEvents returns every record in chain order, decoding every section.
func (ix Index) AllEvents() ([]EventRec, error) {
	out := make([]EventRec, ix.Count)
	filled := 0
	place := func(recs []EventRec) error {
		for _, r := range recs {
			if r.Ord < 0 || r.Ord >= len(out) || out[r.Ord].ID != "" {
				return fmt.Errorf("%w: ordinal %d of %d", ErrIndexSection, r.Ord, len(out))
			}
			out[r.Ord] = r
			filled++
		}
		return nil
	}
	if ix.lazy != nil {
		for _, k := range ix.lazy.keys {
			recs, err := ix.lazy.section(k)
			if err == nil {
				err = place(recs)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	for _, recs := range ix.mem {
		if err := place(recs); err != nil {
			return nil, err
		}
	}
	if filled != len(out) {
		return nil, fmt.Errorf("%w: %d records for a count of %d", ErrIndexSection, filled, len(out))
	}
	return out, nil
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
// regardless of type. ord is the record's chain ordinal. blobSha names the event.json blob this event's
// record points at, for a later body fetch; ci is this event's already
// -interned committer slot (see internCommitter).
func applyIndexEvent(schema map[string][]string, spine map[string]map[string]SpineEntry, ev model.Event, blobSha string, ci, ord int) EventRec {
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
	return EventRec{ID: ev.ID, BlobSha: blobSha, Type: ev.Type, Kind: ev.Kind, Key: ev.Key, TS: ev.TS, Author: ev.Author, CI: ci, Ord: ord}
}

// internCommitter returns name's slot in *list, appending it in
// first-appearance order the first time name is seen. Shared by BuildIndex's
// from-root fold and ApplyTail's tail extension so both derive the identical
// ordering rule the blob's byte-determinism depends on.
func internCommitter(list *[]string, seen map[string]int, name string) int {
	if ci, ok := seen[name]; ok {
		return ci
	}
	ci := len(*list)
	*list = append(*list, name)
	seen[name] = ci
	return ci
}

// BuildIndex folds evs (already sentinel-contracted, chronological - the
// same slice fold.Fold and cache.Project consume) into an Index. blobSha
// names the event.json blob sha for event id i - the caller already has it
// off the same cat-file batch that decoded evs, at eventsDAG or at
// scratch-fold time, so no second read is spent getting it. committer names
// event id i's committer - dgd-272's replacement for a per-call
// Store.Committers `git log`, interned into Committers as evs is walked.
func BuildIndex(slug, head string, evs []model.Event, meta model.Meta, blobSha func(id string) string, committer func(id string) string) Index {
	schema := cloneSchema(meta.Fields)
	spine := map[string]map[string]SpineEntry{}
	mem := map[string][]EventRec{}
	committers := []string{}
	seen := map[string]int{}
	for i, ev := range evs {
		ci := internCommitter(&committers, seen, committer(ev.ID))
		rec := applyIndexEvent(schema, spine, ev, blobSha(ev.ID), ci, i)
		mem[rec.Key] = append(mem[rec.Key], rec)
	}
	return Index{Slug: slug, Base: head, Count: len(evs), Schema: schema, Spine: spine, Committers: committers, Origin: OriginRoot, mem: mem}
}

// ApplyTail extends ix in place with a bounded tail of new events, the
// index's counterpart to board.Board.ApplyTail: same rule (fold.Fold's
// set/vocab logic), same replay order, over the (event, blob sha) pairs the
// walk already fetched. committerOf keys the tail's committer names by
// event id (IndexSource.TailCommitters' shape, matching Store.Committers'
// own 10-char truncation), and new names are appended to ix.Committers in
// the same first-appearance order BuildIndex would have produced folding
// the whole chain in one pass - the cached prefix's names come first
// because they were interned first, and the tail's new names follow in the
// order the tail itself introduces them.
func (ix *Index) ApplyTail(tail []tailEvent, committerOf map[string]string) {
	seen := make(map[string]int, len(ix.Committers))
	for i, name := range ix.Committers {
		seen[name] = i
	}
	if ix.mem == nil {
		ix.mem = map[string][]EventRec{}
	}
	for _, te := range tail {
		ci := internCommitter(&ix.Committers, seen, committerOf[te.Event.ID])
		rec := applyIndexEvent(ix.Schema, ix.Spine, te.Event, te.BlobSha, ci, ix.Count)
		ix.Count++
		ix.mem[rec.Key] = append(ix.mem[rec.Key], rec)
	}
}

// EncodeIndex is the index blob's bytes: deterministic by construction,
// exactly like Encode - sorted-key maps, a sorted section directory, chain
// order within each section, no clock. It decodes every section of ix, so a
// section that does not decode is an ErrIndexSection error.
func EncodeIndex(ix Index) ([]byte, error) {
	all, err := ix.AllEvents()
	if err != nil {
		return nil, err
	}
	byKey := map[string][]EventRec{}
	for _, r := range all {
		byKey[r.Key] = append(byKey[r.Key], r)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var body []byte
	refs := make([]sectionRef, 0, len(keys))
	for _, k := range keys {
		line, err := json.Marshal(byKey[k])
		if err != nil {
			return nil, err
		}
		refs = append(refs, sectionRef{Key: k, Off: len(body), Len: len(line)})
		body = append(body, line...)
		body = append(body, '\n')
	}
	schema := ix.Schema
	if schema == nil {
		schema = map[string][]string{}
	}
	spine := ix.Spine
	if spine == nil {
		spine = map[string]map[string]SpineEntry{}
	}
	committers := ix.Committers
	if committers == nil {
		committers = []string{}
	}
	head, err := json.Marshal(indexHeader{V: IndexVersion, Slug: ix.Slug, Base: ix.Base, Count: len(all),
		Schema: schema, Spine: spine, Committers: committers, Keys: refs})
	if err != nil {
		return nil, err
	}
	head = append(head, '\n')
	return append(head, body...), nil
}

// decodeIndex decodes the blob's header line alone and keeps the rest of the
// bytes, undecoded, behind the Index's accessors. Origin is the caller's to
// set, exactly like decode.
func decodeIndex(raw []byte) (Index, error) {
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 {
		return Index{}, errors.New("index blob has no header line")
	}
	var h indexHeader
	if err := json.Unmarshal(raw[:nl], &h); err != nil {
		return Index{}, err
	}
	if h.V != IndexVersion {
		return Index{}, fmt.Errorf("%w: schema v%d", ErrNoCache, h.V)
	}
	if len(h.Base) != 40 && len(h.Base) != 64 {
		return Index{}, fmt.Errorf("%w: base %q is not a full sha", ErrNoCache, h.Base)
	}
	body := raw[nl+1:]
	if h.Count < 0 || h.Count > len(body) {
		return Index{}, fmt.Errorf("index blob count %d does not fit a %d byte body", h.Count, len(body))
	}
	l := &lazySections{body: body, dir: make(map[string]sectionRef, len(h.Keys)), keys: make([]string, 0, len(h.Keys))}
	for _, ref := range h.Keys {
		if ref.Off < 0 || ref.Len < 0 || ref.Off+ref.Len > len(body) {
			return Index{}, fmt.Errorf("index blob section %q lies outside the body", ref.Key)
		}
		l.dir[ref.Key] = ref
		l.keys = append(l.keys, ref.Key)
	}
	return Index{Slug: h.Slug, Base: h.Base, Count: h.Count, Schema: h.Schema, Spine: h.Spine,
		Committers: h.Committers, lazy: l}, nil
}

// decodeIndexFull is decodeIndex plus a decode of every section: the check
// Verify wants, and the one a write wants before it trusts a blob's records.
func decodeIndexFull(raw []byte) (Index, error) {
	ix, err := decodeIndex(raw)
	if err != nil {
		return Index{}, err
	}
	if _, err := ix.AllEvents(); err != nil {
		return Index{}, err
	}
	return ix, nil
}

// IndexSource binds one slug's index cache to a store - the same shape as
// Source, over the second ref and the index schema instead of the
// projection's. FoldIndex is this slug's from-root index build, the
// fallback every validation failure degrades to. TailCommitters answers a
// tail refresh's committer names - up to n commits back from head, the same
// mailmap-resolved path Store.Committers uses for a whole-chain read, just
// bounded - so a tail application never has to fall back to Store.Committers
// itself.
type IndexSource struct {
	Git            Git
	Slug           string
	LedgerRef      string
	IndexRef       string
	FoldIndex      func(rev string) (Index, error)
	TailCommitters func(head string, n int) (map[string]string, error)
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
	committers, err := s.TailCommitters(head, WalkBound)
	if err != nil {
		return Index{}, false
	}
	ix.ApplyTail(tail, committers)
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
	ix, derr := decodeIndexFull(raw)
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
	if err := s.Write(refreshed); err != nil {
		if errors.Is(err, ErrIndexSection) {
			return s.refreshFromRoot(head)
		}
		return err
	}
	return nil
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
	stored, derr := decodeIndexFull(raw)
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
