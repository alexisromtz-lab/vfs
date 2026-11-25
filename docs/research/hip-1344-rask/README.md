# HIP-1344: Compact Sparse-Range Indexing for `hanzoai/vfs` (RASK)

**Refs:** `hanzoai/vfs#59` · **Tracking:** HIP-1344 · **Implementation:** [`internal/index/rask/`](../../../internal/index/rask/), [`tools/rask-bench/main.go`](../../../tools/rask-bench/main.go) · **Numbers:** [`score_table.md`](score_table.md), [`raw_results.json`](raw_results.json)

A research bounty admits a documented negative result. One is reported here.

---

## Verdict first

1. **The encoding wins, and it is not close.** Per extent the payload is **4.12 B against the baseline's 16** (0.26x) on extents written in bursts, and 7.10 B (0.44x) when every offset is random. A run that continues where the last one ended costs one byte of gap plus one or two of length; the sixteen bytes of a `run{a, b uint64}` are a choice, and they are not needed to find a covering range.
2. **As built, the index is 9.59 B per extent (0.51x), not 4.12.** The 5.47 bytes in between are per-leaf bookkeeping and the capacity an append left unused — not encoding. **H1 (≤40%) is refuted as built** for the pattern this prototype exists for; it holds only where extents never gap (0.34x). The gap is attributed and closable, and closing it is the next step, not this submission.
3. **On lookup p99, which the Acceptance Test names, the answer is no.** Sampled one call at a time with the baseline and the index interleaved in the same iteration, the p99 ratio is **1.76x to 2.05x at 64 extents per leaf and 6.20x to 9.81x at 512** — over 1.20x in all four runs, so that half of H2 is **refuted**, not noisy. At **32 extents per leaf**, the configuration with the best byte ratio, the band is **1.23-1.41**: over the threshold in all four runs but by only 2.5% at the low end, which is a thin refutation and is reported as thin. At 8-16 extents per leaf it is **1.05-1.27**, which decides nothing, and there the byte win drops to 0.61x. There is no leaf size that satisfies both halves of the claim.
4. **H3 holds.** Moving the whole file from offset 0 to 2^50 with every gap unchanged costs **+3.4%** (97,720 → 101,048 B) while the baseline stays flat at 180,224 B. The cost follows the number of extents, not the logical span.

---

## 1. Paper & License Audit

