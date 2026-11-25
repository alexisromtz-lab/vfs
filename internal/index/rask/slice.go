package rask

import "sort"

// Slice models the index the engine keeps today: every extent in one array
// sorted by offset, found by binary search. file.go:590 keeps a run as a pair of
// uint64 (16 B) and record.go:235 keeps an extent as an offset, a length and
// the chunk id; the id is the payload and is not counted on either side.
type Slice struct {
	e []Extent
}

// Len is the number of extents indexed.
func (s *Slice) Len() int { return len(s.e) }

// Insert adds an extent, keeping the array sorted by offset. The engine hands
// its index runs that are already ordered, so the common case appends.
func (s *Slice) Insert(x Extent) {
	if x.Len == 0 {
		return
	}
	if n := len(s.e); n == 0 || s.e[n-1].Off < x.Off {
		s.e = append(s.e, x)
		return
	}
	i := sort.Search(len(s.e), func(i int) bool { return s.e[i].Off >= x.Off })
	s.e = append(s.e, Extent{})
	copy(s.e[i+1:], s.e[i:])
	s.e[i] = x
}

// Lookup returns the extent that covers off.
func (s *Slice) Lookup(off uint64) (Extent, bool) {
	i := sort.Search(len(s.e), func(i int) bool { return s.e[i].Off > off })
	if i == 0 {
		return Extent{}, false
	}
	e := s.e[i-1]
	if off < e.End() {
		return e, true
	}
	return Extent{}, false
}

// Bytes is the size of the index structure: one Extent per entry over the
// capacity Go's append schedule actually reserved.
func (s *Slice) Bytes() int { return cap(s.e) * 16 }
