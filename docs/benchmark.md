# Reproducible CLI and HTTP baseline

`scripts/benchmark.py` measures the real `touko` binary against new local Git
galleries. It answers a narrow question: how long do local startup, state,
original-image, find, reindex, batch-add and commit operations take at a few
known gallery sizes?

It does not measure the browser UI or GitHub. HTTP samples cover the loopback
request, response body and origin byte read only. JavaScript, layout, paint,
image decode and remote publication are outside this harness.

The script uses only the Python standard library, refuses to reuse an existing
gallery or state directory, and retains every generated fixture, gallery, state
and timing for inspection. It creates files only under the caller-supplied root;
choose a root outside the repository (or an ignored path) and never commit that
output.

## Requirements

- Linux (the HTTP and RSS workflows use Linux loopback and `/proc`).
- Go, Git and Python 3 available on `PATH`.
- Go module dependencies already cached: the build is forced offline with
  `GOPROXY=off` and `GOTOOLCHAIN=local`, matching `examples/demo.sh`.
- A fresh root directory. An empty temporary directory is the intended choice.

The child environment used for every Go, Git, CLI and server process has
`GITHUB_TOKEN`, `GH_TOKEN` and `TOUKO_HOME` removed. Every TOUKO invocation gets
an explicit `--state-dir`, so an ambient workspace cannot be selected.

## Full default run

From the repository root:

```bash
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/touko-benchmark.XXXXXXXX")
python3 scripts/benchmark.py --root "$ROOT"
```

Defaults:

| Option | Default | Meaning |
| --- | --- | --- |
| `--counts` | `0,100,1000` | Committed gallery cardinalities |
| `--repeat` | `3` | Independent samples per metric and per cardinality |
| `--batch` | `10` | Images staged and committed per mutating repetition |
| `--binary` | build | Build `touko` into `ROOT/bin/touko` |
| `--prepare-only` | off | Build fixtures and committed galleries, then stop |

`--root` must not exist or must be an empty directory. A symlink or nonempty
directory is refused before any file is created. The root is retained after
success or failure; the script never deletes it.

An existing binary can be measured instead of building one:

```bash
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/touko-benchmark.XXXXXXXX")
python3 scripts/benchmark.py --root "$ROOT" \
  --binary /absolute/path/to/touko \
  --counts 0,100,1000 --repeat 3 --batch 10
```

Generate fixtures and desktop-ready committed states without timing:

```bash
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/touko-fixtures.XXXXXXXX")
python3 scripts/benchmark.py --root "$ROOT" --counts 1000 --prepare-only
```

The machine-readable manifest is always `ROOT/benchmark.json`. On success the
script prints the manifest path, fixture paths, desktop state paths and medians.
With `--prepare-only`, stdout lists each dataset's `images/` and `batch/` paths
and each baseline state/gallery path. On failure it prints the error and the
retained root; no partial manifest is written.

## Deterministic fixtures and fresh galleries

Fixtures are 8-bit RGB PNGs generated with Python's `struct`, `zlib` and
`hashlib` only. The fixed seed is `20261018`; dimensions are `256x192`. Use the
same zlib version when comparing byte-for-byte corpus hashes; both build-time
and runtime zlib versions are recorded. Every fixture index has distinct bytes,
and the script refuses to continue if it generates duplicate image hashes.

For each cardinality `N`, the script builds, outside all timed sections:

1. `datasets/N/images/bench-000001.png` through `bench-N.png`.
2. `datasets/N/batch/` with `--batch` further distinct images, indexed after the
   baseline images.
3. `datasets/N/gallery.git`, a fresh bare local repository.
4. `datasets/N/state`, initialized, committed and verified at exactly `N`
   assets with no staged changes.
5. `datasets/N/verify-state`, an independent state recovered by `repo use` and
   `sync` from that gallery, used to check Asset identity recovery.

The script also records an aggregate SHA-256 over every fixture's name, digest
and size. Re-running with the same seed, dimensions, counts and batch size
should reproduce the same per-dataset aggregates.

Baseline construction uses one `repo init`, chunked `add` calls, one `commit`
when `N > 0`, `verify`, and a fresh-state `sync`. None of this setup, validation
or gallery copying is inside a timing sample.

## Retained layout

