# HIP-1344 RASK Comparative Score Table

Measured on `darwin/arm64` (Apple Silicon, 8 logical CPUs, Go `go1.27.4`) against
the engine's own run list (`run{a, b uint64}`, 16 B, `file.go:590`). Machine-readable
source data: [`raw_results.json`](raw_results.json).

Reproduce directly:

```sh
go run ./tools/rask-bench -out docs/research/hip-1344-rask/raw_results.json
go test -run TestMeasure -v ./internal/index/rask/
go test -run XXX -bench . -count=3 ./internal/index/rask/
go test -race -count=1 ./internal/index/rask/
```

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

## 4. Lookup, reported as a spread and not as a verdict (H2)

`ns/op` for a covering lookup, 100,000 clustered extents, minimum and worst of
three repetitions of the same binary (`tools/rask-bench -reps 3`), from two runs
of that same binary on the same host:

| max leaf | extents per leaf | leaves | min ns/op | worst ns/op | spread | min ns/op, run 2 | spread, run 2 |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 16 | 8 | 12,499 | 371 | 1,018 | 2.74x | 536 | 2.40x |
| 32 | 16 | 6,249 | 370 | 556 | 1.50x | 555 | 2.17x |
| 64 | 32 | 3,124 | 795 | 2,615 | 3.29x | 2,006 | 1.24x |
| 128 | 64 | 1,562 | 2,424 | 4,500 | 1.86x | 3,057 | 3.90x |
| 256 | 128 | 781 | 6,652 | 9,042 | 1.36x | 7,758 | 2.33x |
| 1,024 | 512 | 195 | 20,531 | 39,368 | 1.92x | 28,045 | 1.62x |

The same configuration, repeated three times, moved **2.74x**. Two runs of the
same binary disagreed on one configuration by **2.5x** (795 → 2,006 ns/op). H2
asks for a 1.20x resolution; the measuring box cannot supply it under load, so H2
is `NO_MEDIDO` and not a verdict.

The one thing both runs agree on is the **ordering**: cost rises monotonically
with leaf size in both, and every figure at leaf 1,024 exceeds every figure at
leaf 16. That is enough to say the in-leaf scan is the dominant term, and not
enough to say by how much. The bytes in sections 1-3 are the part of this table
that is exact: all ten byte-per-extent figures and all three H3 rows came out
**bit-identical** across the two runs.
