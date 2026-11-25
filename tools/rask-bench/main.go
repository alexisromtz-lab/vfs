// Copyright 2026 Hanzo AI Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

// Command rask-bench reproduces the RASK measurement of HIP-1344 (Refs #59)
// and writes the raw numbers the report quotes.
//
//	go run ./tools/rask-bench
//	go run ./tools/rask-bench -out docs/research/hip-1344-rask/raw_results.json
//
// The memory half is deterministic: the same shapes, the same seeds and the
// same leaf sizes give the same bytes per extent on every run and every
// machine. The timing half is not, and says so: it reports the minimum of
// -reps repetitions, because the measuring box was oversubscribed and the
// spread between repetitions of one binary reached 2.7x. A timing difference
// under 2x is not claimed anywhere.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hanzoai/vfs/internal/index/rask"
)

type row struct {
	Shape      string  `json:"shape"`
	N          int     `json:"n"`
	MaxLeaf    int     `json:"max_leaf"`
	Leaves     int     `json:"leaves"`
	SliceBytes int     `json:"slice_bytes"`
	RaskBytes  int     `json:"rask_bytes"`
	RaskPayld  int     `json:"rask_payload_bytes"`
	BytesPerEx float64 `json:"bytes_per_extent_slice"`
	RaskPerEx  float64 `json:"bytes_per_extent_rask"`
	Ratio      float64 `json:"ratio_rask_over_slice"`
	PayloadPer float64 `json:"payload_per_extent"`
	AllocSlice float64 `json:"alloc_bytes_per_extent_slice"`
	AllocRask  float64 `json:"alloc_bytes_per_extent_rask"`
	AllocRatio float64 `json:"alloc_ratio_rask_over_slice"`
}

// sink keeps the timed lookups from being optimised away. One probe in ten is
// a deliberate hole, so the result is discarded rather than asserted.
var sink rask.Extent

type h3row struct {
	Base       uint64 `json:"base_offset"`
	Extents    int    `json:"extents"`
	RaskBytes  int    `json:"rask_bytes"`
	SliceBytes int    `json:"slice_bytes"`
}

type leafrow struct {
	MaxLeaf  int     `json:"max_leaf"`
	Leaves   int     `json:"leaves"`
	NsPerOp  float64 `json:"lookup_ns_min"`
	SpreadNs float64 `json:"lookup_ns_max"`
	Reps     int     `json:"reps"`
}

// pctrow is the lookup distribution of the two structures sampled in the same
// window. A percentile from interleaved single-call samples is not the same
// measurement as a mean over a loop: the host's noise lands in both arms of each
// pair, so the ratio survives conditions that make an absolute ns/op worthless.
type pctrow struct {
	Shape       string  `json:"shape"`
	MaxLeaf     int     `json:"max_leaf"`
	Leaves      int     `json:"leaves"`
	Samples     int     `json:"samples"`
	SliceP50    float64 `json:"slice_p50_ns"`
	SliceP90    float64 `json:"slice_p90_ns"`
	SliceP99    float64 `json:"slice_p99_ns"`
	RaskP50     float64 `json:"rask_p50_ns"`
	RaskP90     float64 `json:"rask_p90_ns"`
	RaskP99     float64 `json:"rask_p99_ns"`
	RatioP50    float64 `json:"ratio_p50"`
	RatioP90    float64 `json:"ratio_p90"`
	RatioP99    float64 `json:"ratio_p99"`
	RatioP99Net float64 `json:"ratio_p99_net"`
	OverheadNs  float64 `json:"clock_overhead_ns"`
}

