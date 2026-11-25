package rask

import (
	"math/rand/v2"
	"sort"
	"testing"
)

// model is the naive index the tests compare against: a sorted array scanned
// linearly. It is deliberately slower than either implementation so that a
// wrong answer in one of them cannot be hidden by the other.
type model []Extent

func (m model) sorted() model {
	s := append(model(nil), m...)
	sort.Slice(s, func(i, j int) bool { return s[i].Off < s[j].Off })
	return s
}

func (m model) lookup(off uint64) (Extent, bool) {
	for _, e := range m {
		if off >= e.Off && off < e.End() {
			return e, true
		}
	}
	return Extent{}, false
}

func randExtents(rng *rand.Rand, n int, span uint64) []Extent {
	out := make([]Extent, 0, n)
	var off uint64
	for i := 0; i < n; i++ {
		if i%3 == 0 {
			off += uint64(rng.IntN(1 << 20))
		}
		l := uint32(1 + rng.IntN(1<<14))
		out = append(out, Extent{Off: off, Len: l})
		off += uint64(l)
	}
	return out
}

func TestBothIndexesAgreeWithModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	es := randExtents(rng, 400, 1<<20)
	m := model(es).sorted()

	s := &Slice{}
	x := New(16)
	for _, e := range es {
		s.Insert(e)
		x.Insert(e)
	}
	if got, want := s.Len(), len(es); got != want {
		t.Fatalf("Slice.Len() = %d, want %d", got, want)
	}
	if got, want := x.Len(), len(es); got != want {
		t.Fatalf("Index.Len() = %d, want %d", got, want)
	}

	if got := x.Entries(); !sameExtents(got, m) {
		t.Fatalf("Index.Entries() is not the sorted input (%d vs %d)", len(got), len(m))
	}

	for probe := uint64(0); probe < m[len(m)-1].End()+64; probe += 997 {
		want, wantOK := m.lookup(probe)
		if got, ok := s.Lookup(probe); ok != wantOK || got != want {
			t.Fatalf("Slice.Lookup(%d) = %v,%v; want %v,%v", probe, got, ok, want, wantOK)
		}
		if got, ok := x.Lookup(probe); ok != wantOK || got != want {
			t.Fatalf("Index.Lookup(%d) = %v,%v; want %v,%v", probe, got, ok, want, wantOK)
		}
	}
}

func TestOutOfOrderInsert(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	es := randExtents(rng, 300, 1<<20)
	shuffled := append([]Extent(nil), es...)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	x := New(16)
	for _, e := range shuffled {
		x.Insert(e)
	}
	if got := x.Entries(); !sameExtents(got, model(es).sorted()) {
		t.Fatal("out-of-order inserts do not settle into offset order")
	}
}

func TestInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	x := New(16)
	for _, e := range randExtents(rng, 1000, 1<<30) {
		x.Insert(e)
	}
	if x.Len() != 1000 {
		t.Fatalf("Len() = %d, want 1000", x.Len())
	}
	if len(x.bounds) != len(x.leaves) {
		t.Fatalf("%d bounds for %d leaves", len(x.bounds), len(x.leaves))
	}
	var prev uint64
	for i, l := range x.leaves {
		if l.n == 0 || l.n > x.max {
			t.Fatalf("leaf %d holds %d extents, bounds are [1, %d]", i, l.n, x.max)
		}
		if l.last != x.bounds[i] {
			t.Fatalf("leaf %d ends at %d, bound says %d", i, l.last, x.bounds[i])
		}
		if i > 0 && x.bounds[i] <= prev {
			t.Fatalf("bounds not ascending at %d: %d after %d", i, x.bounds[i], prev)
		}
		prev = x.bounds[i]
	}
	es := x.Entries()
	for i := 1; i < len(es); i++ {
		if es[i].Off < es[i-1].End() {
			t.Fatalf("extents %d and %d overlap or touch", i-1, i)
		}
	}
}

// TestPetabyteSpan is the 1 PiB behaviour: what the index costs must follow the
// number of extents, not the logical size, and a lookup near the far end must
// still answer.
func TestPetabyteSpan(t *testing.T) {
	const span = uint64(1) << 50
	offs := []uint64{0, 65536, 1 << 30, 1 << 40, span - 4096}
	es := make([]Extent, len(offs))
	for i, off := range offs {
		es[i] = Extent{Off: off, Len: 4096}
	}
	s := &Slice{}
	x := New(16)
	for _, e := range es {
		s.Insert(e)
		x.Insert(e)
	}
	for i, off := range offs {
		if got, ok := x.Lookup(off + 10); !ok || got != es[i] {
			t.Fatalf("Index.Lookup(%d) = %v,%v; want %v", off+10, got, ok, es[i])
		}
		if got, ok := s.Lookup(off + 10); !ok || got != es[i] {
			t.Fatalf("Slice.Lookup(%d) = %v,%v; want %v", off+10, got, ok, es[i])
		}
	}
	if bytes := x.Bytes(); bytes > 4096 {
		t.Fatalf("%d extents over 1 PiB cost %d bytes", len(es), bytes)
	}
	if _, ok := x.Lookup(span - 1); !ok {
		t.Fatal("last byte of 1 PiB is not covered")
	}
	if _, ok := x.Lookup(span); ok {
		t.Fatal("byte past 1 PiB is covered")
	}
}

func sameExtents(a, b []Extent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
