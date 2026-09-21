// Package cache is the ledger fold cache: a force-updated sibling ref,
// refs/ledger-cache/<slug>, pointing straight at a BLOB that carries the
// folded board projection plus the ledger sha it was folded from (D76, as
// revised 2026-09-18 - not a checkpoint commit in the chain).
//
// What it is for. `chit ready` and an idle `chit watch` tick used to fold
// the whole chain on every call; on a 16,000-event ledger that is
// dominated by the fold, not by reading the ref. With a cache present they
// read one blob and walk the bounded tail of commits that landed since.
//
// What it is NOT. The cache is never authoritative. Every validation
// failure - absent ref, wrong version, foreign base, a base ahead of head,
// a merge in the range, a range longer than the bound - degrades to a full
// fold from root, never to an error. That property is the entire reason a
// pushed cache is safe without a handshake between replicas: a reader that
// cannot prove the blob describes its own history simply ignores it.
//
// What it deliberately does not carry: the event list. fold.Ledger.Events,
// Spine, Parent, Losers and dag.Result all stay out (D76 sizes the
// projection at 353,770 bytes against a 38x-larger event history).
//
// WHAT THIS DOES NOT FIX, stated with the numbers, because the name "ledger
// cache" invites the opposite assumption. Measured on the live board,
// 2026-09-21:
//
//	GET /api/task/<key>   2.3 - 4.3 s
//	  of which ~2.4 s is two chit subprocesses:
//	    View.task -> client.status(key) -> client.notes(key, limit=50)
//	GET /api/board         12 ms alone
//	                       11 s with two card-detail requests in flight
//
// The page polls the board every 5 s while a card is open, so opening a
// card starves that poll and the whole UI stalls - reported by the operator
// as "about 10s" and as the card being slow. The card is not slow; it has
// taken the machine. That delay runs through `chit status` and `chit notes`,
// and NEITHER is touched here: `status`, `show`, `ls`, `where`, `rollup` and
// `notes` still load the whole chain. `notes` in particular needs the event
// list this cache deliberately refuses to carry, so making it fast is
// separate work with a different shape and size, not a follow-up tweak to
// this. Only `ready` and an idle `watch` tick get faster when this lands.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ledger/internal/board"
	"ledger/internal/dag"
	"ledger/internal/fold"
	"ledger/internal/gitx"
	"ledger/internal/model"
)

// Version is the blob schema version. A blob carrying anything else is
// ignored exactly like an absent one - a version bump is a cache flush, by
// construction, and needs no migration.
const Version = 1

// CacheEvery is the write-path cadence: the cache ref is refreshed once the
// tail since its base reaches this many non-sentinel commits, never once
// per write.
//
// Why 50, against the 2026-09-17 guest measurements in D76. Per-write
// cadence writes one loose blob per event; at the measured live volume
// (2,205 events/day) with a 353,770-byte projection that is about 136 MB of
// loose objects a day, which trips the guest's automatic packing roughly 7
// times a day - and repacking a live ledger repo under the factory is the
// cost this whole change exists to avoid paying. At every-50 the same day
// writes about 44 blobs, roughly 2 MB/day loose, and packing stays a
// background event rather than a workload.
//
// The read side pays the other half of the trade: a 50-commit tail at
// D76's measured 50.8 us/event fold slope is about 2.5 ms, against 586 ms
// to fold 16,000 events from root. Pushing the cadence lower buys
// milliseconds on the read and costs megabytes on the write; pushing it
// much higher makes the tail walk, not the fold, the thing to measure.
const CacheEvery = 50

// WalkBound caps the parent walk, which is what stops a stale cache from
// turning a cheap read into a long one. Four times the cadence: a healthy
// store refreshes at 50, and the slack absorbs refreshes that were skipped
// (a crashed write, a peer running an older binary) without ever letting
// the walk approach whole-chain cost. Past the bound the cache is ignored
// and the read folds from root, which is the same answer, just slower.
const WalkBound = 4 * CacheEvery

