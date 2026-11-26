# Parallel delta restore: plan the copy by ref, not by tree (ParaSync)

Refs #64. Tracked by HIP-1344. The planner is `delta.go` in the root package, the
benchmark runner is `tools/deltabench`, the live `vfs.Volume` tests are in
`delta_test.go`, and the planner's own tests in `delta_internal_test.go`.

## 1. Primary reference, summary, and license audit

* **Primary paper:** ParaSync -- Exploiting Fine-Grained Parallelism for Efficient
  File Synchronization, 24th USENIX Conference on File and Storage Technologies
  (FAST '26), pages 477-492. Zhihao Zhang, Lu Tang, Huiba Li, Yue Yu, Guangtao Xue,
  Jiwu Shu, Yiming Zhang.
* **Summary:** content-defined chunking (CDC) sync spends its time in three phases --
  chunking, chunk matching and delta reconstruction -- and the paper breaks the
  dependencies that keep the three sequential, so that chunking, matching and
  reconstruction overlap. The published result is up to 7.6x faster chunking and up
  to 3.7x faster sync than the prior state of the art, at the same network traffic.
* **What this change takes from it, and what it does not.** The paper's subject is
  parallelism *inside* a CDC sync over a network. Its transfer volume, though, comes
  from chunk matching: a chunk the far side already holds is not sent. `hanzoai/vfs`
  has no chunk-level session -- a store holds sealed shards and packs -- so what is
  reproduced here is that *matching*, at the granularity the layout offers: two
  checkpoints of one Volume are compared by `ref`, an equal subtree is not walked,
  and the objects the target lacks are copied at once, several at a time. The
  pipelined reconstruction of §4 of the paper is **not** implemented: it needs a
  chunk stream, and this store's unit is the object.
* **License and clean-room audit:** `delta.go`, `delta_test.go`,
  `delta_internal_test.go`, `tools/deltabench` and this document are an independent,
  clean-room Apache-2.0 Go implementation written from this repository's own
  primitives (`shardKey`, `ref`, `decodeWhere`, `fence`). No upstream code was read,
  ported or vendored. The USENIX presentation page as read on 2026-03-06 offers the
  paper, the video and the slides under USENIX Open Access and links no code
  artifact, so there was nothing to copy and no license to reconcile beyond the
  paper's own; the novelty shipped here is the pruning rule, not the paper's
  pipeline. `go.mod` and `go.sum` are untouched: the planner uses the standard
  library and packages already in the module.

## 2. The baseline the planner has to beat

A restore into a store that holds nothing is the only restore the tree offers today:
`Volume` opens a `Head`, walks it and re-seals and writes every shard it reaches.
Nothing in the layout says which of those objects the destination may already hold,
so a second restore of a Volume that moved by one file costs what the first did.

Three properties of the layout are what make a cheaper copy possible, and none of
them needed a change to it:

1. **A shard is named by its content.** `shardKey` is a hash of the sealed bytes, so
   two checkpoints that share a subtree share the *name* of it: equal `ref` means
   equal bytes, and there is nothing to fetch or compare.
2. **A chunk lives at a recorded place in a pack.** The metadata records
   `where{pack, off, n}` per chunk (`record.go`), so a pack a chunk was appended to
   is named by every leaf that reaches one of its chunks. A pack named by both
   checkpoints does not travel.
3. **The Head goes last.** `fence` rewrites `head` only after every object it names
   is written, so a restore that dies leaves the destination on its old checkpoint
   rather than on a Head whose objects are missing.

## 3. Architecture (`delta.go`)

* `Side{Volume, At}` is one side of a plan: a store, and the checkpoint of it to
  name. A zero `Side` is an empty store, which is the full copy, so the same code
  serves both directions of the comparison. `Side.Checkpoint()` reads the Volume's
  Head when `At` is zero.
* `PlanDelta(ctx, from, to)` walks the two metadata trees in lockstep
  (`walker.walk`): equal `ref` for the same key range ends that subtree in O(1) and
  counts it in `Plan.Pruned`; otherwise the nodes are matched by their last ended
  child, and at a leaf the chunks of `to` are read through `decodeWhere`. A node
  whose height differs from its counterpart's is not trusted, and that subtree is
  copied conservatively.
* `Plan{From, To Head; Shards, Packs []string; Pruned, Leaves, Height int}` names
  the objects to copy as *keys* -- `meta/<2hh>/<hex>`, `pack/<2hh>/<hex>` -- sorted,
  so a plan is comparable and a test can assert one exactly.
* `(*Plan).Apply(ctx, src, dst)` copies them `Streams` at a time over the source
  backend and into the destination, verifying every shard against its own name,
  counting bytes moved, and stopping every other stream on the first error. It
  does not write the Head.
* `(*Plan).Commit(ctx, cfg)` seals the target Head to the destination's place and
  rewrites `head` through `fence`, refusing a round that does not exceed the
  recorded one (`ErrStaleRound`).
* `Restore(ctx, dst, src)` is the three in order, for a caller that has two
  `Config`s and nothing else.

A plan is only meaningful between two checkpoints of one Volume: `PlanDelta`
refuses sides whose `ID` or `Seed` differ (`ErrDelta`), because a shard of another
Volume does not open under this one's key and its chunks were cut differently.

## 4. Hypotheses, and what became of them

Registered before the runs in §5.

**H1 -- correctness does not depend on the change.** *Prediction:* after `Apply`
and `Commit` onto a store holding checkpoint A, the destination is
indistinguishable from one restored in full to checkpoint B: same inodes, same
contents, same metadata, and the metadata and pack objects it wrote are byte-equal
to the source's. *Verdict:* **holds** (`TestDeltaRestoreBringsAStoreToACheckpoint`,
three runs). An identical checkpoint is a plan of zero objects and zero writes
(`Pruned` = 1, the root), asserted exactly.

