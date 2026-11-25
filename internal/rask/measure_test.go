package rask

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"testing"
)

// The pre-registered hypothesis for Bounty #2, stated before any number below
// was read:
//
//	H1  at 100,000 extents written in bursts, Index costs at most 40% of
//	    Slice's bytes per extent;
//	H2  a covering lookup stays within 1.20x of Slice's cost;
//	H3  the cost follows the number of extents, not the logical span.
//
// F1 is H1 refuted, F2 is H2, F3 is H3. A refutation is a result: the bounty
// admits a documented experiment with valid negative results, and the numbers
// below decide which of the two deliverables is sent.
//
// Environment of every number here, because it is part of the measurement: Go
// 1.27.4 darwin/arm64, 8 logical CPUs, load average 14-23 while these ran (the
// box was oversubscribed ~3x). Wall clock on this box has a floor near 100 ns
// and up to 4x spread between repeats of the same benchmark, so timings are the
// *minimum* of k repeats — the run least contaminated by preemption — and no
// timing difference under 2x is claimed. Memory is deterministic and needs no
// repeat game.

const pib = uint64(1) << 50

// sink keeps the lookups from being optimised away.
var sink Extent

// extents builds n extents over 1 PiB in one of three shapes. contiguous is
// the floor of the comparison (no gaps at all); clustered continues a run
// seven times out of eight, which is the production pattern RASK is built on;
// uniform is the adversary, every offset random.
func extents(n int, dist string, rng *rand.Rand) []Extent {
	es := make([]Extent, 0, n)
	switch dist {
	case "contiguous":
		var off uint64
		for i := 0; i < n; i++ {
			l := uint32(4096 + rng.IntN(1<<16))
			es = append(es, Extent{Off: off, Len: l})
			off += uint64(l)
		}
	case "clustered":
		var off uint64
		for i := 0; i < n; i++ {
			if i%8 == 0 {
				off += uint64(1 + rng.IntN(1<<20))
			}
			l := uint32(4096 + rng.IntN(1<<16))
			es = append(es, Extent{Off: off, Len: l})
			off += uint64(l)
		}
	case "uniform":
		seen := make(map[uint64]struct{}, n)
		for len(es) < n {
			off := rng.Uint64N(pib)
			if _, dup := seen[off]; dup {
				continue
			}
			seen[off] = struct{}{}
			es = append(es, Extent{Off: off, Len: 4096})
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Off < es[j].Off })
	default:
		panic("unknown distribution " + dist)
	}
	return es
}

func buildSlice(es []Extent) *Slice {
	s := &Slice{}
	for _, e := range es {
		s.Insert(e)
	}
	return s
}

func buildIndex(es []Extent, maxLeaf int) *Index {
	x := New(maxLeaf)
	for _, e := range es {
		x.Insert(e)
	}
	return x
}

// probes are nine hits and one hole out of ten.
func probes(es []Extent, n int, rng *rand.Rand) []uint64 {
	q := make([]uint64, n)
	for i := range q {
		e := es[rng.IntN(len(es))]
		if i%10 == 9 {
			q[i] = e.End() + 1 + rng.Uint64N(4096)
		} else {
			q[i] = e.Off + rng.Uint64N(uint64(e.Len))
		}
	}
	return q
}

// allocated is every byte the build asks the runtime for, cumulative, over a
// control run at n=0. TotalAlloc is deterministic — it counts the same on every
// run and has no collection to lose a delta to — so it is the portable meter
// here; a live-heap reading (/proc on Linux, HeapAlloc elsewhere) came out
// unstable on this box and is not what decides the comparison.
func allocated(build func([]Extent) any, es []Extent) int64 {
	measure := func(v []Extent) int64 {
		runtime.GC()
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		o := build(v)
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		runtime.KeepAlive(o)
		runtime.KeepAlive(v)
		return int64(m1.TotalAlloc) - int64(m0.TotalAlloc)
	}
	return measure(es) - measure(nil)
}

