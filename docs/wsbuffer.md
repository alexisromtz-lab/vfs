# WSBuffer — partial-write absorption for the journal's foreground writes

What a scrap buffer between the journal and its segment files does to the pages the media
writes, measured on this tree. Refs #58 (HIP-1344, Wave 1 #7).

The journal hands the file layer one `WriteAt` per batch, and a batch is written wherever the
previous one ended. Under concurrency a sync round carries several short batches, each at an
offset the writer did not choose, so a media that writes whole 4 KiB pages must round both ends
of every one of them out to the page: the bytes the journal writes are its logical bytes, the
bytes the media writes are those rounded out twice per batch. Handing the media one run instead
of several pieces removes the boundaries between them. That is what `pkg/wsbuffer` does, and
what it counts.

## 1. Paper and license audit

- **Primary research**: Yekang Zhan, Tianze Wang, Zheng Peng, Haichuan Hu, Jiahao Wu, Xiangrui
  Yang, Qiang Cao (Huazhong University of Science and Technology), Hong Jiang (University of
  Texas at Arlington), Jie Yao (Huazhong University of Science and Technology).
  *Rearchitecting Buffered I/O in the Era of High-Bandwidth SSDs.* 24th USENIX Conference on
  File and Storage Technologies (FAST '26), Santa Clara, CA, February 2026, pp. 579–595,
  ISBN 978-1-939133-53-3.
  <https://www.usenix.org/conference/fast26/presentation/zhan>
- **Design, as the paper describes it**: a *scrap buffer* holds the small and unaligned parts of
  a write in memory; the large and aligned parts go straight to the device; dirty data is
  flushed in two stages, opportunistically, so the flush that a sync needs has little left to do.
- **What is implemented here**: the first two. A `journal.Disk` that holds a write until the
  next one continues it, overwrites it, or a sync, a read, a writeback hint or a full window
  ends the grace period. The two-stage flush is **not** implemented: this layer flushes on the
  caller's path (see §6.3 for what that costs).
- **License**: this is a clean-room implementation from the published description. No artifact,
  patch or source of the paper's implementation was fetched, read or copied; the package is
  Apache-2.0 like the rest of this tree, and it carries no code under another license.
- **Patents**: not searched. Nothing here is claimed as novel; the mechanism is the paper's.

## 2. Matched baselines

The baseline is `journal` `main` on the same tree, the same Go runtime, the same disk and the
same second: `tools/wsbufferbench` runs both modes alternately, and each mode differs from the
other in one option (`wsbuffer.Options{Run}`). The mode called `through` is the journal writing
straight to `journal.Files`; it passes through the same counters, so the counters themselves are
not the difference.

`journal/bench` is the tree's own baseline and stays the authority for the absolute numbers
against plain and grouped files. It is `main_linux.go`, so on this machine (darwin/arm64) it does
not build:

    go build ./journal/bench/
    # package github.com/hanzoai/vfs/journal/bench: build constraints exclude all Go files

Absolute throughput against plain files is therefore **NO_MEDIDO** here. The ratio this document
publishes is journal-with-layer over journal-without-layer, which needs no second binary.

## 3. Falsifiable hypothesis and failure modes

- **H1 — the layer removes pages.** For one workload, `Stats.Pages < Stats.RefPages`.
  *Refuted if* the layer writes as many pages as the journal asked for, which is what it must do
  when every write is already page aligned and no two are contiguous.
- **H2 — the amplification is `1 + P/E[batch]`.** With pages of `P` bytes and a mean batch of
  `E[batch]` bytes, each batch wastes on average one page at its ends. *Refuted if* the measured
  ratio of a mode without the layer departs from that bound by more than the spread between
  repetitions. It holds: §4.1's `through` rows report 1.257 and 1.251 at 469–531 calls for
  8,450,556 B (mean batch 15–18 KiB, bound 1.23–1.26).
- **H3 — the layer costs nothing on the fast path.** *Refuted here*, and this is the finding that
  matters: at one sync per record the buffered mode is slower than the unbuffered one, because
  the flush happens on the sync path. §6.3 and §7 say what to do about it.
- **Failure modes the layer must not have** (each has a test in §5): a byte written to the journal
  and never reaching the file; a byte reaching the file twice or out of order; a reader of a
  segment seeing less than the journal said it wrote; a sync returning before the window is on
  the file.

## 4. Measurements

Machine: arm64, 8 CPUs, macOS, Go 1.27.4, load average 6.35 at the start of the run — a shared
machine, so the absolute throughput and the latency of one row are not portable; the ratio
between the two modes of the same row is what the table is for. Raw rows: `docs/wsbuffer.csv`.
Each writer appends one record and waits for it durable; `every` is the records a writer appends
between its `Sync` calls, that is, how many batches a sync round carries.

Reproduce with

    go run ./tools/wsbufferbench -dir /var/tmp/wsbuffer -writers 8 -ops 256 -every 1 -rep 2 -header

### 4.1 Pages the media writes

8 writers, 4 KiB records, 2,048 records, 8,450,296 B of payload = 2,063 pages.

| every | mode | pages (run 1 / 2) | ratio | pages, journal `main` | removed vs `main` |
|---|---|---|---|---|---|
| 1 | through | 2,594 / 2,582 | 1.257 / 1.251 | — | — |
| 1 | window | 2,521 / 2,518 | 1.222 / 1.220 | 2,594 / 2,582 | **2.8%** |
| 8 | through | 2,175 / 2,173 | 1.054 / 1.053 | — | — |
| 8 | window | 2,130 / 2,130 | 1.032 / 1.032 | 2,175 / 2,173 | **2.1%** |
| 32 | through | 2,137 / 2,117 | 1.036 / 1.026 | — | — |
| 32 | window | 2,092 / 2,090 | 1.014 / 1.013 | 2,137 / 2,117 | **2.1% / 1.3%** |
| 8 (256 KiB window) | through | 2,181 / 2,190 | 1.057 / 1.062 | — | — |
| 8 (256 KiB window) | window | 2,128 / 2,128 | 1.031 / 1.031 | 2,181 / 2,190 | **2.4%–2.8%** |

The `every=32` row is the one where the two repetitions disagree most (2,092 against 2,137
in the first, 2,090 against 2,117 in the second): it is published as a pair rather than
collapsed, because a single figure there would hide a factor of 1.6 between the runs.

The window's own counterfactual is larger than `main`'s at `every=1` (2,888 pages asked for
against 2,594): the layer changes the timing of the writer, so the journal's group commit forms
smaller batches and asks for more calls (826 against 531). Both comparisons are published
because they answer different questions — 12.7% of the calls *its own run* made, 2.8% of the
pages *`main`* wrote.

### 4.2 What the layer does when a round is long

The regime the layer is for is the one where a sync round carries many batches. In
`TestScrapJournalHoldsTheSameVolume` (`wsbuffer_journal_test.go`) a Volume writes seven files,
seventy journal calls land in one round, and a second Volume replays them through the layer:

| | journal asks the files for | the files get |
|---|---|---|
| through | 69 calls, 346,467 B, 153 pages, ratio 1.809 | the same |
| window (1 MiB) | 70 calls, 346,467 B, 154 pages, ratio 1.821 | 2 calls, 346,467 B, **86 pages, ratio 1.017** |

44% fewer pages, byte for byte the same stream, and both modes replay the same 119 changes.

### 4.3 CPU and latency

Per record, from `cpu_us` over 2,048 records (µs of process CPU, one row's own measure):

| every | mode | CPU/record | p50 of a sync | records/s |
|---|---|---|---|---|
| 1 | through | 25.3 / 28.7 | 3.99 ms / 4.00 ms | 1,820 / 1,699 |
| 1 | window | 24.7 / 27.7 | 7.03 ms / 7.00 ms | 1,176 / 1,205 |
| 8 | through | 9.6 / 8.8 | 4.02 ms / 6.06 ms | 9,595 / 8,399 |
| 8 | window | 10.7 / 8.9 | 7.95 ms / 7.96 ms | 7,912 / 7,497 |
| 32 | through | 9.9 / 12.1 | 16.59 ms / 17.38 ms | 16,265 / 12,155 |
| 32 | window | 12.0 / 13.7 | 12.55 ms / 11.89 ms | 14,338 / 14,431 |

No CPU cost is visible at `every=1`. At `every=32` the buffered mode is 13–21% dearer in CPU, the
p50 of a sync is lower in both repetitions (12.6 / 11.9 ms against 16.6 / 17.4 ms) and the counted
throughput is mixed (14,338 / 14,431 against 16,265 / 12,155 records/s): at that rate the fsync of
a round no longer waits behind a chain of small writes, and what is left is noise from a shared
machine.

## 5. Evidence and safety

- **Byte identity**: the counters prove it per row. `Stats.Bytes == Stats.RefBytes` in all 16 rows
  of `docs/wsbuffer.csv`; a lost, doubled or reordered byte would break that identity. The unit
  test `TestScrapWritesTheSameBytes` compares two directories byte for byte, written through the
  layer and written straight.
- **Concurrency**: `TestScrapConcurrent` writes 8 × 64 records from 8 goroutines through one
  layer and compares the result byte for byte; the package passes `go test -race`.
- **Replay**: a Volume that closes with unpublished changes leaves them in its journal, and the
  Volume that takes over reads them back through the same wrapped `File`. `ReadAt` writes the
  window first, so a replay cannot read short.
  `TestScrapJournalHoldsTheSameVolume` proves the tree, the journal watermarks and the inode
  count are identical with and without the layer, and that the changes really were replayed.
- **Clean close**: `TestScrapJournalAfterACleanClose` proves nothing is replayed twice when the
  journal ended with everything published.
- **Durability**: `Sync` flushes the window before it returns, so a record the journal calls
  durable is on the file. `Close` does the same, and `Disk.Close` flushes every file it handed
  out, including one the journal left open.
- **Multi-tenant isolation**: the layer holds no state across segments or Volumes — one `Disk` per
  journal directory (`Config.Disk` is called per directory), one window per `File`, keyed by the
  offset the journal passes in.
- **The CI selectors were run here, with one red that is not this change.** `go build` / `go vet`
  over the tree CI builds, the `GOOS=darwin` cross build, `gofmt -l` empty, the `unit` selector,
  `go test -race -short ./pkg/...` and `go test -race -short -run '^TestScrap' .` are green.
  `TestRacingClaims` failed once in the full `unit` run with `journal: directory is in use by
  another journal`; it flakes **1 of 8** runs when selected alone
  (`go test -short -count=1 -run '^TestRacingClaims$' .`), and it does not go through this layer —
  it calls the stock `cfg`, which leaves `Config.Disk` unset. Declared, not attributed.

## 6. Limits and negative regimes

1. **The page the journal's mark lives in cannot be merged.** Marks are 20 B at offsets 40 and 60
   of a segment's first page, while data is being written at the segment's end. A mark ends any
   open window and pays a page of its own in both modes; nothing in this layer can change that.
2. **A window boundary is a page boundary.** The default window is 1 MiB, the span after which
   the journal itself asks the system to start writing out, so the window ends where the journal
   is already splitting the stream. Each boundary costs one page per MiB, which is the 1.01–1.03
   floor the `every=32` rows sit at rather than the 1.00 an oracle would reach.
3. **The flush is on the sync path, and that costs latency.** `Sync` must flush the window before
   it can fsync, so at one sync per record the p50 of a sync goes from ~4.0 ms to ~7.0 ms and the
   throughput drops by a third (§4.3). The bytes written are fewer; the wait for the first one is
   longer. This is the part of WSBuffer that is missing here: the paper's two-stage flush would
   drain the window before the sync needs it. Until then, at fine-grained sync rates the layer
   trades throughput for write amplification; at coarse ones (`every=32`) it lowers the p50 of a
   sync and leaves throughput where the shared machine's noise puts it.
4. **A smaller window does not buy the latency back** (2.4% of pages removed at 256 KiB against
   2.1% at 1 MiB, with the same p50): the window usually does not fill between two syncs at this
   rate, so the flush lands on the sync path whatever the window is.
5. **Throughput against plain files is not measured here** (§2).
6. **These are counts of pages, not of device writes.** The kernel, the filesystem and the SSD
   each coalesce and split on their own; what this instrument measures is what the journal asks
   the file layer for, which is the part this tree decides.

## 7. Recommendation

1. **Ship the counters, not the coalescing, by default.** `wsbuffer.Options{Run: -1}` is a
   pass-through that counts: it changes nothing about what reaches the files and gives the
   journal a write-amplification number in every run. That is worth having in `journal`'s own
   tests and in `journal/bench`, independently of this experiment.
2. **For the coalescing to win where the bounty measures it** (4 KiB random writes with a sync
   per write), the journal has to drain the window earlier: one `Writeback` hint for the batch it
   just wrote, instead of one per MiB. With that, the flush moves off the sync path and the
   expected result is the page count of `every=1` with the latency of `through`. That is a change
   in `journal`, and it is not in this branch.
3. **Do not adopt a window above 1 MiB.** The journal's own hint ends at 1 MiB; a larger window
   would be flushed by the hint anyway and would only make the sync-path flush bigger.

## 8. Reproduction

    go test -count=1 ./pkg/wsbuffer/                 # the layer, alone
    go test -count=1 -run TestScrapJournal -v .      # through a Volume, including replay
    go run ./tools/wsbufferbench -dir /var/tmp/wsbuffer -writers 8 -ops 256 -every 1 -rep 2 -header

`docs/wsbuffer.csv` is four such invocations (`-every 1`, `-every 8`, `-every 32`, and `-every 8
-window 262144`), each with `-rep 2`, concatenated under one header.

Columns: `run` repetition; `mode` through (journal `main`) or window; `window` bytes the layer
holds (`-1` = write through); `records` appends; `ref_calls`/`ref_bytes`/`ref_pages` what the
journal asked the files for; `calls`/`bytes`/`pages` what reached them; `absorbed` writes merged
into a window and `flushed` windows written; `ref_ratio`/`ratio` pages × 4096 / bytes for each
column pair; `ops_s` records per second; `p50_us`/`p99_us` of the calls that waited for
durability; `cpu_us` process CPU for the whole run.