// Origin records where a projection came from, for `cache` verbs and tests
// to assert on. Nothing in a rendered payload depends on it.
type Origin string

const (
	OriginCache Origin = "cache" // the blob's base was the ledger head
	OriginTail  Origin = "tail"  // the blob plus a bounded tail fold
	OriginRoot  Origin = "root"  // no usable cache: folded from root
)

// ErrNoCache is Load's miss. Callers degrade; they never surface it.
var ErrNoCache = errors.New("no_cache")

// UnreadableCacheError is the narrower miss where the ref EXISTS but the
// object under it is not a cache blob at all - a commit sha, a tree, an
// object this store does not have. It unwraps to ErrNoCache, so every READ
// path degrades on it exactly as it degrades on absence: presence of a ref
// is never trust. `cache verify` is the one caller that tells the two
// apart, because they are different claims. Absence claims nothing and
// cannot be wrong. A ref in this namespace pointing at a non-blob is a
// claim, and it is false - nothing that writes this namespace produces
// one, so something else wrote it.
type UnreadableCacheError struct{ Sha, Reason string }

func (e *UnreadableCacheError) Error() string { return e.Reason }
func (e *UnreadableCacheError) Unwrap() error { return ErrNoCache }

// Blob is the cache blob's schema, v1. Every field is derived from the
// chain and nothing is derived from the clock: there is no generated_at, no
// host, no duration. Two folds of the same sha must produce identical
// bytes, in the same process or in different store directories, and a
// timestamp is the one thing that would make that impossible.
//
// Field order here IS the byte order: encoding/json emits struct fields in
// declaration order and sorts map keys, so a Blob has exactly one encoding.
type Blob struct {
	V     int    `json:"v"`
	Slug  string `json:"slug"`
	Base  string `json:"base"`
	Count int    `json:"count"`
	// LastTS is the fold's last event timestamp - what the ambient
	// ledger-picking loop sorts open ledgers by when more than one is open.
	LastTS string `json:"last_ts"`
	// State, SupersededBy and ExtraLinks are fold.Ledger's three scalars
	// that a `ready` read actually consults: State decides which ledger the
	// ambient loop picks, the other two are the redirect a superseded
	// ledger's payload must carry. They are in here because without them a
	// cached read cannot answer `ready` at all and would have to fold the
	// chain anyway, which would leave the cache doing nothing.
	State        string   `json:"state"`
	SupersededBy string   `json:"superseded_by"`
	ExtraLinks   []string `json:"extra_links"`
	// Roots is the sentinel-contracted DAG's root set. Cacheable exactly:
	// under the attachment condition the tail's commits all have a parent,
	// so no tail commit is ever a root and the whole chain's root set equals
	// the cached prefix's. The read-time freshness check compares against
	// it, so a cached read that dropped it would start warning about a root
	// mismatch that is not there.
	Roots []string `json:"roots"`
	// Meta is the chain's meta.json. Folded reads resolve meta from the
	// OLDEST commit carrying one, so a prefix's meta is the whole chain's  -
	// unless the prefix carries none at all, which is why HasMeta exists and
	// why a metaless ledger is never cached.
	HasMeta bool                      `json:"has_meta"`
	Meta    model.Meta                `json:"meta"`
	Keys    map[string]board.KeyState `json:"keys"`
	Contest []board.ContestState      `json:"contests"`
}

// Projection is the board a `ready` read needs, plus the bookkeeping that
// makes it cacheable. It is what both the cached path and the from-root
// path return, so the two are interchangeable by type, not by convention.
type Projection struct {
	Slug         string
	Meta         model.Meta
	HasMeta      bool
	Board        *board.Board
	State        string
	SupersededBy string
	ExtraLinks   []string
	Roots        []string
	LastTS       string
	// Base is the ledger commit this projection describes, full 40 chars.
	Base   string
	Count  int
	Origin Origin
}