// TestMeasure prints the table the report quotes. It asserts nothing: the
// numbers are the deliverable, and the pass/fail of the hypothesis is stated in
// the PR against F1..F3.
func TestMeasure(t *testing.T) {
	if testing.Short() {
		t.Skip("a measurement")
	}
	// Two leaf sizes, because they trade the two axes against each other: a
	// split leaves the halves at max/2 extents, so max 32 holds ~16 (smaller
	// per-leaf overhead share, faster scan) and max 64 holds ~32 (cheaper per
	// extent). Timing lives in the benchmarks; this table is the deterministic
	// half of the comparison.
	t.Logf("%-10s %7s %6s %8s %10s %10s %7s %10s %12s %12s %7s",
		"shape", "n", "max", "leaves", "B/ext slice", "B/ext rask", "ratio", "payload", "alloc slice", "alloc rask", "alloc r")
	for _, dist := range []string{"contiguous", "clustered", "uniform"} {
		for _, n := range []int{1000, 10000, 100000} {
			es := extents(n, dist, rand.New(rand.NewPCG(uint64(n), 42)))
			s := buildSlice(es)
			if s.Len() != n {
				t.Fatalf("%s n=%d: built %d", dist, n, s.Len())
			}
			as := allocated(func(v []Extent) any { return buildSlice(v) }, es)
			for _, maxLeaf := range []int{32, 64} {
				x := buildIndex(es, maxLeaf)
				if x.Len() != n {
					t.Fatalf("%s n=%d: built %d", dist, n, x.Len())
				}
				ax := allocated(func(v []Extent) any { return buildIndex(v, maxLeaf) }, es)
				t.Logf("%-10s %7d %6d %8d %10.2f %10.2f %7.2f %10.2f %12.2f %12.2f %7.2f",
					dist, n, maxLeaf, x.Leaves(),
					float64(s.Bytes())/float64(n), float64(x.Bytes())/float64(n),
					float64(x.Bytes())/float64(s.Bytes()), float64(x.Payload())/float64(n),
					float64(as)/float64(n), float64(ax)/float64(n),
					float64(ax)/float64(as))
			}
		}
	}

	// H3: moving the whole file out must not move the cost. Same n, same gaps,
	// only the base offset changes; the first gap of each leaf is absolute, so
	// it is the one thing that may grow.
	small := extents(10000, "clustered", rand.New(rand.NewPCG(7, 8)))
	for _, base := range []uint64{0, 1 << 30, pib} {
		shifted := make([]Extent, len(small))
		for i, e := range small {
			shifted[i] = Extent{Off: e.Off + base, Len: e.Len}
		}
		t.Logf("H3 base=%d (%d extents, same gaps): rask %d bytes, slice %d bytes",
			base, len(small), buildIndex(shifted, 64).Bytes(), buildSlice(shifted).Bytes())
	}
}

func BenchmarkLookup(b *testing.B) {
	for _, dist := range []string{"clustered", "uniform"} {
		for _, n := range []int{1000, 10000, 100000} {
			es := extents(n, dist, rand.New(rand.NewPCG(uint64(n), 42)))
			q := probes(es, 4096, rand.New(rand.NewPCG(uint64(n), 43)))
			s := buildSlice(es)
			x := buildIndex(es, 32)
			b.Run(fmt.Sprintf("%s/slice/n=%d", dist, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					sink, _ = s.Lookup(q[i&4095])
				}
			})
			b.Run(fmt.Sprintf("%s/rask/n=%d", dist, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					sink, _ = x.Lookup(q[i&4095])
				}
			})
		}
	}
}

func BenchmarkInsert(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		es := extents(n, "clustered", rand.New(rand.NewPCG(uint64(n), 42)))
		b.Run(fmt.Sprintf("slice/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s := &Slice{}
				for _, e := range es {
					s.Insert(e)
				}
				sink = Extent{Off: uint64(s.Bytes())}
			}
		})
		b.Run(fmt.Sprintf("rask/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				x := New(32)
				for _, e := range es {
					x.Insert(e)
				}
				sink = Extent{Off: uint64(x.Bytes())}
			}
		})
	}
}

// BenchmarkLeafSize finds where the per-leaf scan costs more than the wider
// search over more leaves. It is what decides whether an in-leaf fence is
// worth its bytes.
func BenchmarkLeafSize(b *testing.B) {
	es := extents(100000, "clustered", rand.New(rand.NewPCG(100000, 42)))
	q := probes(es, 4096, rand.New(rand.NewPCG(100000, 43)))
	for _, maxLeaf := range []int{16, 32, 64, 128, 256, 1024} {
		x := buildIndex(es, maxLeaf)
		b.Run(fmt.Sprintf("leaf=%d/leaves=%d", maxLeaf, x.Leaves()), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink, _ = x.Lookup(q[i&4095])
			}
		})
	}
}
