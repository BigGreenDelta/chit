package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"ledger/internal/fold"
	"ledger/internal/model"
	"ledger/internal/out"
	"ledger/internal/store"
)

// lsClosedCutoff and lsIdleAfter are the spec's 30-day/45-day windows, kept
// as package vars (rather than inlined constants) so tests can override them
// instead of needing a fake clock.
var (
	lsClosedCutoff = 30 * 24 * time.Hour
	lsIdleAfter    = 45 * 24 * time.Hour
)

func init() { register(newLsCmd) }

func newLsCmd(c *Ctx) *cobra.Command {
	var all bool
	cmd := &cobra.Command{Use: "ls", Short: "list ledgers with freshness", Args: noPositionals("show"),
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLs(c, all)
		}}
	cmd.Flags().BoolVar(&all, "all", false, "include ledgers closed more than 30 days ago")
	return cmd
}

// lsBootstrapHint is what `ls` prints in place of an empty listing when the
// committed breadcrumb (.ledger.toml) is present but no ledger refspec has
// been installed in this clone (installedRefspec, remote.go) — a fresh
// clone of a repo that uses chit, before its own `chit init && chit
// sync` has ever run here. Without this, the first `ls` in that clone reads
// as "no ledgers exist" when the truth is "nothing has been synced yet".
const lsBootstrapHint = "this repo uses chit, but it hasn't been bootstrapped in this clone — run `" + bootstrapCmd + "`"

// lsEntry is one row `ls` renders, sourced from either of two places: a
// local ledger (dgd-265's fast path, off Store.Projection - count and meta
// are exactly what a projection already carries, so ls needs nothing the
// cache doesn't have) or a tracking-only ledger (trackingOnlyLedgers, still
// a whole-chain fold of a remote's tracking ref — that ref has no cache of
// its own, and ls has no write path to mint one there). Unifying on this
// shape, rather than rendering the two sources through different code
// paths, is what lets lsLine/sort/filter stay a single implementation
// either way.
type lsEntry struct {
	Slug, Scope, State, LastTS string
	Events                     int
	Unsynced                   bool
}

func runLs(c *Ctx, all bool) error {
	slugs, err := c.Store.Slugs()
	if err != nil {
		return err
	}

	local := make(map[string]bool, len(slugs))
	entries := make([]lsEntry, 0, len(slugs))
	for _, s := range slugs {
		local[s] = true
		// dgd-265: `ls` reads through the fold cache exactly the way `ready`
		// does — everything it renders (slug, scope, state, last event
		// timestamp, event count) is already on the v1 projection blob, so
		// this needs no new cache of its own. Store.Projection degrades to a
		// whole-chain fold on any validation failure, same as before.
		p, err := c.Store.Projection(s)
		if err != nil {
			continue // torn/foreign ref: skip, never crash a listing
		}
		entries = append(entries, lsEntry{Slug: p.Slug, Scope: p.Meta.Scope, State: p.State, LastTS: p.LastTS, Events: p.Count})
	}

	unsynced := map[string]bool{}
	for _, e := range trackingOnlyLedgers(c, local) {
		entries = append(entries, e)
		unsynced[e.Slug] = true
	}

	if len(entries) == 0 {
		return emitLsEmpty(c, "no ledgers in this repo — chit create <slug> --scope <ref> starts one")
	}

	now := model.Now().UTC()
	kept := entries[:0]
	for _, e := range entries {
		if all || e.State == "open" || now.Sub(lsEventTime(e.LastTS)) <= lsClosedCutoff {
			kept = append(kept, e)
		}
	}

	sort.Slice(kept, func(i, j int) bool { return lsEventTime(kept[i].LastTS).After(lsEventTime(kept[j].LastTS)) })

	if len(kept) == 0 {
		return emitLsEmpty(c, "no ledgers match — chit ls --all also shows ledgers closed more than 30 days ago")
	}

	rows := make([]map[string]any, 0, len(kept))
	lines := make([]string, 0, len(kept))
	for _, e := range kept {
		last := lsEventTime(e.LastTS)
		idle := e.State == "open" && now.Sub(last) > lsIdleAfter
		rows = append(rows, map[string]any{
			"slug": e.Slug, "scope": e.Scope, "state": e.State,
			"last": e.LastTS, "events": e.Events, "idle": idle, "unsynced": e.Unsynced,
		})
		lines = append(lines, lsLine(e, idle, now))
	}
	payload := map[string]any{"ledgers": rows}
	outEmit(c, payload, c.noteShadowedStore(payload, lines))
	return nil
}