// Git is the git surface the cache needs. store.Store satisfies it; taking
// an interface is what keeps this package out of store's import cycle.
type Git interface {
	RevParse(rev string) (string, bool)
	Batch(specs []string) ([]gitx.Object, error)
	WriteObject(typ string, payload []byte) (string, error)
	UpdateRefForce(name, sha string) error
	DeleteRef(name string) error
}

// Source binds one slug's cache to a store. Fold is the from-root fold of
// an arbitrary rev - the fallback every validation failure lands in, and
// the other half of `cache verify`'s comparison.
type Source struct {
	Git       Git
	Slug      string
	LedgerRef string
	CacheRef  string
	Fold      func(rev string) (Projection, error)
}

// Project builds a Projection from a whole-chain read. The from-root path
// is exactly this, which is what makes the cached path's job "reproduce
// Project's output", not "compute something similar".
func Project(slug, head string, evs []model.Event, meta model.Meta, d dag.Result) Projection {
	led := fold.Fold(slug, evs, meta)
	b := board.Build(meta, evs)
	b.ComputeContests(evs, d)
	return Projection{
		Slug: slug, Meta: meta, HasMeta: hasMeta(meta),
		Board: b, State: led.State, SupersededBy: led.SupersededBy, ExtraLinks: led.ExtraLinks,
		Roots: d.Roots, LastTS: lastTS(evs), Base: head, Count: len(evs), Origin: OriginRoot,
	}
}

// hasMeta reports whether the chain this projection folded actually
// carried a readable meta.json. Every minting path (create, import, adopt)
// writes slug and created, so either is proof; Fields is checked too so a
// hand-built board without those still counts.
func hasMeta(m model.Meta) bool {
	return m.Slug != "" || m.Created != "" || len(m.Fields) > 0
}

func lastTS(evs []model.Event) string {
	if len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].TS
}

// Encode is the blob's bytes: deterministic by construction. Sorted-key
// maps only (encoding/json sorts them), fixed field order, integers
// formatted by encoding/json's own integer path - no floats anywhere in the
// schema, so no formatting choice to drift.
func Encode(p Projection) ([]byte, error) {
	snap := p.Board.Snapshot()
	links := p.ExtraLinks
	if links == nil {
		links = []string{}
	}
	roots := p.Roots
	if roots == nil {
		roots = []string{}
	}
	return json.Marshal(Blob{
		V: Version, Slug: p.Slug, Base: p.Base, Count: p.Count, LastTS: p.LastTS,
		State: p.State, SupersededBy: p.SupersededBy, ExtraLinks: links, Roots: roots,
		HasMeta: p.HasMeta, Meta: p.Meta, Keys: snap.Keys, Contest: snap.Contests,
	})
}

// decode turns blob bytes back into a Projection. Origin is the caller's to
// set: decode does not know whether the base was the head.
func decode(raw []byte) (Projection, error) {
	var b Blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return Projection{}, err
	}
	if b.V != Version {
		return Projection{}, fmt.Errorf("%w: schema v%d", ErrNoCache, b.V)
	}
	if len(b.Base) != 40 && len(b.Base) != 64 {
		return Projection{}, fmt.Errorf("%w: base %q is not a full sha", ErrNoCache, b.Base)
	}
	bd := board.Restore(b.Meta, board.Snapshot{Keys: b.Keys, Contests: b.Contest})
	return Projection{
		Slug: b.Slug, Meta: b.Meta, HasMeta: b.HasMeta, Board: bd, State: b.State,
		SupersededBy: b.SupersededBy, ExtraLinks: b.ExtraLinks, Roots: b.Roots,
		LastTS: b.LastTS, Base: b.Base, Count: b.Count,
	}, nil
}

// LoadRaw reads the cache ref's blob bytes, unvalidated.
func (s Source) LoadRaw() ([]byte, string, error) { return loadRawBlob(s.Git, s.CacheRef) }

