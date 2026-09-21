package cmd

import (
	"fmt"
	"sort"

	"ledger/internal/cache"
	"ledger/internal/out"
)

// LoadProjection is Load's fold-cache counterpart: the same answer, read
// through refs/ledger-cache/<slug> plus a bounded tail walk instead of a
// whole-chain fold. It returns a cache.Projection rather than a
// *fold.Ledger because it deliberately does NOT carry the event list - the
// projection is the board, and only the verbs that need nothing else can
// use it.
//
// Only `ready` and `watch`'s ledger pick use this. `status`, `show`, `ls`,
// `where`, `rollup` and `notes` still go through Load and still pay for the
// whole chain: they render events, which this cache refuses to carry.
func (c *Ctx) LoadProjection(slug string) (cache.Projection, error) {
	p, err := c.Store.Projection(slug)
	if err != nil {
		return cache.Projection{}, out.Errf("unknown_ledger", c.shadowHint("chit ls --all  (lists every ledger here)"),
			4, "no ledger '%s' here", slug)
	}
	return p, nil
}

// PickProjection is PickLedger over projections: same resolution order,
// same three outcomes, same error codes and hints. Kept as a parallel
// function rather than a rewrite of PickLedger, because every other verb
// still needs the folded event list PickLedger returns and sharing one
// implementation would mean folding the chain for both.
func (c *Ctx) PickProjection(ledgerFlag string) (cache.Projection, error) {
	if ledgerFlag != "" {
		return c.LoadProjection(ledgerFlag)
	}
	slugs, err := c.Store.Slugs()
	if err != nil {
		return cache.Projection{}, out.Errf("git_failed", "", 1, "%s", err)
	}
	var all, opens []cache.Projection
	for _, s := range slugs {
		p, err := c.Store.Projection(s)
		if err != nil {
			continue
		}
		all = append(all, p)
		if p.State == "open" {
			opens = append(opens, p)
		}
	}
	switch len(opens) {
	case 1:
		return opens[0], nil
	case 0:
		if len(all) == 1 {
			return all[0], nil
		}
		hint := "chit create <slug> --scope <what-it-tracks>  starts one; chit ls --all lists closed ones"
		if len(all) > 1 {
			hint += "; --ledger <slug> targets a closed one directly (notes and rollups are still allowed there)"
		}
		return cache.Projection{}, out.Errf("no_open_ledger", c.shadowHint(hint), 4, "no open ledgers in this repo")
	}
	sort.Slice(opens, func(i, j int) bool { return opens[i].LastTS > opens[j].LastTS })
	list := ""
	for i, p := range opens {
		if i > 0 {
			list += "; "
		}
		list += fmt.Sprintf("%s (%s, last write %s)", p.Slug, p.Meta.Scope, out.Age(p.LastTS))
	}
	return cache.Projection{}, out.Errf("ambiguous_ledger", "add --ledger <slug>. Open: "+list, 4,
		"%d ledgers are open - say which one", len(opens))
}

// LoadIndex is LoadProjection's counterpart over dgd-265's spine-and-events
// index: the same answer, read through refs/ledger-cache-index/<slug> plus
// a bounded tail walk instead of a whole-chain fold, falling back to a
// root fold on the same terms Store.Index does.
//
// Nothing calls this for its own single-ref fallback today - status, show
// and notes want the CHEAPER dual check (CacheSource/CacheIndexSource's
// TryRead, paired in cachedRead) so a miss on either ref does not cost a
// second root fold. It exists as LoadProjection's named mirror per spec,
// and for any future caller content with Index alone.
func (c *Ctx) LoadIndex(slug string) (cache.Index, error) {
	ix, err := c.Store.Index(slug)
	if err != nil {
		return cache.Index{}, out.Errf("unknown_ledger", c.shadowHint("chit ls --all  (lists every ledger here)"),
			4, "no ledger '%s' here", slug)
	}
	return ix, nil
}

// PickIndex is PickProjection's counterpart over the index cache. Index
// alone carries no State (dgd-265's schema deliberately doesn't - status is
// a projection field), so the ambient "which ledger" decision cannot be
// made from an index by itself; PickIndex resolves it exactly the way
// PickProjection does (same order, same three outcomes, same error codes
// and hints - literally PickProjection's own resolution) and then loads
// that slug's index, so the two mirrors can never drift on what "ambiguous"
// or "no open ledger" means.
func (c *Ctx) PickIndex(ledgerFlag string) (cache.Index, error) {
	p, err := c.PickProjection(ledgerFlag)
	if err != nil {
		return cache.Index{}, err
	}
	return c.LoadIndex(p.Slug)
}

// cachedRead is status/show/notes' fast-path check: the ledger, resolved
// exactly as PickProjection (and, since they share resolution rules,
// PickLedger) would resolve it, plus a cheap yes/no on whether BOTH cache
// refs describe that ledger's HEAD right now.
//
// err is the resolution error alone (unknown/ambiguous/no-open-ledger) -
// identical to what PickLedger would return for the same flag, since
// PickProjection's own doc comment guarantees the two never drift. p is
// always meaningful when err is nil, regardless of ok: PickProjection's
// Store.Projection call already had to settle for a root fold or it
// wouldn't have an answer at all, and p.Slug is exactly the ledger a
// caller's own fallback fold should target - never re-run the ambient
// "which ledger is open" search a second time.
//
// ok is true only when p's own read did NOT need a root fold (p.Origin !=
// OriginRoot - cheap to check, already computed) AND the index ref's cheap
// TryRead lands on that exact same base. Either ref alone being stale, or
// the two resolving to different bases (the two-refs-can-disagree gap the
// design gate asked to close), falls through to ok=false: a verb must
// never mix a cache answer from one head with an index answer from
// another.
func (c *Ctx) cachedRead(ledgerFlag string) (cache.Projection, cache.Index, bool, error) {
	p, err := c.PickProjection(ledgerFlag)
	if err != nil {
		return cache.Projection{}, cache.Index{}, false, err
	}
	if p.Origin == cache.OriginRoot {
		return p, cache.Index{}, false, nil
	}
	ix, ok := c.Store.CacheIndexSource(p.Slug).TryRead()
	if !ok || ix.Base != p.Base {
		return p, cache.Index{}, false, nil
	}
	return p, ix, true, nil
}
