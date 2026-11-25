package rask

import (
	"math/rand/v2"
	"runtime"
	"sort"
)

// Pib is the logical span every workload is scattered over: 1 PiB, well past
// what a 32-bit offset field can address, so the index is exercised on the
// address space the bounty names rather than on a toy one.
const Pib = uint64(1) << 50

// Extents builds n sorted, disjoint extents over Pib in one of three shapes.
//
//   - contiguous is the floor of the comparison: no gaps at all.
//   - clustered continues a run seven times out of eight, which is the
//     production pattern RASK is built on (a journal span or a sparse file
//     grows in runs, not in uniformly random pages).
//   - uniform is the adversary: every offset random over the whole span.
//
// The shapes exist so the answer is not "RASK wins" but "RASK wins on these
// shapes by this much", which is the only form a maintainer can act on.
func Extents(n int, dist string, rng *rand.Rand) []Extent {
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
			off := rng.Uint64N(Pib)
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

// BuildSlice fills the baseline with es.
func BuildSlice(es []Extent) *Slice {
	s := &Slice{}
	for _, e := range es {
		s.Insert(e)
	}
	return s
}

// BuildIndex fills the RASK prototype with es, at most maxLeaf extents per leaf.
func BuildIndex(es []Extent, maxLeaf int) *Index {
	x := New(maxLeaf)
	for _, e := range es {
		x.Insert(e)
	}
	return x
}

// Probes returns n query offsets drawn from es: nine hits and one hole in ten,
// so a structure that returns "not found" for everything cannot score well.
func Probes(es []Extent, n int, rng *rand.Rand) []uint64 {
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

// Allocated is every byte the build asks the runtime for, cumulative, over a
// control run at n=0. TotalAlloc is deterministic — it counts the same on every
// run and has no collection to lose a delta to — so it is the portable meter
// here; a live-heap reading (/proc on Linux, HeapAlloc elsewhere) came out
// unstable on the measuring box and is not what decides the comparison.
func Allocated(build func([]Extent) any, es []Extent) int64 {
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
