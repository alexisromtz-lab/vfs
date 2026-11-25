# Compact sparse-range indexing for journal spans and sparse files (RASK)

Refs #59. Tracked by HIP-1344. The library is `pkg/rask`, the synthetic
benchmark runner is `tools/raskbench`, and the live `vfs.Volume` integration
tests are in `rask_journal_test.go` and `pkg/rask/index_test.go`. Raw rows of the
integration runs are in [`rask.csv`](rask.csv).

A Research Bounty admits a documented negative result. One is reported here, and
it comes from the engine's own data rather than from a synthetic shape.

## Verdict

1. **The encoding wins on bytes.** Per extent the payload is 4.12 B against the
   baseline's 16 (0.26x) on extents written in bursts, 7.10 B (0.44x) when every
   offset is random. A run that continues where the last one ended costs one byte
   of gap plus one or two of length.
2. **As built, the index is 9.59 B per extent (0.51x) on 100,000 synthetic
   extents, and 0.53x-0.81x on the spans a real `Volume` writes.** The gap
   between 4.12 and 9.59 is per-leaf bookkeeping and unused append capacity, not
   encoding.
3. **On lookup, the index loses — and the real journal makes it worse.** On
   100,000 synthetic extents the p99 ratio is 1.18x-1.63x up to 64 extents per
   leaf and 11x-33x at 1,024. On the spans this Volume actually journaled, the
   **median** is 2.5x-3.0x at 8 extents per leaf, 3.8x-4.6x at 32 and 6.6x-8.3x
   at 64, in three runs. **H2 is refuted, and the integration is what refutes
   it**: the synthetic shape is kinder to the index than the engine is.
4. **H3 holds, on the real path too.** A file truncated to 1 PiB whose runs sit
   in its middle indexes at 9.68 B/extent — the same bytes as a small file,
   because the cost follows the number of extents, not the logical span. Every
   run the journal recorded is found at both of its ends and every hole between
   runs is not.

## 1. Primary reference, summary, and license audit

* **Primary paper:** "Range as a Key" is the Key! Fast and Compact Cloud Block
  Store Index with RASK — Haoru Zhao, Mingkai Dong, Erci Xu, Zhongyu Wang, Haibo
  Chen. FAST '26, pp. 183–201. Open access.
* **Design followed:** a *range* is the key, not the offset. Each leaf keeps a
  log of `(gap, length)` varint pairs; the leaf's last extent bounds the search
  key; a leaf that passes its maximum splits in two, so neither the bounds search
  nor the in-leaf scan grows without bound.
* **Design not followed:** the paper's garbage collection and range-aware *merge*
  are absent — section 6 declares it. What is measured is the log-structured
  leaf and the split, not the full tree.
* **Clean-room:** the paper's USENIX page links no source artifact, so this is an
  implementation **from the prose**. Nothing is copied, no dependency is added,
  nothing is redistributed. `go.mod` and `go.sum` are untouched: standard library
  only.
* **License:** `hanzoai/vfs` is Apache-2.0 and so is `pkg/rask`.
* **Patent search:** **not performed.** `NO_MEDIDO`, not "none".

Figures published in the paper are the authors' own and are not restated here as
Hanzo expectations. Every number below was produced by the code in this PR, on
the host in section 8.

## 2. Matched baselines

`Slice` is the baseline **in the engine's own terms**: one array sorted by
offset, binary search for the covering entry, `cap(e)*16` bytes. It is built from
the same `[]Extent`, in the same process, on the same run as the index.

| Candidate element | Definition | Bytes/extent | Lives where |
|---|---|---:|---|
| `run{a, b uint64}` | the engine's in-memory run list | **16** | `file.go` |
| `extent{off, n, id}` | the persistent record, chunk id included | 40 | `record.go` |
| `rask.Extent{Off uint64, Len uint32}` | this prototype's in-memory extent (+4 B padding) | 16 | `pkg/rask/extent.go` |

The baseline models the **16-byte in-memory run**, because that is what `vfs`
holds while it runs, and because neither structure carries a chunk id. The
40-byte persistent record is not the comparator: it is a format-v2 user story,
and format-v2 is explicitly out of scope. On a real sparse file the array's
measured cost is **20.46 B/extent at 2,002 runs and 18.84 B/extent at 20,002** —
the append schedule reserves more than 16 bytes per entry, which is why the
ratio against it is better than 9.59/16 would suggest.