type report struct {
	Benchmark   string    `json:"benchmark"`
	HIP         string    `json:"hip"`
	Issue       string    `json:"issue"`
	Timestamp   string    `json:"timestamp_utc"`
	GoVersion   string    `json:"go_version"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	NumCPU      int       `json:"num_cpu"`
	LoadAverage string    `json:"load_average"`
	Note        string    `json:"note"`
	Results     []row     `json:"results"`
	LargeSpan   []h3row   `json:"h3_base_sweep"`
	LeafSize    []leafrow `json:"leaf_size_sweep"`
	Percentiles []pctrow  `json:"lookup_percentiles"`
}

func main() {
	out := flag.String("out", "", "write the raw JSON here instead of printing a table")
	asJSON := flag.Bool("json", false, "emit the JSON report on stdout")
	reps := flag.Int("reps", 3, "timing repetitions; the minimum is reported")
	noTiming := flag.Bool("no-timing", false, "skip the timing sweeps (deterministic only)")
	samples := flag.Int("samples", 20000, "single-call samples per arm in the percentile sweep")
	shapes := flag.String("shapes", "clustered", "comma-separated extent shapes for the percentile sweep")
	flag.Parse()

	rep := report{
		Benchmark:   "hanzoai/vfs RASK compact sparse-range index",
		HIP:         "HIP-1344",
		Issue:       "hanzoai/vfs#59",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		NumCPU:      runtime.NumCPU(),
		LoadAverage: loadAverage(),
		Note:        "Memory is deterministic (TotalAlloc over an n=0 control). Percentiles are single-call samples interleaved against the baseline; the absolute ns are not a measurement on a shared host, the ratios are. leaf_size_sweep is the mean-over-a-loop estimator, kept as evidence of what this host does to it.",
	}

	for _, shape := range []string{"contiguous", "clustered", "uniform"} {
		for _, n := range []int{1000, 10000, 100000} {
			es := rask.Extents(n, shape, rand.New(rand.NewPCG(uint64(n), 42)))
			s := rask.BuildSlice(es)
			as := rask.Allocated(func(v []rask.Extent) any { return rask.BuildSlice(v) }, es)
			for _, maxLeaf := range []int{32, 64} {
				x := rask.BuildIndex(es, maxLeaf)
				ax := rask.Allocated(func(v []rask.Extent) any { return rask.BuildIndex(v, maxLeaf) }, es)
				rep.Results = append(rep.Results, row{
					Shape: shape, N: n, MaxLeaf: maxLeaf, Leaves: x.Leaves(),
					SliceBytes: s.Bytes(), RaskBytes: x.Bytes(), RaskPayld: x.Payload(),
					BytesPerEx: float64(s.Bytes()) / float64(n),
					RaskPerEx:  float64(x.Bytes()) / float64(n),
					Ratio:      float64(x.Bytes()) / float64(s.Bytes()),
					PayloadPer: float64(x.Payload()) / float64(n),
					AllocSlice: float64(as) / float64(n),
					AllocRask:  float64(ax) / float64(n),
					AllocRatio: float64(ax) / float64(as),
				})
			}
		}
	}

	// H3: moving the whole file out must not move the cost. Same n, same gaps,
	// only the base offset changes; the first gap of each leaf is absolute, so
	// it is the one thing that may grow.
	small := rask.Extents(10000, "clustered", rand.New(rand.NewPCG(7, 8)))
	for _, base := range []uint64{0, 1 << 30, rask.Pib} {
		shifted := make([]rask.Extent, len(small))
		for i, e := range small {
			shifted[i] = rask.Extent{Off: e.Off + base, Len: e.Len}
		}
		rep.LargeSpan = append(rep.LargeSpan, h3row{
			Base: base, Extents: len(small),
			RaskBytes:  rask.BuildIndex(shifted, 64).Bytes(),
			SliceBytes: rask.BuildSlice(shifted).Bytes(),
		})
	}

	if !*noTiming {
		es := rask.Extents(100000, "clustered", rand.New(rand.NewPCG(100000, 42)))
		q := rask.Probes(es, 4096, rand.New(rand.NewPCG(100000, 43)))
		for _, maxLeaf := range []int{16, 32, 64, 128, 256, 1024} {
			x := rask.BuildIndex(es, maxLeaf)
			lo, hi := -1.0, 0.0
			for i := 0; i < *reps; i++ {
				res := testing.Benchmark(func(b *testing.B) {
					for i := 0; i < b.N; i++ {
						sink, _ = x.Lookup(q[i&4095])
					}
				})
				ns := float64(res.NsPerOp())
				if lo < 0 || ns < lo {
					lo = ns
				}
				if ns > hi {
					hi = ns
				}
			}
			rep.LeafSize = append(rep.LeafSize, leafrow{
				MaxLeaf: maxLeaf, Leaves: x.Leaves(), NsPerOp: lo, SpreadNs: hi, Reps: *reps,
			})
		}
	}

	// The Acceptance Test names lookup p99, and a mean over a loop cannot answer
	// it. Sample one call at a time, alternating which arm goes first so a slow
	// scheduler tick cannot be charged to the same side twice, and report the
	// ratio of like percentiles: the host's noise is common to both arms of a
	// pair and cancels in the ratio, which is what H2 asks about.
	if !*noTiming {
		for _, shape := range strings.Split(*shapes, ",") {
			shape = strings.TrimSpace(shape)
			es := rask.Extents(100000, shape, rand.New(rand.NewPCG(100000, 42)))
			q := rask.Probes(es, 4096, rand.New(rand.NewPCG(100000, 43)))
			sl := rask.BuildSlice(es)
			for _, maxLeaf := range []int{16, 32, 64, 128, 256, 1024} {
				x := rask.BuildIndex(es, maxLeaf)
				rep.Percentiles = append(rep.Percentiles, sampleLookups(shape, sl, x, q, *samples, maxLeaf))
			}
		}
	}

	if *asJSON || *out != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "rask-bench: encode: %v\n", err)
			os.Exit(1)
		}
		b = append(b, '\n')
		if *out != "" {
			if err := os.WriteFile(*out, b, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "rask-bench: write %s: %v\n", *out, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "rask-bench: wrote %s (%d bytes)\n", *out, len(b))
		}
		if *asJSON {
			os.Stdout.Write(b)
			return
		}
	}

	printTable(&rep, *reps)
}

// sampleLookups times one lookup per structure per step and returns the
// percentiles of both arms. The order alternates, so neither structure is
// systematically the one that pays for a scheduler tick.
func sampleLookups(shape string, sl *rask.Slice, x *rask.Index, q []uint64, n, maxLeaf int) pctrow {
	sSlice := make([]float64, 0, n)
	sRask := make([]float64, 0, n)
	overhead := clockOverhead()

	for i := 0; i < n; i++ {
		off := q[i%len(q)]
		// Both arms are timed inside one iteration, and which goes first
		// alternates: a scheduler tick hits one arm per pair, not one structure.
		var tr, ts float64
		if i&1 == 0 {
			tr, ts = timeIndex(x, off), timeSlice(sl, off)
		} else {
			ts, tr = timeSlice(sl, off), timeIndex(x, off)
		}
		sRask = append(sRask, tr)
		sSlice = append(sSlice, ts)
	}

	row := pctrow{
		Shape: shape, MaxLeaf: maxLeaf, Leaves: x.Leaves(), Samples: n, OverheadNs: overhead,
		SliceP50: percentile(sSlice, 50), SliceP90: percentile(sSlice, 90), SliceP99: percentile(sSlice, 99),
		RaskP50: percentile(sRask, 50), RaskP90: percentile(sRask, 90), RaskP99: percentile(sRask, 99),
	}
	row.RatioP50 = row.RaskP50 / row.SliceP50
	row.RatioP90 = row.RaskP90 / row.SliceP90
	row.RatioP99 = row.RaskP99 / row.SliceP99
	// Every sample carries two time.Now() calls, and that cost is the same
	// constant in both arms, so it pulls the ratio toward 1. Subtracting it is
	// the only way to read the threshold without flattering it.
	row.RatioP99Net = net(row.RaskP99, row.SliceP99, overhead)
	return row
}

// timeIndex and timeSlice time one lookup of each arm, in nanoseconds. Both read
// the same offset, so an iteration compares two answers to the same question.
func timeIndex(x *rask.Index, off uint64) float64 {
	t0 := time.Now()
	e, ok := x.Lookup(off)
	t1 := time.Now()
	sink, _ = e, ok
	return float64(t1.Sub(t0).Nanoseconds())
}

func timeSlice(sl *rask.Slice, off uint64) float64 {
	t0 := time.Now()
	e, ok := sl.Lookup(off)
	t1 := time.Now()
	sink, _ = e, ok
	return float64(t1.Sub(t0).Nanoseconds())
}

// net removes the shared constant from both arms of a ratio. It is a floor, not
// a correction with an error bar: the overhead is measured, not modelled.
func net(a, b, overhead float64) float64 {
	if a-overhead < 1 {
		a = overhead + 1
	}
	if b-overhead < 1 {
		b = overhead + 1
	}
	return (a - overhead) / (b - overhead)
}

// clockOverhead measures what two time.Now() calls cost here, because that cost
// is inside every sample and sets the floor below which no difference is real.
func clockOverhead() float64 {
	const rounds = 10000
	var best = math.MaxFloat64
	for r := 0; r < 5; r++ {
		t0 := time.Now()
		for i := 0; i < rounds; i++ {
			t1 := time.Now()
			_ = t1
		}
		d := float64(time.Since(t0).Nanoseconds()) / rounds
		if d < best {
			best = d
		}
	}
	return best
}

// percentile returns the p-th percentile by nearest rank, on a copy: the sample
// order is the probe order and sorting it in place would spoil the next arm.
func percentile(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	slices.Sort(c)
	rank := (p*len(c) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(c) {
		rank = len(c)
	}
	return c[rank-1]
}

func printTable(rep *report, reps int) {
	fmt.Printf("RASK compact sparse-range index (HIP-1344, hanzoai/vfs#59)\n")
	fmt.Printf("%s %s/%s, %d cpus, load %s\n\n", rep.GoVersion, rep.OS, rep.Arch, rep.NumCPU, rep.LoadAverage)
	fmt.Printf("| Shape | n | max | leaves | B/ext slice | B/ext rask | ratio | payload | alloc slice | alloc rask | alloc ratio |\n")
	fmt.Printf("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rep.Results {
		fmt.Printf("| %s | %d | %d | %d | %.2f | %.2f | **%.2f** | %.2f | %.2f | %.2f | %.2f |\n",
			r.Shape, r.N, r.MaxLeaf, r.Leaves, r.BytesPerEx, r.RaskPerEx, r.Ratio, r.PayloadPer,
			r.AllocSlice, r.AllocRask, r.AllocRatio)
	}
	fmt.Printf("\nH3 base sweep (10,000 clustered extents, identical gaps):\n")
	for _, h := range rep.LargeSpan {
		fmt.Printf("  base=%-20d rask %7d B  slice %7d B\n", h.Base, h.RaskBytes, h.SliceBytes)
	}
	if len(rep.LeafSize) > 0 {
		fmt.Printf("\nLeaf size sweep (100,000 clustered extents, lookup, min of %d):\n", reps)
		for _, l := range rep.LeafSize {
			fmt.Printf("  maxLeaf=%-5d leaves=%-6d %8.0f ns (worst %8.0f)\n", l.MaxLeaf, l.Leaves, l.NsPerOp, l.SpreadNs)
		}
	}
	if len(rep.Percentiles) > 0 {
		fmt.Printf("\nLookup percentiles, interleaved (100,000 extents, one call per sample):\n")
		fmt.Printf("| shape | max leaf | leaves | slice p50 | slice p90 | slice p99 | rask p50 | rask p90 | rask p99 | p50 ratio | p99 ratio | p99 net |\n")
		fmt.Printf("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, r := range rep.Percentiles {
			fmt.Printf("| %s | %d | %d | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | **%.2f** | **%.2f** | **%.2f** |\n",
				r.Shape, r.MaxLeaf, r.Leaves, r.SliceP50, r.SliceP90, r.SliceP99, r.RaskP50, r.RaskP90, r.RaskP99,
				r.RatioP50, r.RatioP99, r.RatioP99Net)
		}
		fmt.Printf("  clock overhead %.0f ns per sample, present in both arms\n", rep.Percentiles[0].OverheadNs)
	}
}

// loadAverage reports the 1/5/15-minute figures of the box, because a number
// measured on a busy host is not the same number measured on an idle one.
func loadAverage() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable on " + runtime.GOOS
	}
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
