# chit admin runbook

This is the material `chit init` points at but doesn't print in full: the
hazards that only matter to whoever administers a shared remote, not to an
agent doing day-to-day reads and writes. Agent-facing doctrine lives in
`chit quickstart`; this file is for humans setting up or recovering a
shared store. Bulk-seeding a board from an existing GitHub backlog is its
own recipe: `ledger/docs/migrate-github.md`.

## What can destroy the remote copy

Ledger refs (`refs/ledger/*`) are ordinary git refs living alongside your
branches. Two operations can wipe them on the remote:

- **`git push --mirror`** from any clone that never fetched the ledger
  refspec. A mirror push makes the remote's ref set match the pusher's
  exactly — refs the pusher never fetched are refs the remote loses.
- **Force-pushes** to `refs/ledger/*`, whether accidental (a script that
  force-pushes everything) or a rebase tool that doesn't know these refs
  are append-only history, never branches to be rewritten.

Neither is chit-specific; they're generic git hazards that happen to be
sharper here because these refs aren't visible in `git branch` and nobody's
watching them the way they watch `main`.

**Mitigation**: on any remote you control, set `receive.denyNonFastForwards`
(or the newer `--force-with-lease`-only workflow) and consider
`receive.denyDeletes` — see the tradeoff below before turning that on.

## `refs/ledger-cache/*`: derived, force-updated, safe to lose

Alongside each ledger ref sits `refs/ledger-cache/<slug>`: a single blob
holding the folded board projection plus the ledger sha it was folded from.
`chit ready` and `chit watch` read it instead of folding the whole chain.

Three facts an admin needs:

- It is **derived**. It holds no event that `refs/ledger/<slug>` does not
  hold. Deleting it costs nothing but speed, and `chit cache reset <slug>`
  rebuilds it. `chit cache verify <slug>` re-folds from root and byte-
  compares. It exits 0 in exactly two cases: there is no cache ref, or the
  ref is a cache blob a fold from root reproduces byte for byte. Every
  other shape exits 5 and names what differs - readable but wrong,
  undecodable, or a ref pointing at something that is not a cache blob at
  all (a commit sha, say). Absence is the only clean non-cache: a store
  that has never written one must not look corrupt, or the check could not
  be run unconditionally.
- It is **force-updated** on every refresh, but `chit push` never pushes it
  (nor `refs/ledger-cache-index/*`). The cache is local only: a fresh clone
  or `chit sync` never fetches either namespace, so there is nothing to
  scope `receive.denyNonFastForwards` around here.
- It is **never trusted blind, but only against linkage, not content**. A
  reader that cannot prove the blob's base attaches to the local chain -
  because the base sha is foreign, ahead of the local tip, or separated
  from it by a merge, among other shapes - ignores the blob entirely and
  folds from root. That bounded walk on `base..head` proves the blob's
  base sha is reachable from head; it proves nothing about whether the
  blob's `keys` actually reflect that history. A blob whose `base` is the
  real head but whose contents are forged passes every validation and is
  read as-is. Corruption shapes that do degrade safely this way: absent,
  undecodable, bad schema version, foreign base, base-ahead-of-head,
  merge-in-range, tail-past-bound, and a ref pointing at something that is
  not a cache blob at all. Forging the blob itself is not one of them, and
  now requires local write access to the store's refs - the cache is
  never pushed, so that access means owning the machine, not merely
  having push rights to a shared remote.

## `receive.denyDeletes`: the tradeoff

`receive.denyDeletes` on the remote blocks any push that deletes a ref,
which stops an accidental `git push --mirror` or `git push -d` from wiping
ledger history. It also blocks the one legitimate reason to delete a
ledger ref: secrets remediation (below), which needs a delete-and-replace
push against the remote to actually remove the secret from the shared
copy. Decide per remote: `denyDeletes` is the safer default; a remote that
expects to need secrets remediation should leave it off, or the admin
needs push access to toggle it off temporarily when an incident happens.

## Secrets incident runbook

Ledger events are immutable and, once pushed, fetched into every clone's
object database. There is no "delete the bad commit" — once a secret has
been written and synced, treat it as compromised. Steps, in order:

1. **Rotate the credential first.** This is the only true fix — nothing
   below undoes exposure, it only cleans up copies of a secret that no
   longer works.
2. **On every clone that has fetched**, before its next sync: delete the
   local ref that carries the secret —
   `git update-ref -d refs/ledger/<slug>`. Do this before the next sync,
   not after: a surviving local ref would re-plant the secret into the
   remote (and every other clone) the moment that clone syncs again.
3. **Push the deletion to the remote** to remove the shared copy. This is
   the operation `receive.denyDeletes` blocks — if the remote has it set,
   an admin needs to disable it temporarily to land this push, then
   decide whether to re-enable it.

None of this scrubs the object database of clones that already have the
blob — git's reflog and unreferenced objects can keep it around locally
for a while (`git gc --prune=now` is the closest thing to a purge, and even
that isn't a guarantee). Rotation is what actually neutralizes the
exposure; ref surgery just stops the ledger from continuing to hand it to
future readers.
