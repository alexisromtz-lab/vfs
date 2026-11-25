# HIP-1344 RASK Comparative Score Table

Measured on `darwin/arm64` (Apple Silicon, 8 logical CPUs, Go `go1.27.4`) against
the engine's own run list (`run{a, b uint64}`, 16 B, `file.go:590`). Machine-readable
source data: [`raw_results.json`](raw_results.json).

Reproduce directly:

```sh
go run ./tools/rask-bench -out docs/research/hip-1344-rask/raw_results.json   # all six tables, ~3 min
go run ./tools/rask-bench -no-timing                                          # the deterministic half, ~20 s
go test -run TestMeasure -v ./internal/index/rask/
go test -race -count=1 ./internal/index/rask/
```

Sections 1-3 are exact and reproduce bit-identically. Sections 4 and 4b are
timings on a shared box, and every one of them is reported as a band across runs
rather than as a single value.

## 1. Bytes per extent (`Index.Bytes`, `Slice.Bytes`), capacity included

`payload` is the encoded extent bytes alone — the floor the encoding reaches, and
the column that separates the encoding's win from the allocator's cost.

| Shape | n | max leaf | leaves | baseline B/ext | rask B/ext | ratio | payload B/ext |
|---|---:|---:|---:|---:|---:|---:|---:|
| `contiguous` | 1,000 | 64 | 31 | 20.48 | 5.98 | 0.29 | 3.91 |
| `contiguous` | 10,000 | 64 | 312 | 18.02 | 6.42 | 0.36 | 3.92 |
| `contiguous` | 100,000 | 64 | 3,124 | 18.84 | **6.31** | **0.34** | 3.94 |
| `clustered` | 1,000 | 64 | 31 | 20.48 | 9.18 | 0.45 | 4.10 |
| `clustered` | 10,000 | 64 | 312 | 18.02 | 9.76 | 0.54 | 4.12 |
| `clustered` | 100,000 | 64 | 3,124 | 18.84 | **9.59** | **0.51** | 4.12 |
| `clustered` | 100,000 | 32 | 6,249 | 18.84 | 11.48 | 0.61 | 4.19 |
| `uniform` | 1,000 | 64 | 31 | 20.48 | 15.83 | 0.77 | 8.04 |
| `uniform` | 10,000 | 64 | 312 | 18.02 | 10.16 | 0.56 | 7.78 |
| `uniform` | 100,000 | 64 | 3,124 | 18.84 | **9.85** | **0.52** | 7.10 |

`contiguous` is the floor of the comparison — no gaps at all, so a gap costs one
byte and a 16-byte run becomes four. `clustered` is the pattern this prototype
exists for: seven extents in eight continue the previous one. `uniform` is the
adversary: every offset random, so every gap is a full six-byte varint and the
win shrinks to half. A split leaves the halves at half the maximum, which is why
`max 32` holds 16 extents per leaf and costs more per extent than `max 64`.

## 2. Allocation pressure (`TotalAlloc` delta against a control build at n=0)

| Shape | n | max leaf | baseline B/ext | rask B/ext | ratio |
|---|---:|---:|---:|---:|---:|
| `contiguous` | 100,000 | 64 | 89.43 | 66.23 | 0.74 |
| `clustered` | 1,000 | 64 | 50.42 | 83.58 | **1.66** |
| `clustered` | 100,000 | 64 | 89.43 | 87.86 | 0.98 |
| `uniform` | 100,000 | 64 | 89.43 | 87.89 | 0.98 |

At 100,000 extents the prototype allocates about what the baseline allocates; at
1,000 it allocates 1.66x **more**, because one header and one log per leaf is a
lot of small allocations relative to 31 of them. The live structure is where the
win is; the allocator is what stands between the win and the process.

## 3. Span, with the gaps unchanged (H3)

10,000 clustered extents, only the base offset moved:

| Base offset | rask bytes | baseline bytes |
|---|---:|---:|
| 0 | 97,720 | 180,224 |
| 1 GiB | 98,488 | 180,224 |
| 1 PiB (2^50) | 101,048 | 180,224 |

+3.4% for moving the whole file 2^50 bytes out: the absolute first gap of each
leaf grows from one byte to six, and nothing else does. The baseline is flat
because it never compressed anything.

## 4. Lookup, p99 first (H2)

The Acceptance Test asks for **lookup p99**, so that is what is measured: one call
at a time, `time.Now()` around each, the baseline and the index sampled **inside
the same iteration** with the order alternating, 20,000 samples per arm per leaf
size, 100,000 clustered extents. Interleaving is the point of the design, not a
detail: the host's noise lands on both arms of a pair, so the **ratio** survives
conditions that make an absolute `ns/op` worthless.

That claim is measured too. Four runs of the same binary, same seeds:

| max leaf | extents/leaf | leaves | p99 ratio, overhead removed, 4 runs | band | vs 1.20x |
|---:|---:|---:|---|---:|---|
| 16 | 8 | 12,499 | 1.13 / 1.27 / 1.05 / 1.27 | 1.05-1.27 | straddles |
| 32 | 16 | 6,249 | 1.22 / 1.15 / 1.11 / 0.88 | 0.88-1.22 | straddles, one run below 1.0 |
| 64 | 32 | 3,124 | 1.37 / 1.41 / 1.23 / 1.13 | 1.13-1.41 | over in 3 of 4 |
| 128 | 64 | 1,562 | 2.02 / 2.05 / 1.76 / 1.81 | 1.76-2.05 | **over in all 4** |
| 256 | 128 | 781 | 3.69 / 3.18 / 2.90 / 2.74 | 2.74-3.69 | **over in all 4** |
| 1,024 | 512 | 195 | 9.65 / 9.46 / 9.81 / 6.28 | 6.28-9.81 | **over in all 4** |

