package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"ledger/internal/cache"
	"ledger/internal/out"
	"ledger/internal/store"
)

func init() { register(newCacheCmd) }

func newCacheCmd(c *Ctx) *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "manage a ledger's fold cache (refs/ledger-cache/<slug>)",
		Long: "The fold cache is a derived blob at refs/ledger-cache/<slug>, force-updated,\n" +
			"holding the folded board projection plus the ledger sha it was folded from.\n" +
			"`ready` and `watch` read through it; every other verb still folds the chain.\n" +
			"It is local only: chit never pushes or fetches this ref.\n" +
			"\n" +
			"It is never authoritative: a reader that cannot prove the blob's base attaches\n" +
			"to the local chain ignores it and folds from root. That proves linkage, not\n" +
			"content - a blob whose base is the real head but whose contents are forged\n" +
			"passes every check and is read as-is. Forging one now requires local write\n" +
			"access to the store's refs, since the cache is never pushed. These verbs\n" +
			"exist to rebuild it and to prove the linkage property holds."}
	reset := &cobra.Command{Use: "reset <slug>", Short: "drop the cache ref and rebuild it from root",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return runCacheReset(c, args[0]) }}
	verify := &cobra.Command{Use: "verify <slug>", Short: "re-fold from root and byte-compare against the cache",
		Long: "Re-folds the chain from root and byte-compares, twice over:\n" +
			"  - the STORED blob against a from-root fold of the blob's own base, which is\n" +
			"    what catches a corrupted, stale or hand-written ref;\n" +
			"  - the CACHED READ at head against a from-root fold at head, which is what\n" +
			"    catches the tail resume producing a different board from the fold it is\n" +
			"    supposed to reproduce.\n" +
			"\n" +
			"Exit 0 means one of exactly two things: there is no cache ref, or the ref\n" +
			"is a cache blob a fold from root reproduces byte for byte. Anything else\n" +
			"exits 5 and names what differs - a blob that is readable but wrong, one\n" +
			"this binary cannot decode, or a ref pointing at something that is not a\n" +
			"cache blob at all. This is a differential test meant to be run by a\n" +
			"script, so it answers through its exit status and not only through JSON.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return runCacheVerify(c, args[0]) }}
	cmd.AddCommand(reset, verify)
	return cmd
}

func runCacheReset(c *Ctx, slug string) error {
	if _, ok := c.Store.FullHead(slug); !ok {
		return out.Errf("unknown_ledger", c.shadowHint("chit ls --all  (lists every ledger here)"),
			4, "no ledger '%s' here", slug)
	}
	p, err := c.Store.CacheSource(slug).Reset()
	if err != nil {
		return out.Errf("git_failed", "", 1, "%s", err)
	}
	// dgd-265: the index ref rebuilds alongside the projection's, on the
	// same `cache reset` call - a reset that left one of the two refs stale
	// would make `cache verify` immediately report a difference against the
	// reset it just ran.
	ix, err := c.Store.CacheIndexSource(slug).Reset()
	if err != nil {
		return out.Errf("git_failed", "", 1, "%s", err)
	}
	sha, _ := c.Store.RevParse(store.CacheRef(slug))
	indexSha, _ := c.Store.RevParse(store.CacheIndexRef(slug))
	payload := map[string]any{"ledger": slug, "ref": store.CacheRef(slug), "blob": sha,
		"base": p.Base, "events": p.Count, "keys": len(p.Board.Keys),
		"index_ref": store.CacheIndexRef(slug), "index_blob": indexSha, "index_events": len(ix.Events)}
	outEmit(c, payload, []string{fmt.Sprintf("%s  cache rebuilt from root: %d events, %d keys, base %s (index: %d event records)",
		slug, p.Count, len(p.Board.Keys), short(p.Base), len(ix.Events))})
	return nil
}

func runCacheVerify(c *Ctx, slug string) error {
	if _, ok := c.Store.FullHead(slug); !ok {
		return out.Errf("unknown_ledger", c.shadowHint("chit ls --all  (lists every ledger here)"),
			4, "no ledger '%s' here", slug)
	}
	// dgd-265: verify byte-compares BOTH refs. Absence is still the only
	// thing that exits 0 for a given ref (cache.ErrNoCache); a ref that
	// exists and is wrong, on either side, is a difference the operator is
	// told about by name, never silently outvoted by the other ref being
	// clean.
	pDiffs, perr := c.Store.CacheSource(slug).Verify()
	if perr != nil && !errors.Is(perr, cache.ErrNoCache) {
		return out.Errf("git_failed", "", 1, "%s", perr)
	}
	iDiffs, ierr := c.Store.CacheIndexSource(slug).Verify()
	if ierr != nil && !errors.Is(ierr, cache.ErrNoCache) {
		return out.Errf("git_failed", "", 1, "%s", ierr)
	}
	diffs := append(append([]cache.Diff{}, pDiffs...), iDiffs...)

	if errors.Is(perr, cache.ErrNoCache) && errors.Is(ierr, cache.ErrNoCache) {
		// Neither ref claims anything, so nothing can be wrong. Exit 0: a
		// store that has simply never written a cache must not look like a
		// corrupt one, or `cache verify` cannot be run unconditionally.
		//
		// This branch is ABSENCE only, of BOTH refs. A ref that exists and
		// points at something that is not a cache blob used to land here
		// too, and so reported "no cache ref" and exited 0 - the same answer
		// as a clean store, for a ref somebody had hand-written over. Verify
		// now returns that as a cache_unreadable difference instead (exit
		// 5): the read path is right to ignore such a ref, but ignoring it
		// is precisely the thing an operator asked this verb to tell them
		// about. See cache.UnreadableCacheError for why the two part here.
		outEmit(c, map[string]any{"ledger": slug, "ref": store.CacheRef(slug),
			"index_ref": store.CacheIndexRef(slug), "cache": nil, "differences": []any{}},
			[]string{slug + "  no cache ref - nothing to verify (chit cache reset " + slug + " builds one)"})
		return nil
	}
	if len(diffs) == 0 {
		outEmit(c, map[string]any{"ledger": slug, "ref": store.CacheRef(slug),
			"index_ref": store.CacheIndexRef(slug), "differences": []any{}},
			[]string{slug + "  cache verified: byte-identical to a fold from root (both refs)"})
		return nil
	}
	lines := []string{fmt.Sprintf("%s  cache DIFFERS from a fold from root (%d difference(s))", slug, len(diffs))}
	for _, d := range diffs {
		lines = append(lines, "  "+d.What+": "+d.Detail)
	}
	// The ok:false envelope, same shape sync/push use for partial_failure:
	// the whole document - outcomes and error contract together - is written
	// in one write, never a second error document tacked on after.
	payload := map[string]any{"ledger": slug, "ref": store.CacheRef(slug), "index_ref": store.CacheIndexRef(slug),
		"differences": diffs, "ok": false, "error": "cache_differs",
		"message": "the cache ref does not match a fold from root",
		"hint":    "chit cache reset " + slug + "  rebuilds it; see `differences` for where they part"}
	out.Emit(c.Stdout, c.TTY, payload, lines)
	// The payload above is already written, so the error carries no second
	// document - the same shape `watch --timeout` uses for "reported, then
	// exit non-zero".
	return &out.CLIError{Code: "cache_differs", ExitCode: 5}
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