**H2 -- the cost of a restore tracks the change, not the tree.** *Prediction:* a
change of 1 % to 10 % of the files moves under a quarter of the bytes of a full
copy. *Verdict:* **bifurcated, and the reason is the finding of this bounty.** The
prediction holds with a wide margin when the data is in packs (1.4 % of the bytes
at a 1 % change) and fails when the data is inline in the metadata tree (50.5 % at
a 1 % change). Pruning is what it claims to be -- 14 or 15 subtrees of 22 in the
inline case -- but pruning only decides *which* objects travel; the **unit of
transfer** decides how much that is worth. See §6.

**H3 -- copying several objects at once shortens the restore.** *Prediction:* 8
streams beat 1. *Verdict:* **refuted on this store, and left `NO_MEDIDO` for a
network.** On an in-memory backend there is no latency to hide and 8 streams cost
more wall clock than 1 on every row that had work to do (21.4 % of the full copy
at 1 %, 8 streams, against 12.6 % at 1 stream). A store over the network is where
the prediction could hold; it was not measured here, and this document does not
claim it.

**H4 -- a delta restore is never worse than a full one.** *Prediction:* the two
are equal at worst. *Verdict:* **holds for bytes and objects, not for wall clock.**
At a 100 % change no subtree survives and the plan names every object the full copy
does, so bytes are equal to the byte; but the delta also pays the plan, so it is
slower than the full copy in that row. `tools/deltabench` fails rather than print a
row where the delta copies more objects than the full copy.

## 5. Measurements

Two workloads, each built by `tools/deltabench`: a Volume of `-files` files of
`-size` bytes, a checkpoint, then `-change` percent of the files written again and a
second checkpoint. Each row restores the *second* checkpoint twice: in full into an
empty store, and as a delta into a store that already holds the first. The two
restores are in one process, on one machine, and the pair is what the table
reports. Store: `internal/storetest.Mem`. Medians of 3 runs.

Raw rows: [`parasync.csv`](parasync.csv) (36 rows, one per restore). 8 CPUs, Go
1.27.4, darwin/arm64.

### 5a. Data inline in the tree: 600 files of 2 KiB, `-size 2048`

