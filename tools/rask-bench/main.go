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
	"math/rand/v2"
	"os"
	"runtime"
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
}

func main() {
	out := flag.String("out", "", "write the raw JSON here instead of printing a table")
	asJSON := flag.Bool("json", false, "emit the JSON report on stdout")
	reps := flag.Int("reps", 3, "timing repetitions; the minimum is reported")
	noTiming := flag.Bool("no-timing", false, "skip the timing sweeps (deterministic only)")
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
		Note:        "Memory is deterministic (TotalAlloc over an n=0 control). Timings are the minimum of -reps runs; no difference under 2x is claimed.",
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