// loadRawBlob is LoadRaw's mechanism, generalized over the ref: dgd-265's
// index blob lives under a second ref (refs/ledger-cache-index/<slug>) and
// reads it exactly the same way - a cache ref is a cache ref regardless of
// which schema its blob carries.
func loadRawBlob(g Git, ref string) ([]byte, string, error) {
	sha, ok := g.RevParse(ref)
	if !ok {
		return nil, "", ErrNoCache
	}
	objs, err := g.Batch([]string{sha})
	if err != nil {
		return nil, sha, err
	}
	if len(objs) != 1 || !objs[0].Present {
		return nil, sha, &UnreadableCacheError{Sha: sha,
			Reason: fmt.Sprintf("%s is missing from this store", sha)}
	}
	if objs[0].Type != "blob" {
		return nil, sha, &UnreadableCacheError{Sha: sha,
			Reason: fmt.Sprintf("%s is a %s, not a blob", sha, objs[0].Type)}
	}
	return []byte(objs[0].Content), sha, nil
}

// Read is the three-branch read path.
//
//  1. The blob's base IS the ledger head: answer from the blob alone.
//  2. The blob's base is reached by a bounded single-parent walk back from
//     head: fold only the commits in between onto the cached board.
//  3. Anything else - no ref, wrong version, a base this history has never
//     heard of (a pushed cache from a foreign store), a base AHEAD of head
//     (a peer's cache arriving before its commits), a merge inside the
//     range, or a range longer than WalkBound: ignore the cache entirely
//     and fold from root.
//
// Branch 3 is not a fallback bolted on for safety; it is what makes the
// whole scheme sound. The walk is the proof, not the ref's presence: a
// pushed blob is trusted only insofar as this history can demonstrate it
// describes its own prefix.
func (s Source) Read() (Projection, error) {
	if p, ok := s.TryRead(); ok {
		return p, nil
	}
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return s.Fold(s.LedgerRef) // unknown ledger: let the folder say so
	}
	return s.Fold(head)
}

// TryRead is Read's cache-only half: every branch Read falls back to
// s.Fold(...) on returns ok=false here instead of paying for a root fold.
// dgd-265 needs this split because a verb reading TWO cache refs (this
// blob plus the sibling index) must be able to ask "would each of you hit,
// on its own, without folding" before committing to either answer - see
// store.CachedRead. Read itself is exactly this plus the one fallback
// every failure here shares.
func (s Source) TryRead() (Projection, bool) {
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return Projection{}, false
	}
	raw, _, err := s.LoadRaw()
	if err != nil {
		return Projection{}, false
	}
	p, err := decode(raw)
	if err != nil || p.Slug != s.Slug || !p.HasMeta {
		return Projection{}, false
	}
	if p.Base == head {
		p.Origin = OriginCache
		return p, true
	}
	tailSHAs, ok := walkChain(s.Git, head, p.Base, WalkBound)
	if !ok {
		return Projection{}, false
	}
	tail, err := fetchTailEvents(s.Git, tailSHAs)
	if err != nil {
		return Projection{}, false
	}
	evs := tailEventList(tail)
	p.Board.ApplyTail(evs, p.Count)
	p.Board.AdvanceContests(evs)
	advanceState(&p, evs)
	p.Count += len(evs)
	if ts := lastTS(evs); ts != "" {
		p.LastTS = ts
	}
	p.Base = head
	p.Origin = OriginTail
	return p, true
}

// advanceState folds the tail's non-`set` events into the three scalars
// fold.Fold derives, using fold.Fold's own rules: the first close in total
// order wins, the first superseded_by link wins the redirect and later ones
// are extra links. The cached prefix precedes the tail in that same total
// order, so "first" means "already in the blob, if there is one".
func advanceState(p *Projection, evs []model.Event) {
	for _, ev := range evs {
		if ev.Type != "lifecycle" {
			continue
		}
		switch ev.LifecycleKind {
		case "close":
			if p.State == "open" {
				p.State = "closed:" + ev.Reason
			}
		case "superseded_by":
			if p.SupersededBy == "" {
				p.SupersededBy = ev.Successor
			} else {
				p.ExtraLinks = append(p.ExtraLinks, ev.Successor)
			}
		}
	}
}

