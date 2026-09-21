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