// emitLsEmpty is ls's shared empty-listing path — no ledgers at all, or none
// surviving the closed-cutoff filter. It swaps in lsBootstrapHint instead of
// defaultMsg when the repo's breadcrumb is committed but no ledger refspec
// is installed here yet (see lsBootstrapHint).
func emitLsEmpty(c *Ctx, defaultMsg string) error {
	msg := defaultMsg
	payload := map[string]any{"ledgers": []map[string]any{}}
	if breadcrumbExists(c.Store.Repo.Dir) && !installedRefspec(c.Store.Repo) {
		msg = lsBootstrapHint
		payload["note"] = msg
	}
	outEmit(c, payload, c.noteShadowedStore(payload, []string{msg}))
	return nil
}

// trackingOnlyLedgers folds every slug a remote's tracking ref carries that
// this clone has no local refs/ledger/<slug> for yet — exactly the set
// `chit sync` would adopt. local is the already-known set of slugs with a
// local ref. A slug tracked by more than one remote is listed once. A
// tracking ref has no fold cache of its own (nothing writes one there), so
// this stays a whole-chain fold regardless of dgd-265.
func trackingOnlyLedgers(c *Ctx, local map[string]bool) []lsEntry {
	var out []lsEntry
	seen := map[string]bool{}
	for _, remote := range trackingNamespaces(c.Store.Repo) {
		for _, slug := range trackedSlugs(c.Store.Repo, remote) {
			if local[slug] || seen[slug] {
				continue
			}
			evs, meta, _, err := c.Store.EventsDAGAt(store.TrackingRef(remote, slug))
			if err != nil {
				continue // torn/foreign tracking ref: skip, never crash a listing
			}
			seen[slug] = true
			led := fold.Fold(slug, evs, meta)
			last := ""
			if len(led.Events) > 0 {
				last = led.Events[len(led.Events)-1].TS
			}
			out = append(out, lsEntry{Slug: slug, Scope: meta.Scope, State: led.State,
				LastTS: last, Events: len(nonSyncEvents(led.Events)), Unsynced: true})
		}
	}
	return out
}

// lsEventTime is the freshness clock for sorting, the 30-day closed cutoff,
// and the 45-day idle mark alike, parsed off whichever source (a cached
// projection's LastTS, or a tracking-only fold's last raw event) an lsEntry
// came from. An empty or unparseable timestamp sorts as the zero time,
// never a crash.
func lsEventTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := model.ParseTS(ts)
	if err != nil {
		return time.Time{}
	}
	return t
}

// lsLine renders one TTY row: slug, scope (truncated so a long scope can't
// blow out the column alignment), state — with the idle marker folded in,
// e.g. "open, idle 62d" — last-write age, and the non-sync event count.
// e.Unsynced appends the tracking-only marker for a slug ls found only via
// a remote's tracking ref, with no local ref of its own yet.
func lsLine(e lsEntry, idle bool, now time.Time) string {
	state := e.State
	if idle {
		days := int(now.Sub(lsEventTime(e.LastTS)).Hours() / 24)
		state = fmt.Sprintf("open, idle %dd", days)
	}
	if e.Unsynced {
		state += " (unsynced — run chit sync)"
	}
	return fmt.Sprintf("%-20s %-44s %-20s last %-10s (%d events)",
		out.EscapeControls(e.Slug), out.EscapeControls(truncateRunes(e.Scope, 44)),
		out.EscapeControls(state), out.Age(e.LastTS), e.Events)
}