// walk proves the attachment condition and returns the tail in chain order,
// oldest first, base excluded. It walks parents through the persistent
// cat-file child rather than spawning `git log`: store.eventsDAG's own
// comment records that a parent walk loses badly to one `git log` on a long
// linear chain (3.2x slower at 1,000 commits), but the same measurement has
// it 0.2x - faster - at 100, and this walk is bounded at WalkBound with no
// spawn at all.
//
// A commit with anything other than exactly one parent ends the walk
// unsuccessfully. That covers both refusals at once: a merge (whose second
// parent could attach to the cached prefix's interior, which is precisely
// what would let dag.Sort interleave new commits into the cached order) and
// a root reached without ever meeting base (a foreign or rewritten base).
func (s Source) walk(head, base string, bound int) ([]string, bool) {
	return walkChain(s.Git, head, base, bound)
}

// walkChain is walk's mechanism, freed of Source so IndexSource's own
// TryRead can prove the identical attachment condition against its own
// blob's base without a second copy of the walk.
func walkChain(g Git, head, base string, bound int) ([]string, bool) {
	cur := head
	chain := make([]string, 0, bound)
	for i := 0; i < bound; i++ {
		if cur == base {
			for l, r := 0, len(chain)-1; l < r; l, r = l+1, r-1 {
				chain[l], chain[r] = chain[r], chain[l]
			}
			return chain, true
		}
		parents, ok := parentsOfCommit(g, cur)
		if !ok || len(parents) != 1 {
			return nil, false
		}
		chain = append(chain, cur)
		cur = parents[0]
	}
	return nil, false
}

// parentsOfCommit reads one commit object off the batch reader and returns
// its parent shas. Commit headers end at the first blank line; parents are
// the "parent <sha>" lines among them, in order.
func parentsOfCommit(g Git, sha string) ([]string, bool) {
	objs, err := g.Batch([]string{sha})
	if err != nil || len(objs) != 1 || !objs[0].Present || objs[0].Type != "commit" {
		return nil, false
	}
	var parents []string
	for _, line := range strings.Split(objs[0].Content, "\n") {
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "parent "); ok {
			parents = append(parents, strings.TrimSpace(rest))
		}
	}
	return parents, true
}

// tailEvent pairs a decoded, sentinel-filtered event with the git blob sha
// its event.json content came from. The projection's tail application only
// ever needed the event; the index's Events record needs the blob sha too,
// so the batch read that fetches one fetches both rather than costing a
// second pass over the same commits.
type tailEvent struct {
	Event   model.Event
	BlobSha string
}

// tailEvents reads the tail's event.json blobs in ONE batch and applies
// store.eventsDAG's sentinel rules verbatim: a commit whose event.json is
// missing or unparseable, or whose event type is "sync", is contracted out
// of the fold - never dropped from the chain, never delivered as an event.
// The id truncation matches too (first 10 chars of the commit sha), because
// ids are what every contest and rename record in the cached prefix already
// holds.
func (s Source) tailEvents(shas []string) ([]model.Event, error) {
	tail, err := fetchTailEvents(s.Git, shas)
	if err != nil {
		return nil, err
	}
	return tailEventList(tail), nil
}

func tailEventList(tail []tailEvent) []model.Event {
	evs := make([]model.Event, len(tail))
	for i, te := range tail {
		evs[i] = te.Event
	}
	return evs
}