```text
ROOT/
  benchmark.json                     manifest and all raw samples
  bin/touko                          built CLI unless --binary was used
  datasets/N/images/                 deterministic fixture images
  datasets/N/batch/                  distinct images for mutating repetitions
  datasets/N/state/                  committed state, clean, profiling-ready
  datasets/N/gallery.git             committed baseline gallery
  datasets/N/verify-state/           independent sync/recovery state
  runs/N/rep-XX/read-state/          fresh state for read-only samples
  runs/N/rep-XX/gallery.git          independent clone for add + commit
  runs/N/rep-XX/mutate-state/        state for that clone
  runs/N/rep-XX/verifier-state/      fresh state after the mutation commit
```

`--prepare-only` creates `benchmark.json` and the `datasets/` tree, but no
`runs/` directories and no measurements. Full runs create one `runs/N/rep-XX`
directory per repetition.

## Metrics

All timing values are seconds from `time.perf_counter()`. Each metric keeps
every raw sample plus `median_seconds`, `min_seconds` and `max_seconds`.

| Key | Operation | Timed section |
| --- | --- | --- |
| `startup_state` | New `touko serve --port 0 --no-open` process | Spawn through complete `/api/state` response body |
| `http_state` | Same server process, second `/api/state` | Request through complete response body |
| `image_bytes` | `GET /api/assets/UUID/image` for a committed Asset | Request through complete origin-image body |
| `find` | Fresh CLI process: `find bench --json` | Process spawn through exit |
| `reindex` | Fresh CLI process: `reindex --json` | Process spawn through exit |
| `batch_add` | Fresh CLI process: `add` of the batch files | Process spawn through exit |
| `commit` | Fresh CLI process: `commit -m ... --json` | Process spawn through exit |

The startup sample uses the real service banner
(`TOUKO Webview: http://127.0.0.1:PORT` and `State: ...`), verifies that the
printed state is the explicit benchmark state, and uses port `0` with
`--no-open`. The process is stopped with `SIGTERM` and a bounded wait in a
`finally` block; a process needing `SIGKILL` fails the benchmark.

HTTP state samples run on the server that just completed startup, so they
measure warm state loading rather than another cold process. Image bytes are
checked after the clock stops: status, `Content-Type`, size and full SHA-256
must match the Asset. Browser loading is never inferred from these samples.

The `image_bytes` samples intentionally do not send `If-None-Match`; they measure
the first origin read. The live server separately gives each preview an ETag
derived from the content hash and `private, immutable` caching, so a repeat view
revalidates to `304` without streaming the image again. That saving is a UI
behavior, not part of this first-read number; measure it with browser/network
instrumentation rather than subtracting from this table.

At cardinality `0` there is no committed Asset to request, so `image_bytes`
has no samples and carries a `skipped` reason.

Add and commit run against a separate clone of the same `N` baseline in every
repetition. They are not cumulative: each repetition begins at `N`, stages the
same batch size, and commits once. Find and reindex use a fresh state per
repetition; reindex runs after the find sample.

## Semantic checks

The benchmark fails immediately on a subprocess, HTTP, JSON or invariant error.
Checks performed outside timed sections include:

- `/api/state` reports `initialized: true`, exactly `N` committed Assets, an
  empty change list and a nonempty HEAD.
- `find bench` returns exactly `N` committed Assets.
- `reindex` reports the baseline HEAD and `N` Assets, and leaves staging clean.
- Baseline `verify` finds `N` Assets and `N` distinct object placements; a fresh
  baseline sync recovers the same Asset identities.
- Batch `add` stages exactly `--batch` additions and does not move HEAD or change
  the staging base.
- `commit` publishes exactly `--batch` changes for `N + batch` Assets, moves
  HEAD once, and leaves the change list empty.
- An independent fresh state `sync` and `verify` after the commit recover
  `N + batch` Assets and object placements.
- Fixture image bytes are distinct; generated image bytes returned over HTTP
  match the Asset content hash exactly.

## Reading the manifest

```bash
python3 -m json.tool "$ROOT/benchmark.json"
```

Main fields:

- `parameters`: counts, repeat, batch, seed, dimensions, prepare-only flag.
- `environment`: OS, kernel, CPU model and count, memory, Go/Git/Python
  versions (including zlib), source revision and a tree dirty flag. The flag
  covers tracked and untracked changes; file names are deliberately not recorded.
- `binary`: path, size, SHA-256, whether the benchmark built it, and build time.
- `fixtures.datasets.N`: exact image/batch paths, counts, aggregate SHA-256 and
  total bytes.
- `baselines.N`: exact state, gallery and verify-state paths, HEAD and counts.
- `runs.N`: exact per-repetition paths and HEAD before/after each commit.
- `measurements.N.metrics.M`: raw `samples` and their medians for cardinality
  `N`.
