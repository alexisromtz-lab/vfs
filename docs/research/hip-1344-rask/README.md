# HIP-1344: Compact Sparse-Range Indexing for `hanzoai/vfs` (RASK)

**Refs:** `hanzoai/vfs#59` · **Tracking:** HIP-1344 · **Implementation:** [`internal/index/rask/`](../../../internal/index/rask/), [`tools/rask-bench/main.go`](../../../tools/rask-bench/main.go) · **Numbers:** [`score_table.md`](score_table.md), [`raw_results.json`](raw_results.json)

A research bounty admits a documented negative result. One is reported here.

---

## Verdict first

1. **The encoding wins, and it is not close.** Per extent the payload is **4.12 B against the baseline's 16** (0.26x) on extents written in bursts, and 7.10 B (0.44x) when every offset is random. A run that continues where the last one ended costs one byte of gap plus one or two of length; the sixteen bytes of a `run{a, b uint64}` are a choice, and they are not needed to find a covering range.
2. **As built, the index is 9.59 B per extent (0.51x), not 4.12.** The 5.47 bytes in between are per-leaf bookkeeping and the capacity an append left unused — not encoding. **H1 (≤40%) is refuted as built** for the pattern this prototype exists for; it holds only where extents never gap (0.34x). The gap is attributed and closable, and closing it is the next step, not this submission.
3. **Lookup time is `NO_MEDIDO` on this host, and the instrument says so.** The same configuration repeated three times spanned **2.74x** (371 → 1,018 ns/op), and a second run of the *same binary* moved one configuration's minimum from 795 to 2,006 ns/op. H2 asks for 1.20x resolution; this box cannot supply it, so no timing verdict is claimed anywhere in this file.
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
> **H2** — a covering lookup stays within **1.20x** of the baseline's cost.
> **H3** — the cost follows the **number of extents**, not the logical span.

Declared failure modes, written before the run:

* the per-leaf fixed cost can **exceed** the encoding win at small `n`;
* the in-leaf scan is linear in the leaf, so a large leaf size buys memory and pays latency in the same move;
* the base offset enters the first gap of every leaf in absolute form, so a wide file must cost a few more bytes per leaf.

There is no H4 here: this submission pre-registered three hypotheses and reports the refutation of one, rather than inventing a fourth after seeing the numbers.

---

## 4. Results

Full tables in [`score_table.md`](score_table.md); machine-readable source in [`raw_results.json`](raw_results.json).

| Hypothesis | Prediction | Measured | Verdict |
|---|---|---|---|
| **H1** | rask ≤ 0.40x baseline B/ext at 100k clustered | 0.51x built, 0.26x payload | **refuted as built** |
| **H2** | lookup within 1.20x | 2.74x intra-run spread, 2.5x run-to-run drift | **`NO_MEDIDO`** |
| **H3** | cost tracks extents, not span | +3.4% across a 2^50 move | **holds** |

The decisive number for a maintainer is the difference between the first two: **the payload is 4.12 B per extent and the structure is 9.59 B.** 5.47 bytes of bookkeeping per extent stand between the encoding and the claim, and section 7 names each of them.

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
* **Timing is untested, not "fine".** H2 is `NO_MEDIDO`. Two runs of the same binary agree on one thing only — the cost rises monotonically with leaf size (371 / 795 / 2,424 / 6,652 / 20,531 ns and 536 / 2,006 / 3,057 / 7,758 / 28,045 ns). They disagree on everything else, by up to 2.5x. A host whose own noise is under 20% is required before any 1.20x claim can be made.
* **The engine-level petabyte test does not run here.** `TestPetabyteScattered` reads `/proc/self/status` for RSS (`sparse_test.go:371`) and skips on darwin. The engine's petabyte behaviour is unchanged and unmeasured by this submission.
* **Merge and delete are not exercised.** The prototype has no per-extent delete: the engine punches holes by rewriting runs at publication, not by removing one extent from the index.

---

## 7. Integration Recommendation, and What Closes the Gap

**Recommendation: do not merge the structure as built; adopt the encoding, sized correctly.** The `(gap, length)` varint log is the part that is ready — 0.26x-0.44x of the current per-extent bytes with no format change. What is *not* ready is the allocator around it, and the four items below are ordered by bytes recovered per line of code:

1. **Size the leaf logs instead of doubling them.** Most of the 5.47 B/extent between payload and as-built is capacity an append reserved and never used. Encode a split half into an exactly-sized log.
2. **Arena per generation.** One block per generation instead of one allocation per leaf removes the 1.66x churn at small `n` without changing the layout.
3. **In-leaf fence** — `(bytePos, off)` every eight extents at roughly 1.5 B/extent (an arithmetic estimate from an 8-byte entry every 8 extents, not a measurement), which turns the scan into a bounded one. Only worth its bytes once the leaf-size curve is confirmed on a quiet host.
4. **Merge and delete**, the other half of the range-aware pair, if the publication path turns out to need per-extent removal.

---

## 8. Reproduce

Host of every number here: `go1.27.4`, `darwin/arm64`, 8 logical CPUs. The box was **oversubscribed** while these ran; `/proc/loadavg` is Linux-only, so the tool prints `load unavailable on darwin` rather than a number it cannot read. That is why the byte numbers are single values and the time numbers are spreads.

Two runs of the same binary, kept side by side, say what each half is worth:

* **Computed bytes are exact.** All ten byte-per-extent figures, all three H3 figures and every `ratio_rask_over_slice` came out **bit-identical** across both runs.
* **The allocator is nearly exact.** Eighteen of nineteen `alloc_ratio` figures were bit-identical too. The one that moved (1.66 → 1.56, `contiguous` at n=1,000, leaf 32) is the case where the denominator is ~50 B/ext, so a few kilobytes of runtime noise is a few percent. No claim rests on a difference under 10%.
* **Time is not a measurement on this box.** Same binary, same seeds: 795 → 2,006 ns/op for one configuration. Only the monotone ordering survives.

```sh
go run ./tools/rask-bench -out docs/research/hip-1344-rask/raw_results.json   # both tables + raw JSON
go test -run TestMeasure -v ./internal/index/rask/                            # the deterministic table
go test -run XXX -bench . -count=3 ./internal/index/rask/                     # timings, on a quiet host
go test -race -count=1 ./internal/index/rask/                                 # correctness under -race
```

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