* **Primary reference:** RASK (Zhao et al., **USENIX FAST '26**, open access).
* **Design followed:** a *range* is the key, not the offset. Each leaf keeps a log of `(gap, length)` pairs; the leaf's last extent bounds the search key; a leaf that passes its maximum splits in two, so neither the search nor the scan degrades without bound.
* **Clean-room:** the paper's USENIX page links no source artifact, so this is an implementation **from the prose**. Nothing is copied, no dependency is added, nothing is redistributed.
* **License:** `hanzoai/vfs` is Apache-2.0; so is this package. `go.mod` and `go.sum` are untouched — standard library only.
* **Patent search:** **not performed.** `NO_MEDIDO`, not "none".

Published numbers in the paper are the authors' own and are not restated here as expectations. Every figure in this file was produced by the code below, on the host declared in section 8.

---

## 2. Matched Baselines vs `vfs` `main`

`Slice` is the baseline **in the engine's own terms**, inside the same package, built from the same `[]Extent` on the same run: one array sorted by offset, binary search for the covering entry, `cap(e)*16` bytes.

The element size is the whole comparison, so it is stated rather than assumed:

| Candidate element | Definition | Bytes/extent | Lives where |
|---|---|---:|---|
| `run{a, b uint64}` | the engine's in-memory run list | **16** | `file.go:590` |
| `extent{off, n, id}` | the persistent record, chunk id included | 40 | `record.go:235` |
| `Extent{Off uint64, Len uint32}` | this prototype's in-memory extent (+4 B padding) | 16 | `internal/index/rask/extent.go` |

The baseline models the **16-byte in-memory run**, because that is what `vfs` actually holds while it is running, and because the index does not carry a chunk id on either side. The 40-byte record is *not* the comparator: it is a persistent-format user story, out of scope here (section 6). The baseline element is therefore the one that does **not** carry a field the index is meant to exclude. Concretely: an extra 8-byte field on the baseline element moves the reported ratio from **0.64 to 0.42 at 100,000 extents** without changing one byte of the index, because the denominator doubles in the row that is supposed to stay fixed. Whichever way this bounty is settled, that arithmetic has to be identical on both sides of the comparison.

Both structures are built by the same loop over the same slice, on the same machine, in one process. There is no upstream baseline to match, because the paper ships no artifact: the honest baseline is the engine's current shape.

---

## 3. Falsifiable Hypothesis, stated before the numbers were read

> **H1** — at 100,000 extents written in bursts, the index costs at most **40%** of the baseline's bytes per extent.
> **H2** — the covering lookup's **p99** stays within **1.20x** of the baseline's p99.
> **H3** — the cost follows the **number of extents**, not the logical span.

Declared failure modes, written before the run:

* the per-leaf fixed cost can **exceed** the encoding win at small `n`;
* the in-leaf scan is linear in the leaf, so a large leaf size buys memory and pays latency in the same move;
* the base offset enters the first gap of every leaf in absolute form, so a wide file must cost a few more bytes per leaf.

The threshold is the one pre-registered. The **estimator** was fixed before this run for one reason, stated so it can be held against the numbers: the Acceptance Test asks for p99, and a mean over a loop of millions of calls is not a percentile. Both estimators were run anyway (sections 4 and 4b of the score table), so the choice can be checked rather than trusted.

There is no H4 here: this submission pre-registered three hypotheses and reports the refutation of two, rather than inventing a fourth after seeing the numbers.

---

## 4. Results

Full tables in [`score_table.md`](score_table.md); machine-readable source in [`raw_results.json`](raw_results.json).

| Hypothesis | Prediction | Measured | Verdict |
|---|---|---|---|
| **H1** | rask ≤ 0.40x baseline B/ext at 100k clustered | 0.51x built, 0.26x payload | **refuted as built** |
| **H2** | lookup p99 within 1.20x | 1.76-2.05 at 64 extents/leaf, 6.20-9.81 at 512; 1.23-1.41 at 32; 1.05-1.27 at 8-16 | **refuted at ≥32 extents/leaf, unresolved below** |
| **H3** | cost tracks extents, not span | +3.4% across a 2^50 move | **holds** |

Two numbers decide this and they point in opposite directions. **The payload is 4.12 B per extent and the structure is 9.59 B** — 5.47 bytes of bookkeeping per extent stand between the encoding and the claim. And the index is **slower than binary search at every leaf size**, refuted at ≥32 extents per leaf and undecided below. Section 7 says what that means for a merge.

---

## 5. Correctness, Concurrency, Isolation, Security

* **Correctness**: every extent the index answers is compared against a linear-scan model over random, burst and adversarial shapes, plus out-of-order inserts.
* **Invariants**: bounds strictly ascending; every leaf within `[8, max]` entries; decoded entries equal to the sorted input.
* **Concurrency**: `Index` takes no lock. It is written once and published, like the engine's runs. Eight concurrent readers are checked against the model under `-race`. A concurrent **writer** is out of scope and would corrupt the log — that is the boundary of the claim, not an oversight.
* **Isolation and format**: an in-memory study. No file, no key, no tenant boundary; no byte of the persistent format-v2 layout is touched. There is no regression surface to test because no existing path changes.
* **Security**: no parsing of untrusted bytes, no allocation sized from an attacker-controlled field. The one input assumption is the engine's own: sorted, disjoint, non-touching runs (`file.go`'s `layer.dirty`).

---

## 6. Limitations and Negative Regimes

* **Small `n` is a loss, not a tie.** At 1,000 extents written in bursts the prototype allocates **1.66x more** than the baseline: one header plus one log per leaf is a lot of small allocations relative to 31 of them. The live structure still wins (0.45x bytes), but the allocator does not.
* **`uniform` halves the win.** Random offsets make every gap a six-byte varint: 0.52x instead of 0.34x. Any workload whose extents never continue sees the smaller number.
* **`uniform` is untested for time.** The timing sweep runs on the clustered shape only. A random offset lands in a leaf whose scan is long and whose bounds are cold, so the real ratio there is expected to be worse than the one reported — **expected** is the word: it is not measured.
* **Absolute `ns/op` from this host is not a measurement.** Four runs of the same binary, same seeds, moved the mean `ns/op` at leaf 1,024 between **2,540 and 28,045** — a factor of 11. The percentiles in section 4 are interleaved against the baseline so the ratio survives that; the absolute figures do not, and none is quoted as a cost.
* **Two leaf sizes were swept for bytes and six for time**, because the byte curve is monotone enough that four points settle it and the time curve is not.
* **The engine-level petabyte test does not run here.** `TestPetabyteScattered` reads `/proc/self/status` for RSS (`sparse_test.go:371`) and skips on darwin. The engine's petabyte behaviour is unchanged and unmeasured by this submission.
* **Merge and delete are not exercised.** The prototype has no per-extent delete: the engine punches holes by rewriting runs at publication, not by removing one extent from the index.