// fetchTailEvents is tailEvents' mechanism, freed of Source: the OID a
// "sha:event.json" cat-file request resolves to IS the event.json blob's
// own sha, at no extra cost - git cat-file --batch's header line names the
// resolved object, not the commit fed in. Capturing it here is what lets
// dgd-265's index carry a blob_sha per event without a second batch pass.
func fetchTailEvents(g Git, shas []string) ([]tailEvent, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	reqs := make([]string, len(shas))
	for i, sha := range shas {
		reqs[i] = sha + ":event.json"
	}
	objs, err := g.Batch(reqs)
	if err != nil {
		return nil, err
	}
	if len(objs) != len(reqs) {
		return nil, fmt.Errorf("cat-file returned %d of %d objects", len(objs), len(reqs))
	}
	tail := make([]tailEvent, 0, len(shas))
	for i, sha := range shas {
		if !objs[i].Present {
			continue
		}
		var ev model.Event
		if json.Unmarshal([]byte(objs[i].Content), &ev) != nil {
			continue
		}
		if ev.Type == "sync" {
			continue
		}
		ev.ID = sha[:10]
		tail = append(tail, tailEvent{Event: ev, BlobSha: objs[i].OID})
	}
	return tail, nil
}

// Write force-updates the cache ref to a fresh blob of p. Force is the
// whole point: the ref is a cache, so its history is meaningless and every
// update is a replacement.
func (s Source) Write(p Projection) error {
	if !p.HasMeta {
		// A ledger whose chain carries no meta.json at all cannot be cached
		// soundly: folded reads resolve meta from the oldest commit that has
		// one, so a later commit could change the answer under a cached
		// prefix. Such a ledger is not ready-capable either, so nothing is
		// lost by declining.
		return nil
	}
	raw, err := Encode(p)
	if err != nil {
		return err
	}
	sha, err := s.Git.WriteObject("blob", raw)
	if err != nil {
		return err
	}
	return s.Git.UpdateRefForce(s.CacheRef, sha)
}

// MaybeRefresh is the write path: after an ordinary append, refresh the
// cache ref only once the tail since its base has reached CacheEvery
// commits. Batched, never per-write - see CacheEvery for the arithmetic.
//
// Every error here is swallowed by the caller: a cache that cannot be
// written must not fail a write that already landed.
func (s Source) MaybeRefresh() error {
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return nil
	}
	raw, _, err := s.LoadRaw()
	if err != nil {
		return s.refreshFromRoot(head) // no cache yet: mint one
	}
	p, derr := decode(raw)
	if derr != nil || p.Slug != s.Slug || !p.HasMeta {
		return s.refreshFromRoot(head)
	}
	if p.Base == head {
		return nil
	}
	// The cadence gate IS the walk, bounded at the cadence itself: reaching
	// base inside CacheEvery steps means the tail is still short enough to
	// fold on read, so there is nothing to do. Not reaching it means the
	// tail has grown past the cadence, which is exactly the refresh trigger.
	if _, reached := s.walk(head, p.Base, CacheEvery); reached {
		return nil
	}
	refreshed, err := s.Read()
	if err != nil {
		return err
	}
	return s.Write(refreshed)
}

func (s Source) refreshFromRoot(head string) error {
	p, err := s.Fold(head)
	if err != nil {
		return err
	}
	return s.Write(p)
}

// Reset drops the ref and rebuilds it eagerly from root - `chit cache
// reset <slug>`.
func (s Source) Reset() (Projection, error) {
	if err := s.Git.DeleteRef(s.CacheRef); err != nil {
		return Projection{}, err
	}
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return Projection{}, fmt.Errorf("unknown_ledger: %s", s.Slug)
	}
	p, err := s.Fold(head)
	if err != nil {
		return Projection{}, err
	}
	return p, s.Write(p)
}

// Diff is one difference `cache verify` found.
type Diff struct {
	What   string `json:"what"`
	Detail string `json:"detail"`
}

