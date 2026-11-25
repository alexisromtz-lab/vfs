# RASK prototype: compact sparse-range indexing, measured

Refs #59, HIP-1344. The code is `internal/rask`; this file is the study, its
hypothesis, its numbers and its verdict. It is a research deliverable, so one of
the outcomes it is allowed to report is a refuted hypothesis, and one of them is
reported below.

## Verdict

**The encoding wins; the prototype's allocator eats the win, and the lookup
numbers from this host are not measurements.**

1. **Per extent, the payload is 4.12 bytes against the baseline's 16** — 0.26x —
   on extents written in bursts, and 7.10 bytes (0.44x) when every offset is
   random. A run that continues where the last one ended costs one byte of gap
   and one or two of length; the sixteen bytes of a `run{a, b uint64}` are a
   choice, and they are not needed to find a covering range.
2. **As built, the index is 9.59 bytes per extent (0.51x), not 4.12.** The
   5.47 bytes in between are per-leaf bookkeeping and the capacity an append
   left unused, not encoding. The 40% target is therefore **refuted as built**
   for the pattern this prototype is for; it holds only where extents never gap
   (0.34x). The gap is attributed and closable, and closing it is the next step,
   not this PR.
3. **Lookup time is `NO_MEDIDO` on this host.** Repeating one configuration
   three times gave 1186, 1397 and 3249 ns/op for the *same* baseline binary —
   a 2.7x spread inside one point. That is larger than the 1.20x the hypothesis
   has to resolve, so the timings below are published as a spread with the
   environment that produced them, and no verdict is claimed from them.
4. **The cost follows the number of extents, not the span** (0.34: 97,720 ->
   101,048 bytes, +3.4%, when the whole file is moved from offset 0 to 2^50 with
   every gap unchanged, while the baseline stays at 180,224).

## 1. Paper and license audit

RASK — Zhao et al., FAST '26, USENIX open access. The design this prototype
follows: a range is the key, not the offset; each leaf keeps a log of
gap/length pairs; the leaf's last extent bounds the search key; a leaf that
passes its maximum splits in two, so neither the search nor the scan degrades.

The paper's USENIX page links no source artifact, so this is a **clean-room
implementation from the prose**: nothing is copied, no dependency is added, and
nothing is redistributed. `hanzoai/vfs` is Apache-2.0 and so is this package.
The paper's numbers are the authors' and are not restated as expectations here;
every figure in this file was produced by the code below on the host declared in
section 6. A patent search was **not** performed — `NO_MEDIDO`, not "none".

## 2. Matched baselines

`Slice` is the baseline in the engine's own terms, in the same package, built
from the same `[]Extent` on the same run: one array sorted by offset, binary
search for the covering entry, `cap(e)*16` bytes. That models `run{a, b uint64}`
(`file.go:590`) — 16 bytes per run, which is what the engine keeps today. The
persistent record is fatter (`extent{off, n, id}`, `record.go:235`, 40 bytes with
the chunk id) and is not what is compared: the id is payload the index does not
carry on either side.

Both structures are built by the same loop over the same slice, on the same
machine, in one process. There is no upstream baseline to match: the paper ships
no artifact, so the honest baseline is the engine's current shape.

## 3. Falsifiable hypothesis, stated before the numbers were read

> **H1** at 100,000 extents written in bursts, the index costs at most 40% of
> the baseline's bytes per extent.
> **H2** a covering lookup stays within 1.20x of the baseline's cost.
> **H3** the cost follows the number of extents, not the logical span.

Failure modes declared up front: the per-leaf fixed cost can exceed the encoding
win at small `n`; the in-leaf scan is linear in the leaf, so a large leaf size
buys memory and pays latency in the same move; and the base offset enters the
first gap of every leaf in absolute form, so a wide file must cost a few more
bytes per leaf.

## 4. Deliverable

`internal/rask`, self-contained, no change to the frozen format-v2 layout and no
new dependency:

