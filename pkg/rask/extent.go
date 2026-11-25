// Package rask prototypes the in-memory range index asked for by HIP-1344 /
// Bounty #2: ranges as keys, after RASK (FAST '26), measured against the
// array-and-binary-search index the engine keeps today.
//
// Nothing here touches the stored format: the prototype is an in-memory
// structure plus the harness that compares the two, and the persistent
// layout is the one already on disk.
package rask

// Extent is a written run of a sparse file: Len bytes at Off.
//
// The chunk id that names the payload of a run is not part of the index and is
// left out on both sides of the comparison.
type Extent struct {
	Off uint64
	Len uint32
}

// End is the first byte past the extent.
func (e Extent) End() uint64 { return e.Off + uint64(e.Len) }
