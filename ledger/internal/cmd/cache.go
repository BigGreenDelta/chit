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
			"\n" +
			"It is never authoritative: a reader that cannot prove the blob describes its own\n" +
			"history ignores it and folds from root. Nothing here can make a read WRONG  - \n" +
			"only slower. These verbs exist to rebuild it and to prove that property holds."}
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
	sha, _ := c.Store.RevParse(store.CacheRef(slug))
	payload := map[string]any{"ledger": slug, "ref": store.CacheRef(slug), "blob": sha,
		"base": p.Base, "events": p.Count, "keys": len(p.Board.Keys)}
	outEmit(c, payload, []string{fmt.Sprintf("%s  cache rebuilt from root: %d events, %d keys, base %s",
		slug, p.Count, len(p.Board.Keys), short(p.Base))})
	return nil
}

func runCacheVerify(c *Ctx, slug string) error {
	if _, ok := c.Store.FullHead(slug); !ok {
		return out.Errf("unknown_ledger", c.shadowHint("chit ls --all  (lists every ledger here)"),
			4, "no ledger '%s' here", slug)
	}
	diffs, err := c.Store.CacheSource(slug).Verify()
	if errors.Is(err, cache.ErrNoCache) {
		// No ref claims anything, so nothing can be wrong. Exit 0: a store
		// that has simply never written a cache must not look like a
		// corrupt one, or `cache verify` cannot be run unconditionally.
		//
		// This branch is ABSENCE only. A ref that exists and points at
		// something that is not a cache blob used to land here too, and so
		// reported "no cache ref" and exited 0 - the same answer as a clean
		// store, for a ref somebody had hand-written over. Verify now
		// returns that as a cache_unreadable difference instead (exit 5):
		// the read path is right to ignore such a ref, but ignoring it is
		// precisely the thing an operator asked this verb to tell them
		// about. See cache.ErrUnreadableCache for why the two part here.
		outEmit(c, map[string]any{"ledger": slug, "ref": store.CacheRef(slug), "cache": nil,
			"differences": []any{}},
			[]string{slug + "  no cache ref - nothing to verify (chit cache reset " + slug + " builds one)"})
		return nil
	}
	if err != nil {
		return out.Errf("git_failed", "", 1, "%s", err)
	}
	if len(diffs) == 0 {
		outEmit(c, map[string]any{"ledger": slug, "ref": store.CacheRef(slug), "differences": []any{}},
			[]string{slug + "  cache verified: byte-identical to a fold from root"})
		return nil
	}
	lines := []string{fmt.Sprintf("%s  cache DIFFERS from a fold from root (%d difference(s))", slug, len(diffs))}
	for _, d := range diffs {
		lines = append(lines, "  "+d.What+": "+d.Detail)
	}
	// The ok:false envelope, same shape sync/push use for partial_failure:
	// the whole document - outcomes and error contract together - is written
	// in one write, never a second error document tacked on after.
	payload := map[string]any{"ledger": slug, "ref": store.CacheRef(slug), "differences": diffs,
		"ok": false, "error": "cache_differs",
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
