package rask

import (
	"encoding/binary"
	"sort"
)

// Index is the RASK prototype: ranges are the key, a leaf holds a log of
// gap/length pairs, and the bounds array is the search key.
//
// vfs hands its index runs that are already sorted, disjoint and never
// touching (file.go's layer.dirty), so the log-structured handling of
// overlapping ranges is not exercised here: what this prototype measures is
// range-as-key compression plus range-aware split. Merging is the other half of
// the range-aware pair and is not exercised either, because the prototype has
// no per-extent delete: the engine punches holes by rewriting runs at
// publication, not by removing one extent from the index.
type Index struct {
	bounds []uint64 // first byte past the last extent of each leaf, ascending
	leaves []*leaf
	max    int
}

// minLeaf is the smallest leaf a split may leave: 8 extents.
const minLeaf = 8

// leafOverhead is what one leaf costs beyond its log: last, n, and the three
// words of the log's slice header.
const leafOverhead = 8 + 8 + 24

type leaf struct {
	log  []byte // per extent: uvarint(Off - previous End), uvarint(Len)
	n    int
	last uint64 // End of the last extent
}

// New returns an index whose leaves hold at most maxLeaf extents.
func New(maxLeaf int) *Index {
	if maxLeaf < 2*minLeaf {
		maxLeaf = 2 * minLeaf
	}
	return &Index{max: maxLeaf}
}

// Len is the number of extents indexed.
func (x *Index) Len() int {
	n := 0
	for _, l := range x.leaves {
		n += l.n
	}
	return n
}

// Insert adds one extent.
func (x *Index) Insert(e Extent) {
	if e.Len == 0 {
		return
	}
	if len(x.leaves) == 0 {
		l := &leaf{}
		l.append(e)
		x.leaves = append(x.leaves, l)
		x.bounds = append(x.bounds, e.End())
		return
	}
	i := sort.Search(len(x.bounds), func(j int) bool { return x.bounds[j] > e.Off })
	if i == len(x.bounds) {
		i = len(x.bounds) - 1
	}
	l := x.leaves[i]
	if l.n > 0 && l.last <= e.Off {
		l.append(e)
		x.bounds[i] = l.last
	} else {
		es := l.decode()
		at := sort.Search(len(es), func(j int) bool { return es[j].Off >= e.Off })
		es = append(es, Extent{})
		copy(es[at+1:], es[at:])
		es[at] = e
		l = &leaf{}
		l.encode(es)
		x.leaves[i] = l
		x.bounds[i] = l.last
	}
	if l.n > x.max {
		x.split(i)
	}
}

// split halves leaf i. The left half keeps the lower offsets.
func (x *Index) split(i int) {
	es := x.leaves[i].decode()
	h := len(es) / 2
	left, right := &leaf{}, &leaf{}
	left.encode(es[:h])
	right.encode(es[h:])
	x.leaves[i] = left
	x.leaves = append(x.leaves, nil)
	copy(x.leaves[i+2:], x.leaves[i+1:])
	x.leaves[i+1] = right

	x.bounds = append(x.bounds, 0)
	copy(x.bounds[i+2:], x.bounds[i+1:])
	x.bounds[i] = left.last
	x.bounds[i+1] = right.last
}

// Lookup returns the extent that covers off.
func (x *Index) Lookup(off uint64) (Extent, bool) {
	i := sort.Search(len(x.bounds), func(j int) bool { return x.bounds[j] > off })
	if i == len(x.leaves) {
		return Extent{}, false
	}
	b := x.leaves[i].log
	var prevEnd uint64
	for len(b) > 0 {
		gap, m := binary.Uvarint(b)
		if m <= 0 {
			break
		}
		b = b[m:]
		n, m := binary.Uvarint(b)
		if m <= 0 {
			break
		}
		b = b[m:]
		start := prevEnd + gap
		if off < start {
			return Extent{}, false
		}
		if end := start + n; off < end {
			return Extent{Off: start, Len: uint32(n)}, true
		} else {
			prevEnd = end
		}
	}
	return Extent{}, false
}

// Entries decodes every leaf in order. It is the test's and the harness's
// read path, not the engine's.
func (x *Index) Entries() []Extent {
	out := make([]Extent, 0, x.Len())
	for _, l := range x.leaves {
		out = append(out, l.decode()...)
	}
	return out
}

// Bytes is the size of the index structure: bounds, leaf pointers, one log per
// leaf over the capacity it actually reserved, and each leaf's fixed fields.
func (x *Index) Bytes() int {
	n := cap(x.bounds)*8 + cap(x.leaves)*8
	for _, l := range x.leaves {
		n += cap(l.log) + leafOverhead
	}
	return n
}

// Payload is what the encoded extents themselves cost, without the per-leaf
// slice header, fixed fields or the capacity an append left unused. It is the
// floor the encoding can reach and the line the measurement compares Bytes
// against, so the gap between the two is attributed rather than hidden.
func (x *Index) Payload() int {
	n := 0
	for _, l := range x.leaves {
		n += len(l.log)
	}
	return n
}

// Leaves is the number of leaves, for the measurement's report.
func (x *Index) Leaves() int { return len(x.leaves) }

func (l *leaf) append(e Extent) {
	var tmp [2 * binary.MaxVarintLen64]byte
	gap := e.Off // the decoder starts at 0, so the first gap is absolute
	if l.n > 0 {
		gap = e.Off - l.last
	}
	k := binary.PutUvarint(tmp[:], gap)
	k += binary.PutUvarint(tmp[k:], uint64(e.Len))
	l.log = append(l.log, tmp[:k]...)
	l.n++
	l.last = e.End()
}

func (l *leaf) encode(es []Extent) {
	l.log = l.log[:0]
	l.n = 0
	l.last = 0
	for _, e := range es {
		l.append(e)
	}
}

func (l *leaf) decode() []Extent {
	out := make([]Extent, 0, l.n)
	var prevEnd uint64
	b := l.log
	for len(b) > 0 {
		gap, m := binary.Uvarint(b)
		if m <= 0 {
			break
		}
		b = b[m:]
		n, m := binary.Uvarint(b)
		if m <= 0 {
			break
		}
		b = b[m:]
		start := prevEnd + gap
		out = append(out, Extent{Off: start, Len: uint32(n)})
		prevEnd = start + n
	}
	return out
}