---

## 7. Integration Recommendation, and What Closes the Gap

**Recommendation: do not merge the index as built. The variable-length encoding is worth taking; the two-level search is not.**

Two findings point in opposite directions and neither cancels the other:

* **The encoding is ready.** `(gap, length)` costs 0.26x-0.44x of the current per-extent bytes and needs no format change.
* **The lookup loses.** From 32 extents per leaf up, the p99 ratio is over 1.20x in all four runs (thinly at 32, decisively above it); at 8-16, where the threshold is undecided, the byte win falls to 0.61x. A two-level index with an in-leaf scan is the wrong shape for 100,000 contiguous runs, where binary search over a contiguous array is what the hardware is built for.

What follows from that, in the order I would do it:

1. **Take the encoding into the existing structure.** Keep the sorted array that `file.go` already searches and shrink its element, instead of adding a second level above it. That is a smaller, better-attributed change and it cannot lose on p99, because the search does not change.
2. **If a second level is wanted anyway, it must arrive with an in-leaf fence.** `(bytePos, off)` every eight extents at roughly 1.5 B/extent (an arithmetic estimate from an 8-byte entry every 8 extents, not a measurement) turns the scan into a bounded one. Nothing in this file is evidence that it is enough; it is the next experiment, not a result.
3. **Size the leaf logs instead of doubling them.** Most of the 5.47 B/extent between payload and as-built is capacity an append reserved and never used. Encode a split half into an exactly-sized log.
4. **Arena per generation.** One block per generation instead of one allocation per leaf removes the 1.66x churn at small `n` without changing the layout.
5. **Merge and delete**, the other half of the range-aware pair, if the publication path turns out to need per-extent removal.

## 8. Reproduce

Host of every number here: `go1.27.4`, `darwin/arm64`, 8 logical CPUs. The box was **oversubscribed** while these ran; `/proc/loadavg` is Linux-only, so the tool prints `load unavailable on darwin` rather than a number it cannot read. That is why the byte numbers are single values and the time numbers are spreads.

Repeated runs of the same binary, kept side by side, say what each half is worth:

* **Computed bytes are exact.** All ten byte-per-extent figures, all three H3 figures and every `ratio_rask_over_slice` came out **bit-identical** across runs.
* **The allocator is nearly exact.** All but one `alloc_ratio` figure were bit-identical too. The one that moved (1.66 → 1.56, `contiguous` at n=1,000, leaf 32) is the case where the denominator is ~50 B/ext, so a few kilobytes of runtime noise is a few percent. No claim rests on a difference under 10%.
* **Absolute time is not a measurement on this box.** Same binary, same seeds: the mean `ns/op` at leaf 1,024 ran between **1,733 and 14,004**, and from 64 extents per leaf up a single runaway run moved the mean by **6x-13x**. The **interleaved ratios** are what survive that, and only because both arms are sampled inside the same iteration; `score_table.md` §4b keeps the mean-based sweep as the counterexample.

```sh
go run ./tools/rask-bench -out docs/research/hip-1344-rask/raw_results.json   # every table + raw JSON, ~3 min
go run ./tools/rask-bench -no-timing                                          # the deterministic half only, ~20 s
go test -run TestMeasure -v ./internal/index/rask/                            # the byte table as a test
go test -race -count=1 ./internal/index/rask/                                 # correctness under -race
```

The percentile sweep takes `-samples` (default 20,000 per arm) and the mean sweep
takes `-reps`; the report is meant to be run three or four times and read as a
band, because that is what this host can support.

| File | What it is |
|---|---|
| `extent.go` | `Extent{Off uint64, Len uint32}`, shared by both sides |
| `slice.go` | the baseline: sorted array, binary search, `cap*16` |
| `index.go` | the prototype: leaves of gap/length varints behind a bounds array, split at the maximum |
| `workload.go` | the shapes, the probes and the allocation meter, shared by the tests and the tool |
| `index_test.go` | agreement with a model, out-of-order insert, invariants, a 2^50 span |
| `concurrent_test.go` | eight readers, `-race` |
| `measure_test.go` | the pre-registered hypothesis and the deterministic table |
| `doc.go` | package scope and the single-writer boundary |
