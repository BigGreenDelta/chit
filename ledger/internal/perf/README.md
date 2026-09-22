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

## Results, 2026-09-22, Linux, chit at `eeb878d`

Run in `docker.io/library/golang:1.26` as described above. Warm is a median of
three; cold is one measurement.

| events | 25,000 | 50,000 | 200,000 | 1,000,000 |
|---|---|---|---|---|
| `cache reset` | 1.7 s | 3.5 s | 14.5 s | 1m 16.7 s |
| projection blob | 121 KB | 121 KB | 122 KB | 122 KB |
| index blob | 3.8 MB | 7.5 MB | 29.8 MB | **148.8 MB** |
| warm `ready` | 8 ms | 10 ms | 9 ms | 8 ms |
| warm `ls` | 7 ms | 8 ms | 7 ms | 8 ms |
| warm `status` | 50 ms | 114 ms | 363 ms | 1.69 s |
| warm `show` | 231 ms | 433 ms | 1.69 s | 8.75 s |
| warm `notes --kind` | 229 ms | 427 ms | 1.68 s | 8.52 s |
| warm `rollup` | 861 ms | 1.68 s | 6.91 s | 36.5 s |
| warm `tail` | 908 ms | 1.73 s | 7.28 s | 38.9 s |
| **BOARD RENDER warm** | **3.5 s** | **6.3 s** | **22.1 s** | **1m 52.5 s** |

### Four things this says

**The cache works, and the board is still unusable at 1M.** Warm `ready` is 8 ms
at every size - a 4,566x speedup over cold at a million events. Meanwhile the
board render is nearly two minutes. Those are not in tension: `ready` reads the
PROJECTION blob, which is O(keys) and flat, while twelve of the fourteen board
calls are `notes`, which read the INDEX blob, which is O(events).

**The index is the wall, and it is not linear in effect.** Events went 20x from
50k to 1M; warm `notes` went 20x too, 427 ms to 8.52 s. That is linear per call,
but the board makes twelve of them, so the render grows 20x as well. A 148 MB
blob decoded twelve times per render is the whole cost.

**`rollup` and `tail` are 1.0x at every size.** They read no cache at all - 36 s
and 39 s at a million events. Whether that matters depends on who calls them;
nothing on the board does today.

**Cold numbers stop being a curiosity at scale.** A cold board render at 1M is
18 minutes. Any change that invalidates both refs - a write, a gc, a restored
clone - puts that in front of somebody.

### Earlier Windows run, kept for the platform comparison

Same probe, same commit, on the Windows workhorse, 2026-09-22:

| events | cold `ready` | `cache reset` | warm `ready` | index |
|---|---|---|---|---|
| 25,000 | 952 ms | 1.8 s | 124 ms | 3.8 MB |
| 100,000 | 3,273 ms | 6.8 s | 124 ms | 14.9 MB |
| 200,000 | 6,519 ms | 13.2 s | 115 ms | 29.8 MB |

Warm `ready` is 115-124 ms there against 8-10 ms on Linux for identical work.
That gap is process start, and it is why this probe runs on Linux.

The projection blob is flat in all of these because `scaletest.Churn` reuses a
fixed key set. A real ledger adds keys, so expect that column to grow slowly -
the real dgd store measured 330,670 bytes at 17,177 events across 1,136 keys.
