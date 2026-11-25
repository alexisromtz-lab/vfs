// Package rask is a prototype of the sparse-range index HIP-1344 asks for
// (issue #59, Bounty #2), compared against the shape the engine keeps today.
//
// The premise is the one RASK (Zhao et al., FAST '26) starts from: a sparse
// file is a list of ranges, not of offsets, and 16 bytes per range is a choice,
// not a requirement. Slice is the baseline in the engine's own terms — one
// array sorted by offset, binary search, cap(e)*16 bytes — and Index is the
// prototype: leaves of gap/length varints behind a bounds array, split in two
// when a leaf passes its maximum. Neither touches the persistent format-v2
// layout; this is an in-memory index study.
//
// The measurement, its hypothesis, its environment and its verdict are in
// docs/research/hip-1344-rask/, and tools/rask-bench reproduces them. It is a
// research deliverable: the numbers are what decides it, and a refuted
// hypothesis is one of the outcomes it reports.
package rask
