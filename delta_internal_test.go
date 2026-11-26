package vfs

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/hanzoai/vfs/internal/storetest"
)

// reach returns every shard the root reaches, read through sh.
func reach(t testing.TB, sh *shards, root ref) map[ref]struct{} {
	t.Helper()
	out := make(map[ref]struct{})
	var walk func(r ref)
	walk = func(r ref) {
		if r == none {
			return
		}
		if _, ok := out[r]; ok {
			return
		}
		out[r] = struct{}{}
		n, err := sh.load(context.Background(), r)
		if err != nil {
			t.Fatalf("reach: %v", err)
		}
		if n.level == 0 {
			return
		}
		for _, v := range n.vals {
			walk(refOf(v))
		}
	}
	walk(root)
	return out
}

// fill copies the shards of root from src to dst, and nothing else: the store of a
// replica that holds one checkpoint of the source.
func fill(t testing.TB, src, dst *storetest.Mem, sh *shards, root ref) {
	t.Helper()
	have := reach(t, sh, root)
	for k, v := range src.Dump() {
		for r := range have {
			if k == shardKey(r) {
				if err := dst.Put(context.Background(), k, v); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if len(dst.Keys()) != len(have) {
		t.Fatalf("the replica holds %d shards of the %d the checkpoint reaches", len(dst.Keys()), len(have))
	}
}

// records returns n records whose keys are name%06d.
func recordSet(name string, from, n int) []entry {
	es := make([]entry, n)
	for i := range es {
		k := []byte(fmt.Sprintf("%s%06d", name, from+i))
		es[i] = entry{key: k, val: append([]byte("v"), k...)}
	}
	return es
}

func sorted(es []entry) []entry {
	out := append([]entry(nil), es...)
	sort.Slice(out, func(i, j int) bool { return string(out[i].key) < string(out[j].key) })
	return out
}

// TestDeltaPlanCopiesWhatTheTargetLacks builds two checkpoints of a deep tree that
// differ in a few records, plans the delta between them, applies it to a store that
// holds the older checkpoint, and checks that the older store now reaches the newer
// root and holds the same records under it. The tree is cut small so that a few
// thousand records stand several levels tall: the walk prunes whole subtrees, which is
// the whole point of a Merkle delta.
func TestDeltaPlanCopiesWhatTheTargetLacks(t *testing.T) {
	ctx := context.Background()
	be := storetest.NewMem()
	tr := smallTree(be)
	old := recordSet("k", 0, 3000)
	a, err := tr.build(ctx, 0, old)
	if err != nil {
		t.Fatal(err)
	}
	n, err := tr.sh.load(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if n.level < 3 {
		t.Fatalf("this test needs a tree several levels tall, got %d levels", n.level+1)
	}

	fresh := recordSet("k", 3000, 20)
	changed := recordSet("k", 0, 20)
	for i := range changed {
		changed[i].val = []byte("changed")
	}
	newer := append(append(append([]entry(nil), old[20:]...), fresh...), changed...)
	b, err := tr.build(ctx, 0, sorted(newer))
	if err != nil {
		t.Fatal(err)
	}

	dst := storetest.NewMem()
	fill(t, be, dst, tr.sh, a)
	p, err := planDelta(ctx, tr.sh, tr.sh, a, b, &Plan{Streams: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.Pruned == 0 {
		t.Fatalf("the walk pruned no subtree of %d shards", len(p.Shards))
	}
	if p.Objects() >= len(reach(t, tr.sh, b)) {
		t.Fatalf("the plan copies %d objects, as many as the newer checkpoint has", p.Objects())
	}
	tr2, err := p.Apply(ctx, be, dst)
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Shards != len(p.Shards) {
		t.Fatalf("apply copied %d shards, the plan names %d", tr2.Shards, len(p.Shards))
	}

	// The newer root, read through the replica: a shard the plan left out is a shard
	// the replica does not have, and the read fails.
	dt := smallTree(dst)
	want, got := dump(t, tr, b, nil), dump(t, dt, b, nil)
	if !same(got, want) {
		t.Fatalf("the replica reaches %d records under the newer root, the source %d", len(got), len(want))
	}
}

// TestDeltaPlanSurvivesAHeightChange plans a delta between trees of different height,
// in both directions: a tree that grew has no node at the old place to compare with, so
// the walk copies the subtree it cannot pair instead of guessing.
func TestDeltaPlanSurvivesAHeightChange(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		small, up int
	}{{"grew", 400, 4000}, {"shrank", 4000, 400}} {
		t.Run(tc.name, func(t *testing.T) {
			be := storetest.NewMem()
			tr := smallTree(be)
			a, err := tr.build(ctx, 0, recordSet("k", 0, tc.small))
			if err != nil {
				t.Fatal(err)
			}
			b, err := tr.build(ctx, 0, recordSet("k", 0, tc.up))
			if err != nil {
				t.Fatal(err)
			}
			an, err := tr.sh.load(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			bn, err := tr.sh.load(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			if an.level == bn.level {
				t.Fatalf("both trees are %d levels tall: nothing to change", an.level+1)
			}
			dst := storetest.NewMem()
			fill(t, be, dst, tr.sh, a)
			p, err := planDelta(ctx, tr.sh, tr.sh, a, b, &Plan{Streams: 1})
			if err != nil {
				t.Fatal(err)
			}
			if p.Height != bn.level+1 {
				t.Fatalf("the plan says the newer tree is %d levels tall, it is %d", p.Height, bn.level+1)
			}
			if _, err := p.Apply(ctx, be, dst); err != nil {
				t.Fatal(err)
			}
			if dt := smallTree(dst); !same(dump(t, dt, b, nil), dump(t, tr, b, nil)) {
				t.Fatal("the replica does not reach the newer root")
			}
		})
	}
}

// TestDeltaPlanSkipsPacksTheOlderTreeNames checks the other half of the plan: a leaf
// whose chunk records are the older tree's too names no pack of its own, so the pack
// is not copied. Only the pack of the chunks the newer tree added is.
func TestDeltaPlanSkipsPacksTheOlderTreeNames(t *testing.T) {
	ctx := context.Background()
	var p1, p2 packID
	p1[0], p2[0] = 1, 2
	chunks := func(from, n int, pack packID) []entry {
		es := make([]entry, n)
		for i := range es {
			var id chunkID
			id[0], id[1] = 1, byte(from+i)
			es[i] = entry{key: keyChunk(id), val: encodeWhere(where{pack: pack, off: uint64(i) * 4096, n: 4096})}
		}
		return es
	}
	base := append(recordSet("k", 0, 2000), chunks(0, 20, p1)...)
	more := append(append([]entry(nil), base...), chunks(100, 20, p2)...)

	be := storetest.NewMem()
	tr := smallTree(be)
	a, err := tr.build(ctx, 0, sorted(base))
	if err != nil {
		t.Fatal(err)
	}
	b, err := tr.build(ctx, 0, sorted(more))
	if err != nil {
		t.Fatal(err)
	}
	p, err := planDelta(ctx, tr.sh, tr.sh, a, b, &Plan{Streams: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Packs) != 1 || p.Packs[0] != packKey(p2) {
		t.Fatalf("the plan copies the packs %v; the older tree already names %s", p.Packs, packKey(p1))
	}
}