The fourth column of each row is the run recorded in `raw_results.json`, so a
reader can check one of the four straight from the tree; the other three are
transcribed runs of the same binary on the same host. The four largest leaf
sizes are two orders of magnitude apart from the noise; the three smallest are
inside it, which is why they are reported as bands and not as ratios.

"Overhead removed" is the two `time.Now()` calls inside every sample, measured on
the same host at **40-84 ns** and present as a constant in both arms; subtracting
it is the only way to read a 1.20x threshold without flattering it, and it moves
the ratio *up*, never down.

**Verdict: H2 is refuted from 64 extents per leaf up, and unresolved below it.**

* At 64 extents per leaf (max leaf 128) the index is **1.76x to 2.05x** the
  baseline's p99, and at 512 it is **6.28x to 9.81x**. Over 1.20x in all four
  runs with margin: a refutation, not noise.
* At 32 extents per leaf -- the configuration with the best byte ratio, 0.51x --
  the band is **1.13-1.41**: over the threshold in three runs of four and under
  it in the fourth. That is not a refutation and is not reported as one.
* At 8-16 extents per leaf the bands are **1.05-1.27** and **0.88-1.22**, so the
  threshold sits inside both and H2 is not decided there either -- one run at 16
  extents per leaf actually came in *below* the baseline. Those are also the leaf
  sizes where the byte win is **worst** (0.61x against 0.51x).
* So there is no operating point that satisfies both halves: shrinking the leaf
  until p99 is near parity gives back a fifth of the byte advantage, and the byte
  ratio is best exactly where the p99 ratio is worst.

The **median also loses, at every leaf size**: p50 ratio 1.25-1.79 at 8 extents
per leaf, 1.34-1.60 at 16, 1.67-1.99 at 32, and 8.01-10.23 at 512, with the same
four runs. Unlike p99, the median never crosses 1.0 at any leaf size. That is the
other half of the finding. Binary search over 100,000 contiguous runs is the
shape a CPU is built for; a two-level structure with a scan inside the leaf is
not, and the encoding that saves 9.5 bytes per extent costs a branch per entry.

## 4c. The `uniform` shape, measured instead of assumed

The byte table runs all three shapes; the p99 sweep initially ran only
`clustered`, and section 6 of the README declared `uniform` `NO_MEDIDO` in time
rather than extrapolate it. `tools/rask-bench -shapes clustered,uniform` closes
that cell. One run, 100,000 uniform extents, same instrument:

| max leaf | extents/leaf | p99 ratio net, uniform | p99 ratio net, clustered (same run) | p50 ratio, uniform | p50 ratio, clustered |
|---:|---:|---:|---:|---:|---:|
| 16 | 8 | 1.23 | 1.27 | 1.51 | 1.25 |
| 32 | 16 | 1.20 | 0.88 | 1.66 | 1.34 |
| 64 | 32 | 1.25 | 1.13 | 2.00 | 1.67 |
| 128 | 64 | 1.55 | 1.81 | 2.67 | 2.67 |
| 256 | 128 | 2.96 | 2.74 | 4.00 | 4.00 |
| 1,024 | 512 | 9.94 | 6.28 | 13.66 | 10.23 |

Two things follow, and one of them is a retraction of a guess. Random offsets
are **worse in the median at every leaf size** (1.51-13.66 against 1.25-10.23),
which is the expected direction: an uncorrelated probe pays a longer in-leaf scan.
But on the *p99 tail* the two shapes are within noise of each other below 64
extents per leaf, and `uniform` is **better** at 64 (1.55 against 1.81). The
guess that `uniform` would be worse on the tail is not supported: the tail is
dominated by cache and scheduler effects that the shape does not control. One run
is one run, so this is recorded as a direction to check, not a ratio to quote.

## 4b. The same question asked badly, for the record

The same sweep with `testing.Benchmark` — one mean over a loop of millions of
calls — from four runs of the same binary:

| max leaf | min `ns/op`, four runs | spread |
|---:|---|---:|
| 16 | 142 / 145 / 190 / 98 | 1.9x |
| 32 | 168 / 165 / 212 / 119 | 1.8x |
| 64 | 253 / 268 / 327 / 152 | 2.2x |
| 128 | 348 / 373 / 2,159 / 263 | **8.2x** |
| 256 | 618 / 605 / 7,240 / 428 | **16.9x** |
| 1,024 | 1,733 / 2,443 / 14,004 / 1,550 | **9.0x** |

Same binary, same seeds, same host. At the three smallest leaf sizes the mean
still moves 1.8x-2.2x; from 64 extents per leaf up a single runaway run appears
and the `ns/op` moves by **8x to 17x**. A mean over a loop of millions of
iterations does not average that noise away; it absorbs the noise of whichever
ten-second window it happened to run in, which is why one bad run is enough to
invert a conclusion drawn from it. Note also that these minimums are *lower*
than the earlier runs recorded here: the box was quieter. That is the point --
the number tracks the box, not the structure. This is why section 4 reports
interleaved like-for-like percentiles and section 4b reports nothing.