## 3. Falsifiable hypothesis, stated before the numbers were read

> **H1** — at 100,000 extents written in bursts, the index costs at most **40%**
> of the baseline's bytes per extent.
> **H2** — the covering lookup's **p99** stays within **1.20x** of the
> baseline's.
> **H3** — the cost follows the **number of extents**, not the logical span.

Declared failure modes, written before the run:

* per-leaf fixed cost can exceed the encoding win at small `n`;
* the in-leaf scan is linear in the leaf, so a large leaf buys memory and pays
  latency in the same move;
* the base offset enters the first gap of every leaf in absolute form, so a wide
  file must cost a few more bytes per leaf;
* **a synthetic shape can be kinder than the engine**: the harness places
  extents on an idealised grid, and the live write path coalesces, journals and
  replays its own records. That last one is why `rask_journal_test.go` exists,
  and it is the failure mode this PR confirms.

There is no H4: three hypotheses were pre-registered and two are reported
refuted, rather than a fourth invented after seeing the numbers.

## 4. Measurements

### A. Bytes per extent, deterministic

Same shapes, same seeds, same leaf sizes: these figures came out bit-identical
across three runs.

| Shape | n | max leaf | B/ext baseline | B/ext RASK | ratio |
|---|---:|---:|---:|---:|---:|
| contiguous | 100,000 | 64 | 18.84 | 6.31 | **0.34** |
| clustered | 100,000 | 32 | 18.84 | 11.48 | **0.61** |
| clustered | 100,000 | 64 | 18.84 | 9.59 | **0.51** |
| uniform | 100,000 | 64 | 18.84 | 9.85 | **0.52** |

H1 (≤0.40x as built) is **refuted** for the pattern this prototype exists for:
it holds only where extents never gap (0.34x). The 5.47 B/extent between the
4.12 B payload and the 9.59 B structure is per-leaf headers and unused append
capacity, and it is closable (section 7).

### B. The engine's own spans, through a real `Volume`

