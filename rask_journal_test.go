// Copyright 2026 Hanzo AI Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package vfs

// This file is the integration half of HIP-1344 / Bounty #2 (Refs #59): the
// RASK study in pkg/rask measures synthetic shapes, and this test measures the
// index the Volume would actually build — from the write extents the Volume
// itself put in its journal, decoded by the Volume's own replay decoder, on a
// file truncated to 1 PiB. No stored format changes: the index lives in
// memory and the format-v2 tree on disk is untouched.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/luxfi/age"

	"github.com/hanzoai/vfs/internal/storetest"
	"github.com/hanzoai/vfs/journal"
	"github.com/hanzoai/vfs/pkg/rask"
)

// raskVolume is a writable Volume with a real journal on this node's disk and a
// bound long enough that nothing is published while the test replays.
func raskVolume(t testing.TB) *Volume {
	t.Helper()
	k, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Backend:         storetest.NewMem(),
		Cipher:          Hybrid(k),
		Journal:         t.TempDir(),
		JournalCapacity: 64 << 20,
		Disk:            storetest.Disk,
		Bound:           time.Hour,
	}
	bg := context.Background()
	if err := Create(bg, cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	cfg.Round = 1
	v, err := Attach(bg, cfg)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

// journalSpans replays the journal the Volume holds and returns every written
// run in it, exactly as the Volume records a mutation: the record is unsealed
// with the journal's key and decoded by decodeChange.
func journalSpans(t testing.TB, v *Volume) []rask.Extent {
	t.Helper()
	var spans []rask.Extent
	err := v.led.j.Replay(func(r journal.Record) error {
		plain, err := v.led.open(r.Data)
		if err != nil {
			return err
		}
		c, err := decodeChange(plain)
		if err != nil {
			return err
		}
		if c.kind == changeWrite && len(c.data) > 0 {
			spans = append(spans, rask.Extent{Off: c.off, Len: uint32(len(c.data))})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	slices.SortFunc(spans, func(a, b rask.Extent) int {
		switch {
		case a.Off < b.Off:
			return -1
		case a.Off > b.Off:
			return 1
		}
		return 0
	})
	return spans
}

// writeRealExtents writes n runs of a sparse file through the Volume: at the
// head of a 1 PiB file, a clustered window in its middle, and its last page.
func writeRealExtents(t testing.TB, v *Volume, n int) {
	t.Helper()
	const pib = uint64(1) << 50
	bg := context.Background()
	in, err := v.Create(bg, RootInode, "pib", 0o644)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	f, err := v.Open(bg, in.ID)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(bg, pib); err != nil {
		t.Fatalf("truncate to 1 PiB: %v", err)
	}
	rng := rand.New(rand.NewPCG(59, 1344))
	at := func(off int64) {
		buf := make([]byte, 64+rng.IntN(4031)) // under 4096: consecutive runs never overlap
		if _, err := f.WriteAt(bg, buf, off); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	at(0)
	for i := range n {
		at(int64(pib/2) + int64(i)*4096)
	}
	at(int64(pib) - 4096)
}

// TestRASKIndexesTheJournalSpansOfRealWrites is the acceptance test the bounty
// asks for, on the real write path: the index is built from the journal's own
// records of a 1 PiB sparse file, every run it recorded is found at both ends,
// every hole between runs is not, and the index is smaller than the array of
// extents it replaces.
func TestRASKIndexesTheJournalSpansOfRealWrites(t *testing.T) {
	v := raskVolume(t)
	const n = 2000
	writeRealExtents(t, v, n)
	spans := journalSpans(t, v)
	if len(spans) < n {
		t.Fatalf("journal holds %d written runs, want at least %d", len(spans), n)
	}
	idx := rask.BuildIndex(spans, 64)
	sl := rask.BuildSlice(spans)

	for _, e := range spans {
		for _, off := range []uint64{e.Off, e.End() - 1} {
			got, ok := idx.Lookup(off)
			if !ok {
				t.Fatalf("rask: no extent covers %d (a recorded run starts at %d)", off, e.Off)
			}
			if got != e {
				t.Fatalf("rask: %d resolved to %+v, want %+v", off, got, e)
			}
			if _, ok := sl.Lookup(off); !ok {
				t.Fatalf("slice: no extent covers %d", off)
			}
		}
	}

	holes := 0
	for i := 1; i < len(spans); i++ {
		if spans[i].Off <= spans[i-1].End() {
			continue
		}
		holes++
		if _, ok := idx.Lookup(spans[i-1].End()); ok {
			t.Fatalf("rask: %d is the first byte past a run, before the one at %d, but resolved",
				spans[i-1].End(), spans[i].Off)
		}
	}
	if holes == 0 {
		t.Fatal("the run has no holes: the hole check proved nothing")
	}

	// Rebuilding from the same records gives the same structure: the index is a
	// function of what the journal holds, not of when it was built.
	again := rask.BuildIndex(journalSpans(t, v), 64)
	if again.Bytes() != idx.Bytes() || again.Len() != idx.Len() {
		t.Fatalf("rebuild differs: %d/%d bytes/entries, first %d/%d",
			again.Bytes(), again.Len(), idx.Bytes(), idx.Len())
	}

	t.Logf("1 PiB file, %d journal runs: rask %d B (%.2f B/extent), array %d B (%.2f B/extent), ratio %.2f, %d holes checked",
		len(spans), idx.Bytes(), float64(idx.Bytes())/float64(len(spans)),
		sl.Bytes(), float64(sl.Bytes())/float64(len(spans)),
		float64(idx.Bytes())/float64(sl.Bytes()), holes)
	if idx.Bytes() >= sl.Bytes() {
		t.Fatalf("rask %d B is not smaller than the array %d B", idx.Bytes(), sl.Bytes())
	}
}

// TestRASKLookupPercentilesOnJournalSpans measures lookup latency over real
// journal runs at two file sizes and three leaf widths, one call at a time and
// alternating which arm goes first, so the noise of a shared box is common to
// both arms and the ratio survives it. Each sample is the least of raskReps
// passes of raskRounds calls: a preemption inflates neither arm. The two clock
// calls are in both arms and are subtracted, which can only raise the ratio.
func TestRASKLookupPercentilesOnJournalSpans(t *testing.T) {
	overhead := raskClockOverhead()
	for _, n := range []int{2000, 20000} {
		v := raskVolume(t)
		writeRealExtents(t, v, n)
		spans := journalSpans(t, v)
		sl := rask.BuildSlice(spans)
		rng := rand.New(rand.NewPCG(7, 11))
		probes := rask.Probes(spans, 2000, rng)

		for _, leaf := range []int{8, 32, 64} {
			idx := rask.BuildIndex(spans, leaf)
			dIndex, dSlice := raskSample(t, idx, sl, probes, overhead)
			i50, i90, i99 := raskPercentile(dIndex, 50), raskPercentile(dIndex, 90), raskPercentile(dIndex, 99)
			s50, s90, s99 := raskPercentile(dSlice, 50), raskPercentile(dSlice, 90), raskPercentile(dSlice, 99)
			ratio50 := float64(i50) / float64(s50)
			ratio90 := float64(i90) / float64(s90)
			ratio99 := float64(i99) / float64(s99)
			t.Logf("%d journal runs, leaf %d, %d probes: %d B/extent (array %.2f, ratio %.2f) · "+
				"p50 %d/%d ns ratio %.2f · p90 %d/%d ns ratio %.2f · p99 %d/%d ns ratio %.2f",
				len(spans), leaf, len(probes),
				idx.Bytes()/len(spans), float64(sl.Bytes())/float64(len(spans)),
				float64(idx.Bytes())/float64(sl.Bytes()),
				i50, s50, ratio50, i90, s90, ratio90, i99, s99, ratio99)

			// A leaf scan decodes at most leaf extents; the array makes log2(n)
			// steps. A ratio above the leaf width is not a busy box, it is an
			// encoding that stopped bounding the scan. The median and the 90th
			// are asserted because they are stable run to run; the 99th at this
			// sample count is the twentieth worst sample, and on a shared
			// machine that is a scheduler measurement. The tail the bounty
			// names is measured by tools/raskbench, over 20,000 samples.
			if ratio50 > float64(leaf) {
				t.Fatalf("leaf %d: median lookup is %.1fx the array's, above the %d extents a leaf holds",
					leaf, ratio50, leaf)
			}
			if ratio90 > 2*float64(leaf) {
				t.Fatalf("leaf %d: p90 is %.1fx the array's, above twice the %d extents a leaf holds",
					leaf, ratio90, leaf)
			}
			if idx.Bytes() >= sl.Bytes() {
				t.Fatalf("leaf %d: %d B is not under the array's %d B", leaf, idx.Bytes(), sl.Bytes())
			}
		}
	}
}

const (
	raskRounds = 200 // calls per pass
	raskReps   = 2   // passes per sample; the sample is their least
)

// raskSample returns one latency sample per probe for each arm, in ns, with the
// clock removed. It refuses to time an index that answers differently from the
// array.
func raskSample(t *testing.T, idx *rask.Index, sl *rask.Slice,
	probes []uint64, overhead int64) (dIndex, dSlice []int64) {
	t.Helper()
	for _, q := range probes {
		ie, iok := idx.Lookup(q)
		se, sok := sl.Lookup(q)
		if iok != sok || (iok && ie != se) {
			t.Fatalf("at %d: rask (%+v, %t), array (%+v, %t)", q, ie, iok, se, sok)
		}
	}
	dIndex = make([]int64, 0, len(probes))
	dSlice = make([]int64, 0, len(probes))
	var sink rask.Extent
	once := func(arm interface {
		Lookup(uint64) (rask.Extent, bool)
	}, q uint64) int64 {
		best := int64(1 << 62)
		for range raskReps {
			t0 := time.Now()
			for range raskRounds {
				if e, ok := arm.Lookup(q); ok {
					sink = e
				}
			}
			best = min(best, netTime(time.Since(t0).Nanoseconds(), overhead)/int64(raskRounds))
		}
		return best
	}
	for i, q := range probes {
		if i%2 == 0 {
			dIndex = append(dIndex, once(idx, q))
			dSlice = append(dSlice, once(sl, q))
		} else {
			dSlice = append(dSlice, once(sl, q))
			dIndex = append(dIndex, once(idx, q))
		}
	}
	_ = sink
	return dIndex, dSlice
}

// BenchmarkRASKLookupJournalSpans reports ns/op for both arms over the real
// runs, so a reviewer reruns one command and compares:
//
//	go test -run '^$' -bench RASKLookupJournalSpans ./...
func BenchmarkRASKLookupJournalSpans(b *testing.B) {
	v := raskVolume(b)
	writeRealExtents(b, v, 2000)
	spans := journalSpans(b, v)
	rng := rand.New(rand.NewPCG(7, 11))
	probes := rask.Probes(spans, 1<<14, rng)
	for _, arm := range []struct {
		name string
		l    interface {
			Lookup(uint64) (rask.Extent, bool)
		}
	}{{"rask", rask.BuildIndex(spans, 64)}, {"array", rask.BuildSlice(spans)}} {
		b.Run(fmt.Sprintf("%s_%druns", arm.name, len(spans)), func(b *testing.B) {
			var sink rask.Extent
			for i := 0; i < b.N; i++ {
				if e, ok := arm.l.Lookup(probes[i%len(probes)]); ok {
					sink = e
				}
			}
			_ = sink
		})
	}
}

// raskClockOverhead is what a bracketing pair of time.Now calls costs here: the
// least of many, so the number is the call and not the scheduler. It is
// subtracted from both samples, and subtracting it can only raise the ratio.
func raskClockOverhead() int64 {
	best := int64(1 << 62)
	for range 1000 {
		t0 := time.Now()
		d := time.Since(t0).Nanoseconds()
		best = min(best, d)
	}
	return best
}

// netTime removes the clock from a measured call.
func netTime(ns, overhead int64) int64 {
	if ns <= overhead {
		return 0
	}
	return ns - overhead
}

// raskPercentile is the nearest-rank percentile of a copy of d.
func raskPercentile(d []int64, p int) int64 {
	if len(d) == 0 {
		return 0
	}
	c := slices.Clone(d)
	slices.Sort(c)
	i := (p*len(c)+99)/100 - 1
	return c[max(0, min(i, len(c)-1))]
}