| file | what it is |
|---|---|
| `extent.go` | `Extent{Off uint64, Len uint32}`, shared by both sides |
| `slice.go` | the baseline: sorted array, binary search |
| `index.go` | the prototype: leaves of gap/length varints behind a bounds array, split at the maximum |
| `index_test.go` | agreement with a model, out-of-order insert, invariants, a 2^50 span |
| `concurrent_test.go` | eight readers, `-race` |
| `measure_test.go` | the pre-registered hypothesis and the table below |

## 5. Evidence and safety

- **Correctness**: every extent the index answers is compared with a linear-scan
  model, over random, burst and adversarial shapes, plus out-of-order inserts.
- **Invariants**: bounds strictly ascending, every leaf within `[8, max]`, and
  the decoded entries equal to the sorted input.
- **Concurrency**: `Index` takes no lock. It is written once and published, like
  the engine's runs; eight concurrent readers are checked against the model
  under `-race`. A concurrent *writer* is out of scope and would corrupt the log
  — that is the boundary of the claim, not an oversight.
- **Isolation and format**: an in-memory study; no file, no key, no tenant
  boundary, and no byte of the persistent format is touched. There is no
  regression surface to test because no existing path changes.
- **Security**: no parsing of untrusted bytes, no allocation sized from an
  attacker's field. The one input assumption is the engine's own: sorted,
  disjoint, non-touching runs.

## 6. Measurements

Host: Go 1.27.4, darwin/arm64, 8 logical CPUs, load average 14-23 while these
ran — the box was oversubscribed roughly 3x. That is why the time numbers are a
spread and the byte numbers are a single value: bytes are deterministic and do
not care about the scheduler.

Reproduce:

```
go test -run TestMeasure -v ./internal/rask/     # the tables below
go test -run XXX -bench . -count=3 ./internal/rask/   # times, on a quiet host
go test -race -count=1 ./internal/rask/          # correctness under -race
```

### Bytes per extent, capacity included (`Index.Bytes`, `Slice.Bytes`)

`payload` is the encoded extent bytes alone — the floor the encoding reaches,
and the line that separates the encoding's win from the allocator's cost.

| shape | n | max leaf | leaves | baseline B/ext | rask B/ext | ratio | payload B/ext |
|---|---|---|---|---|---|---|---|
| contiguous | 1,000 | 64 | 31 | 20.48 | 5.98 | 0.29 | 3.91 |
| contiguous | 10,000 | 64 | 312 | 18.02 | 6.42 | 0.36 | 3.92 |
| contiguous | 100,000 | 64 | 3,124 | 18.84 | 6.31 | **0.34** | 3.94 |
| clustered | 1,000 | 64 | 31 | 20.48 | 9.18 | 0.45 | 4.10 |
| clustered | 10,000 | 64 | 312 | 18.02 | 9.76 | 0.54 | 4.12 |
| clustered | 100,000 | 64 | 3,124 | 18.84 | 9.59 | **0.51** | 4.12 |
| clustered | 100,000 | 32 | 6,249 | 18.84 | 11.48 | 0.61 | 4.19 |
| uniform | 1,000 | 64 | 31 | 20.48 | 15.83 | 0.77 | 8.04 |
| uniform | 10,000 | 64 | 312 | 18.02 | 10.16 | 0.56 | 7.78 |
| uniform | 100,000 | 64 | 3,124 | 18.84 | 9.85 | **0.52** | 7.10 |

Readings. `contiguous` is the floor of the comparison — no gaps at all, so a
gap is one byte and 16 bytes of run become four. `clustered` is the pattern this
prototype exists for: seven extents in eight continue the previous one. `uniform`
is the adversary: every offset random, so every gap is a full six-byte varint and
the win shrinks to half. A split leaves the halves at half the maximum, which is
why `max 32` holds 16 extents per leaf and costs more per extent than `max 64`.

### Allocation pressure (`TotalAlloc` delta against a control build at n=0)