`rask_journal_test.go` writes a file truncated to **1 PiB**, then replays the
Volume's journal with the Volume's own path — `v.led.j.Replay` -> `v.led.open`
(unseal with the journal's AES-256-GCM key) -> `decodeChange` — and builds the
index from those decoded write records. Nothing synthetic: the extents being
indexed are the ones the engine recorded.

| runs | leaf | B/ext RASK | B/ext array | ratio | p50 RASK/array | p50 ratio | p90 ratio | p99 ratio |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 2,002 | 8 | 15 | 20.46 | 0.74 | 94/31 ns | **2.94-3.03** | 3.81-3.89 | 3.83-4.95 |
| 2,002 | 32 | 11 | 20.46 | 0.57 | 135-143/30-31 ns | **4.48-4.61** | 6.46-6.72 | 7.08-18.77 |
| 2,002 | 64 | 9 | 20.46 | 0.47 | 255-266/31-32 ns | **8.00-8.31** | 12.03-12.24 | 16.93-23.29 |
| 20,002 | 8 | 15 | 18.84 | 0.81 | 96-99/37-39 ns | **2.54-2.61** | 2.96-3.27 | 3.25-5.53 |
| 20,002 | 32 | 11 | 18.84 | 0.62 | 144-151/38-39 ns | **3.79-3.89** | 5.24-5.43 | 6.23-9.09 |
| 20,002 | 64 | 9 | 18.84 | 0.53 | 252-269/38-39 ns | **6.63-6.90** | 9.41-9.96 | 10.69-52.79 |

Three runs each, `rask.csv` has the rows. Each sample is the least of two passes
of 200 calls, so a preemption inflates neither arm; the two clock calls are in
both arms and are subtracted, which can only raise the ratio.

The consequence is the honest headline of this PR: **the index is smaller and
slower on the engine's data at every leaf width measured.** 0.47x-0.81x of the
bytes for 2.5x-8.3x the median lookup.

Two things this says that the synthetic half cannot:

* **The engine's extents coalesce.** 100,000 writes at stride 4,096 produce
  **20,603** journal write records; at stride 65,536, 21,488; at stride 1 MiB,
  15,592. One run each, with the helper in this file, not asserted by the test.
  The live path therefore does not reach the extent counts where a compact index
  starts to matter for cache residency — and that is exactly where the index
  would have had a chance.
* **The synthetic shape flatters the index.** At 100,000 extents the harness
  reports a p50 ratio of 1.45 at 16 extents per leaf; on 20,002 real spans, at
  the same leaf width, the measured ratio is 2.54-3.89. The harness's ratio is
  also diluted toward 1.0 by a 74 ns clock sample present in both arms, so if
  anything the synthetic figure is *optimistic*.

### C. Lookup percentiles, synthetic (100,000 clustered extents)

20,000 interleaved single-call samples per arm, three runs, ratios of like
percentiles:

| max leaf | p50 ratio | p99 ratio |
|---|---:|---:|
| 16 | 1.40-1.45 | 1.18-1.40 |
| 32 | 1.45-1.50 | 1.21-1.35 |
| 64 | 1.67 | 1.33-1.63 |
| 128 | 2.30-2.40 | 1.86-2.30 |
| 256 | 3.60-3.78 | 3.42-3.65 |
| 1,024 | 9.17-10.29 | 11.01-33.45 |

H2 (≤1.20x) is over the threshold at 32 extents per leaf and above in every run;
at 16 it straddles (1.18, 1.40, 1.19). There is no leaf size that satisfies both
halves of the claim: the byte win falls to 0.61x where the tail approaches the
threshold, and the index never comes closer than 1.18x.

### D. One PiB, on the real path

The integration test's file is truncated to 2^50 bytes and its runs sit at the
head, in a clustered window at 2^49, and on the last page. 2,002 journal records
index into **19,376 B** against the array's 40,960 B (0.47x), and the H3 sweep
over identical gaps at base 0 / 1 GiB / 1 PiB gives 97,720 / 98,488 / 101,048 B
against a flat 180,224 — **+3.4% across a 2^50 move**. The cost follows the
number of extents.

## 5. Evidence and safety

* **Correctness on the real path.** `TestRASKIndexesTheJournalSpansOfRealWrites`
  requires, for every run the journal recorded, that both ends of the run resolve
  to *that* run and that the first byte past it, before the next run, resolves to
  nothing — 2,001 holes checked. Rebuilding from a second replay must give the
  same bytes and the same entry count: the index is a function of what the
  journal holds.
* **Correctness in the library.** `pkg/rask/index_test.go` compares every answer
  against a linear-scan model over random, burst and adversarial shapes, plus
  out-of-order inserts; invariants are bounds strictly ascending, every leaf
  within `[8, max]` entries, decoded entries equal to the sorted input.
* **Agreement with the baseline.** Every probe is answered by both structures
  before either is timed, and a disagreement fails the test rather than being
  timed.
* **Concurrency.** `Index` takes no lock: it is written once and published, like
  the engine's runs. `pkg/rask/concurrent_test.go` checks eight concurrent
  readers against the model under `-race`. A concurrent *writer* is out of scope
  and would corrupt the log — that is the boundary of the claim.
* **Isolation and format.** The index is in memory and per file; it touches no
  key, no tenant boundary and no byte of the persistent format-v2 layout. `head`,
  `meta/` and `pack/` are unchanged, so there is no regression surface on the
  write path: the Volume writes exactly what it wrote before.
* **Security.** No parsing of untrusted bytes, no allocation sized from an
  attacker-controlled field. The one input assumption is the engine's own:
  sorted, disjoint runs.

## 6. Limitations and negative regimes

* **Small `n` is a loss for the allocator.** At 1,000 clustered extents the
  prototype allocates **1.66x more** than the baseline — one header and one log
  per leaf is a lot of small allocations relative to 31 of them. Bytes in the
  live structure still win (0.45x).
* **`uniform` halves the byte win**: 0.52x instead of 0.34x. Any workload whose
  extents never continue sees the smaller number.
* **`uniform` is measured once, not as a band.** `-shapes clustered,uniform`
  samples both shapes in one run: the median is worse at every leaf size
  (1.51-13.66 against 1.25-10.23), but the p99 tail is inside the noise of
  `clustered` below 64 extents per leaf and *better* at 64 (1.55 against 1.81).
  The expectation that random offsets would be worse on the tail was not
  confirmed.
* **The p99 column in section B is a scheduler measurement at this sample
  count.** With 2,000 probes the 99th is the twentieth-worst sample; on a shared
  machine one tick in one arm moves it from 3x to 53x. The test asserts the p50
  and the p90 — both stable run to run — and reports the p99 without asserting
  it. The tail the Acceptance Test names is measured in section C, over 20,000
  samples and three runs.
* **Absolute `ns/op` from this host is not a measurement.** Four runs of one
  binary, same seeds, moved the mean between 2,540 and 28,045 at leaf 1,024 —
  a factor of 11. Only interleaved ratios are quoted.
* **The engine-level petabyte test does not run here.** `TestPetabyteScattered`
  reads `/proc/self/status` and skips on darwin, so the *engine's* petabyte
  behaviour is unchanged and unmeasured by this submission; what is measured is
  the index's behaviour over the records of a 1 PiB sparse file.
* **No merge or delete.** The prototype has no per-extent removal: holes are
  punched by rewriting runs at publication, not by removing one extent.

## 7. Deliverable, recommendation, and what closes the gap

**The deliverable is the bounded implementation plus this negative result: do
not adopt the two-level index as built. The variable-length encoding is worth
taking; the second level is not.**

* **The encoding is ready.** `(gap, length)` costs 0.26x-0.52x of the current
  per-extent bytes and needs no format change.
* **The lookup loses, on synthetic data and worse on the engine's.** At
  engine-realistic sizes — 2,000 to 20,000 runs — the array is 2.5x-8.3x faster
  in the median, and the array is under 400 KiB there, i.e. inside cache. A
  two-level structure with an in-leaf scan is the wrong shape for that regime;
  binary search over a contiguous array is what the hardware is built for.

What follows from that, in the order this evidence supports it:

1. **Take the encoding into the existing structure.** Keep the sorted array that
   `file.go` already searches and shrink its element instead of adding a level
   above it. That is a smaller, better-attributed change and it cannot lose on
   latency, because the search does not change.
2. **If a second level is wanted anyway, it must arrive with an in-leaf fence.**
   `(bytePos, off)` every eight extents, roughly 1.5 B/extent — an arithmetic
   estimate, not a measurement — turns the scan into a bounded one. Nothing in
   this file is evidence that it is enough.
3. **Size the leaf logs instead of doubling them.** Most of the gap between the
   4.12 B payload and the 9.59 B structure is capacity an append reserved and
   never used.
4. **Arena per generation**, to remove the 1.66x allocation churn at small `n`
   without changing the layout.

## 8. Reproducing

Host of every number here: `go1.27.4`, `darwin/arm64`, 8 logical CPUs, box
shared while these ran (the tool prints `load unavailable on darwin` rather than
a number it cannot read).

```sh
go run ./tools/raskbench                       # synthetic half, ~1 min
go run ./tools/raskbench -shapes clustered,uniform -samples 20000   # both shapes
go run ./tools/raskbench -no-timing            # deterministic bytes only, ~20 s
go test -run 'TestRASK' -count=1 -v .          # the real-journal integration, ~6 s
go test -race -count=1 ./pkg/rask              # library correctness under -race
go test -run '^$' -bench RASKLookupJournalSpans ./...   # one command, both arms
```

The percentile sweep takes `-samples` (default 20,000 per arm) and `-shapes`;
the mean sweep takes `-reps`. Read the report three or four times and treat it as
a band: a single absolute time from this host is not evidence.

| File | What it is |
|---|---|
| `pkg/rask/extent.go` | `Extent{Off uint64, Len uint32}`, shared by both sides |
| `pkg/rask/slice.go` | the baseline: sorted array, binary search, `cap*16` |
| `pkg/rask/index.go` | the prototype: leaves of gap/length varints behind a bounds array |
| `pkg/rask/workload.go` | the shapes, the probes and the allocation meter |
| `pkg/rask/index_test.go` | agreement with a model, out-of-order insert, invariants, a 2^50 span |
| `pkg/rask/concurrent_test.go` | eight readers, `-race` |
| `pkg/rask/measure_test.go` | the pre-registered hypothesis and the deterministic table |
| `rask_journal_test.go` | the live `Volume`: journal replay, real spans, 1 PiB, both arms timed |
| `tools/raskbench/main.go` | the synthetic runner |
| `docs/rask.csv` | the raw rows of the integration runs (three runs, six configurations) |