`small = 16 << 10` (`file.go:17`) keeps a file of this size inside the metadata
tree, so a change here moves `meta/` objects only: 15 to 24 shards for the whole
Volume, about 30 files per leaf.

| change | mode | objects | pruned | bytes | restore ms |
|---:|---|---:|---:|---:|---:|
| 0 % | full | 15 | 0 | 1,306,162 | 56.1 |
| 0 % | delta | 0 | 1 | 0 | 1.7 |
| 1 % | full | 22 | 0 | 1,318,593 | 97.3 |
| 1 % | delta | 7 | 15 | 666,389 | 12.2 |
| 5 % | full | 24 | 0 | 1,322,164 | 68.9 |
| 5 % | delta | 17 | 7 | 1,217,270 | 79.9 |
| 20 % | full | 23 | 0 | 1,320,409 | 96.6 |
| 20 % | delta | 21 | 2 | 1,307,706 | 20.3 |
| 100 % | full | 21 | 0 | 1,316,841 | 105.8 |
| 100 % | delta | 21 | 0 | 1,316,841 | 85.7 |

The delta as a percent of the full copy of the same checkpoint:

| change | objects | bytes | restore |
|---:|---:|---:|---:|
| 0 % | 0.0 % | 0.0 % | 3.1 % |
| 1 % | 31.8 % | 50.5 % | 12.6 % |
| 5 % | 70.8 % | 92.1 % | 115.9 % |
| 20 % | 91.3 % | 99.0 % | 21.0 % |
| 100 % | 100.0 % | 100.0 % | 81.0 % |

### 5b. Data in packs: 200 files of 48 KiB, `-size 49152`

Above the inline threshold a file is cut into chunks and the chunks are appended to
packs. The metadata is a single shard here, and what changes is the pack.

| change | mode | objects | shards | packs | bytes | restore ms |
|---:|---|---:|---:|---:|---:|---:|
| 0 % | full | 2 | 1 | 1 | 10,217,103 | 194.9 |
| 0 % | delta | 0 | 0 | 0 | 0 | 3.6 |
| 1 % | full | 3 | 1 | 2 | 10,318,999 | 276.9 |
| 1 % | delta | 2 | 1 | 1 | 140,599 | 9.4 |
| 20 % | full | 5 | 3 | 2 | 12,258,638 | 185.0 |
| 20 % | delta | 4 | 3 | 1 | 2,080,238 | 31.8 |
| 100 % | full | 5 | 3 | 2 | 20,410,636 | 598.4 |
| 100 % | delta | 4 | 3 | 1 | 10,232,236 | 170.1 |

The delta as a percent of the full copy of the same checkpoint:

| change | objects | bytes | restore |
|---:|---:|---:|---:|
| 0 % | 0.0 % | 0.0 % | 1.8 % |
| 1 % | 66.7 % | 1.4 % | 3.4 % |
| 20 % | 80.0 % | 17.0 % | 17.2 % |
| 100 % | 80.0 % | 50.1 % | 28.4 % |

## 6. Where the cost comes from, and the limits this leaves

**The unit of transfer decides, not the pruning.** An equal subtree is skipped in
O(1) in both workloads, and in both it is skipped in numbers the plan reports. What
differs is what one object carries. In 5a one object carries about 60 KiB -- a leaf
holding about 30 inlined files -- so a plan that copies a single leaf still moves a
tenth of the Volume, and a 1 % change spread over 6 leaves of 22 needs half the
bytes. In 5b one object carries a pack, and a pack holds the chunks of many files,
of which only the rewritten files' chunks are new: unchanged packs are named by both
checkpoints and stay home, so 1 % of change moves 1.4 % of the bytes.

Limits, declared rather than discovered later:

1. **Shard granularity is the floor for inlined data.** A leaf that contains one
   changed file is copied whole. Nothing below the shard exists to copy: a shard is
   sealed as one object and named by its hash.
2. **Pack granularity amplifies a small change.** A pack named by one new chunk
   travels whole, up to `packTarget` (16 MiB). No row here forced a full pack to
   travel -- the largest pack measured was 2,080,238 B -- so the amplification is
   **`NO_MEDIDO`**; the mechanism allows it and the table does not bound it.