| shape | n | baseline B/ext | rask B/ext (max 64) | ratio |
|---|---|---|---|---|
| contiguous | 100,000 | 89.43 | 66.23 | 0.74 |
| clustered | 1,000 | 50.42 | 83.58 | 1.66 |
| clustered | 100,000 | 89.43 | 87.86 | 0.98 |
| uniform | 100,000 | 89.43 | 87.89 | 0.98 |

At 100,000 extents the prototype allocates about what the baseline allocates;
at 1,000 it allocates 1.66x more, because one header and one log per leaf is a
lot of small allocations relative to 31 of them. The live structure is where the
win is, and the allocator is what stands between the win and the process.

### Span, with the gaps unchanged (H3)

10,000 clustered extents, only the base offset moved:

| base offset | rask bytes | baseline bytes |
|---|---|---|
| 0 | 97,720 | 180,224 |
| 1 GiB | 98,488 | 180,224 |
| 1 PiB | 101,048 | 180,224 |

+3.4% for a 2^20 move of the whole file: the absolute first gap of each leaf
grows from one byte to six, and nothing else does. The baseline is flat because
it never compressed anything.

### Lookup, as a spread rather than as a verdict (H2)

`ns/op`, minimum and maximum of three repeats of the same binary, 300,000
operations each, 9 hits to 1 hole:

| shape | n | baseline min-max | rask min-max | min ratio |
|---|---|---|---|---|
| clustered | 1,000 | 248 - 1,375 | 978 - 1,844 | 3.9x |
| clustered | 100,000 | 1,186 - 3,249 | 1,279 - 3,114 | 1.08x |
| uniform | 1,000 | 143 - 519 | 1,121 - 1,567 | 7.8x |
| uniform | 100,000 | 922 - 1,864 | 2,595 - 6,572 | 2.8x |

The same baseline configuration, repeated three times, moved 2.7x. H2 asks for a
1.20x resolution; this host cannot supply it, so nothing is concluded from the
column above. What the numbers do show, monotone and reproducible in shape, is
where the prototype's time goes — the leaf size, with 100,000 clustered extents:

| max leaf | extents per leaf | leaves | ns/op (min of 3) |
|---|---|---|---|
| 16 | 8 | 12,499 | 533 |
| 32 | 16 | 6,249 | 378 |
| 64 | 32 | 3,124 | 530 |
| 128 | 64 | 1,562 | 937 |
| 256 | 128 | 781 | 1,411 |
| 1,024 | 512 | 195 | 6,517 |

Flat until a leaf holds about 32 extents, then linear at roughly 6 ns per entry:
that is the in-leaf scan, and it is the term an in-leaf fence would remove.

## 7. Status against HIP-1344

The objective's three axes, as measured here:

- **Memory per extent**: the encoding reaches 0.26x of the baseline on burst
  written extents; the prototype as built reaches 0.51x. The 40% target is met
  only when extents do not gap.
- **Lookup p99**: not measured. The hypothesis stands and the instrument for it
  is in the package; it needs a host whose own noise is under 20%.
- **1 PiB sparse behavior**: the index side holds — 5 extents over 2^50, under
  4 KiB of structure, lookups at both ends of the file, and the cost moves 3.4%
  when the file moves. The file side of that test (`TestPetabyteScattered`) reads
  `/proc/self/status` for RSS (`sparse_test.go:371`) and skips on darwin, so the
  engine-level petabyte behaviour is unchanged and unmeasured by this PR.

## 8. What closes the gap

1. **Size the leaf logs instead of doubling them.** Most of the 5.47 bytes per
   extent between payload and as-built is capacity an append reserved and never
   used. Encode a split half into an exactly-sized log and reuse freed leaves.
2. **Arena per generation.** One block per generation instead of one allocation
   per leaf removes the 1.66x churn at small `n` without changing the layout.
3. **In-leaf fence**, `(bytePos, off)` every eight extents at ~1.5 bytes per
   extent, which turns the scan into a bounded one — only worth its bytes if the
   leaf-size curve above is confirmed on a quiet host.
4. **Merge and delete**, the other half of the range-aware pair, which the
   engine's publication path may or may not need.
