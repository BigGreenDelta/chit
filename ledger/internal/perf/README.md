# chain-scale perf probe

Every read chit performs is linear in chain length. So every performance number
anyone quotes about a ledger is only true at the size it was measured, and the
ones in commit messages and lockfiles go stale silently as the chain grows.

This probe is how those numbers stay honest. Run it occasionally, paste the
output where the claim lives, and note the date and the machine.

It is skipped unless `CHIT_PERF` is set. A full default run seeds and folds
1,250,000 events and takes tens of minutes.

## Run it on Linux

Windows costs roughly an extra 170 ms of process start on EVERY call, and this
probe makes hundreds of them, so a Windows run measures the host more than it
measures chit. Use the same pinned Go image the build uses.

```sh
# from the repo root
podman run --rm \
  -v "$PWD/ledger:/src:ro" -v "$PWD/.perf:/out" -w /src \
  docker.io/library/golang:1.26 \
  sh -c 'go build -o /out/chit . &&
         CHIT_PERF=1 CHIT_PERF_BIN=/out/chit \
         go test ./internal/perf/ -run TestChainScale -v -count=1 -timeout 180m'
```

From PowerShell, same thing with `${PWD}` and the paths quoted.

## Knobs

| variable | meaning |
|---|---|
| `CHIT_PERF` | required; unset means every test here skips |
| `CHIT_PERF_BIN` | required; path to a built chit binary |
| `CHIT_PERF_SIZES` | comma-separated chain lengths. Default `12500,25000,50000,200000,1000000` |

The defaults are geometric, so curvature reads at a glance: if cost is linear
in chain length then doubling the events should double the cold numbers, and a
step that does not is the finding. Two points can only ever draw a straight
line.

The low end is dense on purpose. Three 2x steps in a row - 12.5k, 25k, 50k - is
what separates the **intercept** from the **slope**: fixed cost per invocation,
which is process start plus store open, against cost that grows with the chain.
Spread the points out and the two become indistinguishable, which is how "chit
is slow" gets believed when the truth is "there are fourteen of them".

**12,500 is not a real test.** A chain that size is nothing, so whatever a verb
costs there is very nearly all fixed cost. It is the zero mark on the ruler,
not a workload anyone cares about, and every other row is read against it. The
interesting rows are 200k and 1M.

It does sit just under the real dgd ledger - 17,177 events on 2026-09-22 - and
that is worth one sentence: today's store is still in the region where chain
length barely matters, so anything that feels slow now is fixed cost, not
history.

A quick pass while iterating:

```sh
CHIT_PERF=1 CHIT_PERF_BIN=/out/chit CHIT_PERF_SIZES=25000 \
  go test ./internal/perf/ -run TestChainScale -v -count=1
```

## What it measures

For each chain length, on a freshly seeded store:

- **cold** - every verb with BOTH cache refs deleted, so it folds from root.
  Measured once: at a million events a cold call is tens of seconds and the
  spread between repeats is far smaller than the number itself.
- **warm** - the same verbs with both refs minted. Median of three.
- **`cache reset`** - the whole cold build of both blobs. This is exactly what
  a fresh clone pays once, and it is the number to weigh against any proposal
  to push the cache namespace.
- **blob sizes** - the projection blob is O(keys); the index blob is O(events)
  and grows without bound. `refreshCacheAfterWrite` rewrites the index WHOLE on
  the `CacheEvery` cadence, so this column is a write-path cost, not just disk.
- **BOARD RENDER** - 14 calls in the mix `src/dgd/view.py` actually makes: one
  `ready` (view.py:838), one `show` (1040), twelve kind-filtered `notes` sweeps
  (1052-1067, 3083-3274).

## How to read it

**The board render is the number that matters, and the per-verb ratios will
flatter you.** A warm `ls` beating a cold one by 99x is real and almost
irrelevant: the board still pays 14 process starts plus whatever each call does
that the cache does not cover. A cache cannot take the board below
14 x process-start. Getting under that is a different change - the view not
shelling out fourteen times - not a better cache.

**Watch the two blobs separately.** `ready` and `ls` read the projection, which
stays flat because it is sized by key count. `status`, `show` and `notes` read
the index, which grows with history. A run where warm reads stay flat while the
index doubles is not "no change" - it is the write path getting more expensive
while the read path hides it.

**Numbers from 2026-09-22, Windows workhorse, chit at `eeb878d`.** Recorded
knowing the Windows caveat above; replace them with a Linux run.

| events | cold `ready` | `cache reset` | warm `ready` | projection | index |
|---|---|---|---|---|---|
| 25,000 | 952 ms | 1.8 s | 124 ms | 121 KB | 3.8 MB |
| 50,000 | 1,694 ms | 3.4 s | 122 ms | 121 KB | 7.5 MB |
| 100,000 | 3,273 ms | 6.8 s | 124 ms | 121 KB | 14.9 MB |
| 200,000 | 6,519 ms | 13.2 s | 115 ms | 122 KB | 29.8 MB |

Dead linear, no bend. Warm reads flat at every size. The index at 200,000
events is 29.8 MB and is rewritten in full every `CacheEvery` writes.

The projection blob is flat here because `scaletest.Churn` reuses a fixed key
set. A real ledger adds keys, so expect that column to grow slowly - the real
dgd store measured 330,670 bytes at 17,177 events across 1,136 keys.