3. **The clock is memory, not a network.** Absolute times are this machine's. The
   number to read is the ratio inside a pair, and it is the ratio that a store over
   `hanzoai/s3` would move -- in the direction H3 predicts, unmeasured.
4. **The shard count is not deterministic between runs.** The same workload gave 15
   to 24 shards on different runs, because the publication the Volume performs
   between writes decides where shard boundaries fall. The tests therefore assert
   relations (`delta` is a subset of `full`, `Objects() <= full.Objects()`) and the
   table reports medians, never one run's counts.
5. **A 100 % change is a full copy.** Measured in 5a: no subtree survives, so the
   plan names every object and the delta pays the plan on top. Declared negative.

## 7. Correctness and safety evidence

What was run here, on darwin/arm64 with Go 1.27.4: `gofmt -l .`, `go build ./...`
and `go vet ./...` clean; `go test -short ./...` green over every package except
`tools/gitbench::TestReferenceCloneBorrowsTheStore`, which fails on this machine for
a reason that predates the change (git resolves the temporary directory through
`/private`, so the alternates path it reports is not the store's); and
`go test -race -run 'TestDelta|TestRestore' .`, which is the whole of the
concurrency this change adds. The full `-race` gate was **not** run locally -- the
short suite alone takes 438 s on this machine -- so the CI gate is the first full
race run of this branch, and CI does not run on pull requests from a fork.

The tests:

* `TestDeltaRestoreBringsAStoreToACheckpoint` -- a Volume of 600 files, two
  checkpoints, a second store brought to the first and then to the second by delta.
  Asserts the snapshot is identical field by field to a full copy's, that the
  `meta/` and `pack/` objects written are byte-equal to the source's, that the Head
  is re-sealed to the destination's place, that a checkpoint identical to the one
  the store holds is zero objects and zero writes, and that a plan between two
  different Volumes is `ErrDelta`.
* `TestDeltaRestoreCopiesAWholeStore` -- `Restore` with an empty `dst` is a full
  copy, and the resulting Volume reads the same contents.
* `TestDeltaPlanCopiesWhatTheTargetLacks` -- two trees of 3,000 records several levels
  tall, a change confined to the first leaf: the plan names exactly the objects reachable from
  the target that are not reachable from the source, and every one of them is an
  object the target's tree names.
* `TestDeltaPlanSurvivesAHeightChange` -- the target tree is a level deeper and a
  level shallower than the source's; both directions produce a plan that
  reconstructs the target.
* `TestDeltaPlanSkipsPacksTheOlderTreeNames` -- a chunk appended to a pack the older
  tree already names: the plan copies the new shard and **no** pack, asserted as an
  exact key.

Concurrency: `Apply` starts one goroutine per stream, propagates the first error
through a channel and cancels the rest; the selector above is green under `-race`.
Isolation: nothing is copied that a shard's own name does not
verify, and a shard of another Volume does not open because it is sealed to that
Volume's `ID` -- `PlanDelta` refuses the pairing before any object is read.
Regression: the tree, journal and volume tests are unchanged and green.

## 8. Reproduction

    go run ./tools/deltabench                       # 5a, 600 x 2 KiB
    go run ./tools/deltabench -files 200 -size 49152 -change 0,1,20,100   # 5b
    go run ./tools/deltabench -csv docs/parasync.csv

`-streams`, `-order` (`spread` or `contiguous`) and `-reps` are the other knobs;
`-csv` writes one row per restore. The runner builds and restores in one process
and fails if a delta copies more objects than the full copy it is compared to.

## 9. Status against HIP-1344

Boxes 1 to 6 of the Research-to-PR Acceptance Contract: this document is 1 and 3,
`tools/deltabench` with its matched in-process baseline is 2, `delta.go` with the
tests above is 4 and 5, and §5 with `parasync.csv` is 6. Open item reported to the
HIP: the pack amplification of limit 2 is unmeasured, and H3 needs a store over the
network to be decided.