- `measurements.N.checks`: observed cardinalities and invariant summaries.
- `desktop_profiling`: build, launch and profiler commands for the prepared
  states.

Observed values below come from local smoke runs, not CI performance thresholds.
Keep the retained `benchmark.json` with any comparison; the source checkout's
revision may differ from an explicitly supplied `--binary`, whose hash is recorded.

### Charts

`scripts/plot-benchmark.py` renders retained measurements as dependency-free SVG.
It reads `benchmark.json` and optional RSS JSONL files:

```bash
python3 scripts/plot-benchmark.py \
  --benchmark "$ROOT/benchmark.json" \
  --rss "0=$ROOT/profiles/0.jsonl" \
  --rss "1000=$ROOT/profiles/1000.jsonl" \
  --out-dir docs/images
```

The script writes `benchmark-cli.svg` and, when RSS inputs are supplied,
`benchmark-memory.svg`. It omits a metric with no measurement instead of
inventing a point, and the memory chart uses the per-process RSS values directly.
Keep the source JSON with any published chart.

### Observed local CLI/HTTP baseline

One machine only: Intel 20-logical-CPU host, 32 GB RAM, Linux 7.2.9, Go 1.27.1,
Git 2.56.0, Python 3.14.7, local Git backend, 3 repetitions, batch 10. Values are
medians in milliseconds from a retained `benchmark.json`; they are descriptive,
not thresholds. Warm page cache and this synthetic corpus apply.

| Assets | startup_state | http_state | image_bytes | find | reindex | batch_add | commit |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 3.5 | 0.5 | n/a | 2.7 | 9.6 | 4.0 | 91.3 |
| 100 | 4.6 | 1.4 | 7.5 | 4.4 | 189.7 | 6.1 | 371.2 |
| 1000 | 11.8 | 8.0 | 22.5 | 12.1 | 1791.6 | 16.3 | 2844.9 |

`reindex` and `commit` grow faster than the read-only paths because they walk and
rewrite repository metadata. This baseline records that shape; it does not make
those operations targets for speculative optimization.

### Remote-path comparison with injected latency

Four behaviours cannot be measured by the local CLI harness, so they were
measured with Go test fixtures on a 100-Asset gallery: an in-process Git Data
API serving a real bare repository, a real local clone, and 100 ms charged per
HTTP request or mirror call. The clone's GitHub URL was rewritten to the local
bare repository, so the production Git path ran without network. Numbers are
single-run descriptions of this machine, not thresholds.

| Path | Requests / calls | Wall time |
| --- | ---: | ---: |
| Capacity preview through GitHub API | 103 HTTP | 1.63 s |
| Capacity preview through local Git objects | 0 HTTP | 26 ms |
| Candidate metadata reads without reuse | 102 blob reads | 305 ms |
| Candidate metadata reads reusing the base | 1 blob read | 25 ms |
| R2 sync of 100 objects (4 at a time) | 300 mirror ops | 11.8 s |

The last row is not directly comparable to a serial implementation that was not
run; its serial floor is 300 × 100 ms = 30 s, so the measured wall time shows
that batches really overlap. The R2 row also made 204 API requests because a
full sync reads the metadata snapshot and every image blob not available
locally; an automatic post-commit mirror of new adds reads the staged payload
instead.

These fixtures establish request counts and proportional cost. They do not
measure real GitHub or R2 latency, rate limits, browser rendering, image
encoding, or a complete five-second publish; an isolated writable remote was
not available, so no end-to-end remote timing is claimed here.

### SHA-256 cost on 200 real images

On 2026-10-29, Go's monotonic `time.Since` measured an in-memory
`sha256.Sum256` over the same 200-image corpus, 49,518,825 input bytes per
pass. After one untimed warmup, three passes produced 600 image measurements:

| Metric | Milliseconds per image |
| --- | ---: |
| Mean | 0.111020 |
| Median | 0.023384 |
| P95 | 0.181539 |

The ignored files `benchmarks/results/hash-microbenchmark-20261029T120203Z.{json,csv,md}`
contain the individual measurements. This measures hashing cached bytes only:
it excludes disk I/O, WebP conversion, staging, GitHub and R2, and is not an
upload benchmark. It shows the scale of one SHA-256 pass on this machine, not
the exact total hashing overhead of an upload.

The attempted remote comparison is incomplete. The old Git-only run recorded
196 successful images and one failure; the new Git-only run was stopped after
193 records, including 10 failures. Its script continued after failed commits,
so later successful commits could include previously staged images. The other
samples used different datasets. These records are retained under
`benchmarks/results/`, but cannot establish the requested four-group comparison.