// Verify is D76's differential test as a runnable command: re-fold from
// root and byte-compare, twice over, because the two comparisons catch
// different failures.
//
//   - The STORED blob against a from-root fold of the blob's own base. This
//     is what catches a corrupted or hand-written ref: the blob claims to be
//     the fold of that sha, and either it is byte-identical to one or it is
//     wrong.
//   - The cached READ at head against a from-root fold at head. This is what
//     catches the resume itself - a tail fold that produces a different
//     board from the fold it is supposed to reproduce.
//
// The exit contract this feeds, stated once here because it is what makes
// the verb usable by something that is not a human reading JSON: verify
// reports no differences if and only if the ref is ABSENT, or the ref is a
// cache blob that a fold from root reproduces byte for byte. Every other
// shape - readable but wrong, undecodable, a commit sha, a slug that names
// another ledger - is a difference. A missing ref is the one exemption:
// nothing is claimed, so nothing can be wrong, and `cache verify` must stay
// runnable unconditionally on a store that has never written a cache.
func (s Source) Verify() ([]Diff, error) {
	head, ok := s.Git.RevParse(s.LedgerRef)
	if !ok {
		return nil, fmt.Errorf("unknown_ledger: %s", s.Slug)
	}
	var diffs []Diff
	raw, sha, err := s.LoadRaw()
	if err != nil {
		// Order matters: UnreadableCacheError unwraps to ErrNoCache, so
		// the narrower case is tested first. A ref that exists and is not
		// a cache blob is a difference, not an absence - see that type's
		// own comment for why the two part here and nowhere else.
		var ue *UnreadableCacheError
		if errors.As(err, &ue) {
			return []Diff{{What: "cache_unreadable",
				Detail: fmt.Sprintf("%s points at an object that is not a cache blob: %s", s.CacheRef, ue.Reason)}}, nil
		}
		if errors.Is(err, ErrNoCache) {
			return nil, ErrNoCache
		}
		return []Diff{{What: "blob_unreadable", Detail: err.Error()}}, nil
	}
	stored, derr := decode(raw)
	if derr != nil {
		return []Diff{{What: "blob_undecodable", Detail: fmt.Sprintf("%s: %s", sha, derr)}}, nil
	}
	if stored.Slug != s.Slug {
		diffs = append(diffs, Diff{What: "slug_mismatch",
			Detail: fmt.Sprintf("blob names slug %q, ref is %s", stored.Slug, s.CacheRef)})
	}
	atBase, berr := s.Fold(stored.Base)
	if berr != nil {
		diffs = append(diffs, Diff{What: "base_not_on_ledger",
			Detail: fmt.Sprintf("base %s does not fold on this store: %s", stored.Base, berr)})
	} else {
		atBase.Slug = s.Slug
		want, eerr := Encode(atBase)
		if eerr != nil {
			return nil, eerr
		}
		if string(want) != string(raw) {
			diffs = append(diffs, Diff{What: "stored_blob_differs",
				Detail: firstByteDiff(raw, want, fmt.Sprintf("stored blob at base %s", stored.Base))})
		}
	}
	cached, cerr := s.Read()
	if cerr != nil {
		return nil, cerr
	}
	fromRoot, rerr := s.Fold(head)
	if rerr != nil {
		return nil, rerr
	}
	gotBytes, e1 := Encode(cached)
	wantBytes, e2 := Encode(fromRoot)
	if e1 != nil || e2 != nil {
		return nil, errors.Join(e1, e2)
	}
	if string(gotBytes) != string(wantBytes) {
		diffs = append(diffs, Diff{What: "cached_read_differs",
			Detail: firstByteDiff(gotBytes, wantBytes, fmt.Sprintf("cached read at head %s (origin %s)", head, cached.Origin))})
	}
	return diffs, nil
}

// firstByteDiff names the difference rather than dumping two blobs: the
// offset, and a short window around it from each side. `verify` has to be
// readable in a terminal to be used.
func firstByteDiff(got, want []byte, what string) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	i := 0
	for i < n && got[i] == want[i] {
		i++
	}
	return fmt.Sprintf("%s: first difference at byte %d of %d/%d\n  cached: %s\n  folded: %s",
		what, i, len(got), len(want), window(got, i), window(want, i))
}

func window(b []byte, at int) string {
	lo := at - 40
	if lo < 0 {
		lo = 0
	}
	hi := at + 40
	if hi > len(b) {
		hi = len(b)
	}
	return fmt.Sprintf("%q", string(b[lo:hi]))
}