The old-run figures below exclude the failed attempt; they are not a
three-repeat estimate.

| Group | Result | Mean per image | Total measured time | Throughput |
| --- | --- | ---: | ---: | ---: |
| Old / GitHub only | 196 successful, 1 failed; one attempt | 8,796.2 ms | 1,724,064.4 ms | 0.012 MB/s |
| Old / GitHub + R2 | Same-corpus run incomplete | — | — | — |
| New / GitHub only | 193 records, 10 failures, then stopped; later records may contain multiple images | Invalid | Invalid | Invalid |
| New / GitHub + R2 | Same-corpus run incomplete | — | — | — |

### Single-image publication against a real gallery

The same machine, a 200-Asset public gallery, one 57 KB PNG, and Cloudflare R2
enabled. Requests are counted from the GitHub core rate-limit counter before and
after the command; wall time is `/usr/bin/time`. The network path ran through a
local HTTP proxy at roughly 0.3–0.9 s per GitHub request, so these numbers
describe that link, not GitHub's best case.

| Operation | Build | API requests | Wall |
| --- | --- | ---: | ---: |
| `add` (staging only) | any | 0 | 0.05 s |
| commit one new image, R2 on | API metadata reads | 212 | 25.8 s |
| commit one new image, R2 on | local object reads | 11 | 13.6 s |
| commit one deletion, R2 on | local object reads | 10 | 12.1 s |
| commit one new image, R2 off | local object reads | 9 | 9.3 s |
| commit one new image, R2 on | final | 5 + 5 R2 | 7.0 s |
| commit one deletion, R2 on | final | 4 + 2 R2 | 5.6 s |
| commit one new image, R2 off | final | 5 | 6.0 s |

The observed fast path plans against the staged base and uses a non-forced ref
update; one traced image commit made five API writes: image and metadata blobs,
tree, commit and ref. A deletion made three writes (tree, commit, ref), plus an
authoritative HEAD read when the R2 mirror needed to confirm the object was
unreferenced. A format upgrade, missing local snapshot, recovery, reset or sync
can require additional requests.
The current client has since restored one pre-publication HEAD read to detect
an already-rewound branch, so the historical request counts above are not the
current complete request counts.


GitHub's non-forced update rejects non-fast-forward changes, but it is not an
expected-old compare-and-swap. In particular, an external force reset to an
ancestor of the staged base can still permit a fast-forward to the candidate.
Protect the branch against external force pushes; the staged base alone is not
proof of strict compare-and-swap.

Network round trips are a likely contributor to these single observations; they
do not split time by stage. PicGo's GitHub uploader issues one
`PUT /repos/{owner}/{repo}/contents/{path}` and lets GitHub create the commit;
it does not maintain TOUKO's multi-file atomic batch and manifest.

Two network observations from the same session, neither caused by the client: one
tree read hit the 45 s HTTP timeout, and for several minutes every request body
above roughly 20 KB failed (a `400 malformed request` through the proxy, a
dropped connection without it) while the same body sent chunked succeeded. The
same uploads succeeded before and after that window, so the client was unchanged.

### Large-gallery rendering check

The gallery mounts at most 60 cards first and appends another batch when the
sentinel approaches the viewport; `content-visibility: auto` avoids laying out
cards that are offscreen. A Chromium 1440×900 run against 1010 committed Assets
observed:

- 60 mounted `.asset-card` nodes and 2779 total DOM nodes after initial load;
- clicking "显示更多" mounted 120; scrolling mounted 180, then 1010 over the run;
- 120 programmatic scroll steps: p95 frame interval 18 ms, max 20 ms, zero
  intervals over 100 ms;
- selecting a card kept the same node identity and did not rebuild the gallery;
- a `bench-0009` query debounced to 100 matches and restarted the window at 60.

The native WebKit window remained responsive while 40 synthetic PageDown presses
completed in 4.84 s. Frame timing is from Chromium, not WebKit; treat the native
result as a smoke check rather than a frame-rate benchmark.

### Observed native desktop RSS (Linux only)

Fresh local-Git state, `scripts/desktop-memory.py`, 5 snapshots per phase, the
snapshot with the highest tree RSS selected. Whole-tree values add RSS across
processes, so shared pages are counted repeatedly.

| State / phase | touko-desktop MiB | WebKitWebProcess MiB | WebKitNetworkProcess MiB | other MiB | tree MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| empty gallery, startup | 328.2 | 302.8 | 75.4 | 10.3 | 716.7 |
| 1000 assets, startup | 351.5 | 341.7 | 70.2 | 10.3 | 773.7 |
| 1000 assets, loaded | 325.5 | 341.5 | 67.6 | 10.3 | 744.9 |
| 1000 assets, browsed | 335.8 | 503.6 | 67.9 | 10.3 | 917.7 |
| 1000 assets, committed | 323.1 | 532.8 | 67.9 | 0.0 | 923.8 |

In the `committed` row the 10-image upload and commit were issued through the
desktop process's loopback API (the same Service and commit path the page uses),
then sampled; the native page was not re-rendered by that automation. No PSS/USS
or true peak was captured, and helper processes vary between snapshots, so the
`other` column is not a stable product cost.

## Native desktop memory on Linux

RSS is measured separately from the CLI/HTTP baseline. Prepare the committed
states first, then build the desktop binary from the repository root:

```bash
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/touko-memory.XXXXXXXX")
python3 scripts/benchmark.py --root "$ROOT" --counts 0,100,1000 --prepare-only
go build -trimpath -buildvcs=false -o "$ROOT/bin/touko-desktop" ./cmd/touko-desktop
```

Launch the desktop app against one prepared state and keep its PID:

```bash
"$ROOT/bin/touko-desktop" --state-dir "$ROOT/datasets/1000/state" --port 0 &
PID=$!
```

The app prints `TOUKO MyGo: http://127.0.0.1:PORT` and `State: ...`. Keep the
same PID for all four phases. Perform the phase's user action, then sample:

```bash
python3 scripts/desktop-memory.py --pid "$PID" --phase startup \
  --output "$ROOT/profiles/1000.jsonl" --samples 5 --interval 0.2
python3 scripts/desktop-memory.py --pid "$PID" --phase loaded \
  --output "$ROOT/profiles/1000.jsonl" --samples 5 --interval 0.2
python3 scripts/desktop-memory.py --pid "$PID" --phase browsed \
  --output "$ROOT/profiles/1000.jsonl" --samples 5 --interval 0.2
python3 scripts/desktop-memory.py --pid "$PID" --phase committed \
  --output "$ROOT/profiles/1000.jsonl" --samples 5 --interval 0.2
```

Phase boundaries should be recorded consistently:

- `startup`: immediately after the app process starts, with the prepared state
  selected.
- `loaded`: after the gallery state and representative images are visible.
- `browsed`: after scrolling through a repeatable, documented portion of the
  gallery.
- `committed`: after staging and committing the prepared batch images from
  `datasets/N/batch/` through the UI.

The profiler appends one JSONL row per invocation, with one `phase` field per
row; invoking it for all four phases writes four rows. It snapshots the verified
`touko-desktop` process tree, keeps every snapshot's aggregate in
`tree_rss_kib_observed`, and selects the snapshot with the highest aggregate
tree RSS for the per-process `processes` list and `aggregates`. The selected
snapshot is not a true peak. A missing WebKit renderer or network process is
reported as an explicit zero count, but that absence must not be reported as
zero memory use.

### Whole-tree RSS caveat

`aggregates.tree.rss_kib` adds RSS across processes. Shared pages, especially
WebKit mappings, are counted once per process, so the tree total is not
additional physical memory and is not PSS/USS. Use the per-process rows when
shared mappings matter, compare like phases and like process sets, and state
that RSS is resident set size rather than unique memory.

Do not infer renderer memory from the CLI/HTTP benchmark, and do not subtract a
missing renderer as if it were zero. If a renderer is absent, say it was absent
and only report the processes that actually existed.

## Limitations

- Timings use the host page cache. This harness does not drop caches or isolate
  CPUs, so absolute values depend on machine state.
- HTTP samples are loopback and storage measurements, not full frontend load
  measurements.
- Repository operations use local Git. GitHub API, clone, push and network
  publication latency are out of scope.
- Fixtures are synthetic deterministic color-pattern PNGs, not photographs.
  Image decode and compression behavior differs from real galleries.
- Medians over a small `--repeat` are descriptive. Do not present them as
  confidence intervals or regression proof without repeated runs.
- The script measures what the supplied binary does. Record its SHA-256 and the
  source revision/dirty flag from `benchmark.json` with any result.

## Cleanup

Generated roots are disposable but deliberately retained on failure. Remove a
root only after copying its `benchmark.json` and profile output:

```bash
rm -rf "$ROOT"
```

Never point `--root` at an existing TOUKO state, gallery, `logo/` directory or
another user data directory. The emptiness check is a safety gate, not a
migration tool.
