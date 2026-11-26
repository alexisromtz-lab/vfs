package vfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/hanzoai/vfs"
	"github.com/hanzoai/vfs/internal/storetest"
)

// TestDeltaRestoreBringsAStoreToACheckpoint restores a replica twice: a full copy of
// the first checkpoint into an empty store, then the delta to the second. The replica
// must hold the source's snapshot after each, and the delta must copy fewer objects and
// fewer bytes than the full copy did.
func TestDeltaRestoreBringsAStoreToACheckpoint(t *testing.T) {
	be := storetest.NewMem()
	c := vfsAge(t)
	if err := vfs.Create(ctx, cfg(be, c, 0)); err != nil {
		t.Fatal(err)
	}
	v, err := vfs.Attach(ctx, cfg(be, c, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })

	dir := mkdir(t, v, vfs.RootInode, "d")
	// 600 files of 2 KiB, under the 16 KiB a file is inlined below: their content
	// lives in the metadata tree, which takes tens of shards of 64 KiB, so a change of
	// three files leaves most of them to be pruned.
	var inos []vfs.Inode
	for i := range 600 {
		inos = append(inos, write(t, v, dir, fmt.Sprintf("f%03d", i), content(uint64(i), 2048)))
	}
	sub := mkdir(t, v, dir, "sub")
	write(t, v, sub, "deep", content(77, 4096))
	flush(t, v)
	at1, snap1 := v.Checkpoint(), snapshot(t, v)

	for _, i := range []int{3, 300, 590} {
		f, err := v.Open(ctx, inos[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(ctx, content(uint64(1000+i), 2048), 0); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	write(t, v, dir, "new", content(100, 512))
	if err := v.Remove(ctx, dir, "f005"); err != nil {
		t.Fatal(err)
	}
	flush(t, v)
	at2, snap2 := v.Checkpoint(), snapshot(t, v)
	if at1.Root == at2.Root {
		t.Fatal("the two checkpoints are one")
	}
	src := reopen(t, be, c)

	// The replica, at the first checkpoint.
	rbe := storetest.NewMem()
	full, err := vfs.PlanDelta(ctx, vfs.Side{}, vfs.Side{Volume: src, At: at1})
	if err != nil {
		t.Fatal(err)
	}
	ftr, err := full.Apply(ctx, be, rbe)
	if err != nil {
		t.Fatal(err)
	}
	if err := full.Commit(ctx, cfg(rbe, c, 1)); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, "replica at the first checkpoint", snapshot(t, reopen(t, rbe, c)), snap1)

	// The delta to the second, into the replica as it stands.
	rbe.Reset()
	rv := reopen(t, rbe, c)
	d, err := vfs.PlanDelta(ctx, vfs.Side{Volume: rv}, vfs.Side{Volume: src, At: at2})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pruned == 0 {
		t.Errorf("the walk pruned no subtree: the two trees share nothing")
	}
	// A delta copies a subset of what a full copy of the same checkpoint copies: the
	// shards and packs the newer tree reaches. That it copies fewer is a measurement,
	// not a property, and it is the one this workload is built to show.
	if d.Objects() > full.Objects() {
		t.Fatalf("the delta copies %d objects, the full copy %d", d.Objects(), full.Objects())
	}
	if d.Objects() == full.Objects() {
		t.Errorf("the delta copies every object of the newer checkpoint")
	}
	dtr, err := d.Apply(ctx, be, rbe)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Commit(ctx, cfg(rbe, c, 2)); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, "replica at the second checkpoint", snapshot(t, reopen(t, rbe, c)), snap2)
	if dtr.Bytes == 0 || dtr.Bytes >= ftr.Bytes {
		t.Errorf("the delta copied %d bytes, the full copy %d", dtr.Bytes, ftr.Bytes)
	}
	t.Logf("full: %d objects, %d bytes; delta: %d objects, %d bytes, %d subtrees pruned",
		full.Objects(), ftr.Bytes, d.Objects(), dtr.Bytes, d.Pruned)

	// What the delta wrote is the source's ciphertext: a shard under the name its own
	// bytes hash to, a pack under its own id. Only the Head is re-sealed, to the
	// replica's place, and so its bytes are the one copy that differs.
	source, replica := be.Dump(), rbe.Dump()
	wrote := rbe.Puts()
	if wrote["meta"] != len(d.Shards) || wrote["pack"] != len(d.Packs) {
		t.Errorf("the delta wrote %d shards and %d packs, the plan names %d and %d",
			wrote["meta"], wrote["pack"], len(d.Shards), len(d.Packs))
	}
	if wrote["head"] == 0 {
		t.Error("the delta wrote no Head")
	}
	for _, k := range append(append([]string(nil), d.Shards...), d.Packs...) {
		if !bytes.Equal(replica[k], source[k]) {
			t.Errorf("the copy of %s is not the source's bytes", k)
		}
	}
	if bytes.Equal(replica["head"], source["head"]) {
		t.Error("the replica kept the source's Head, sealed to another place")
	}

	// Two stores at one checkpoint: nothing to copy, and nothing written.
	rbe.Reset()
	same, err := vfs.PlanDelta(ctx, vfs.Side{Volume: reopen(t, rbe, c)}, vfs.Side{Volume: src, At: at2})
	if err != nil {
		t.Fatal(err)
	}
	if same.Objects() != 0 || same.Pruned != 1 {
		t.Errorf("one checkpoint against itself plans %d objects and prunes %d subtrees", same.Objects(), same.Pruned)
	}
	if _, err := same.Apply(ctx, be, rbe); err != nil {
		t.Fatal(err)
	}
	if rbe.Total() != 0 {
		t.Errorf("the delta of one checkpoint against itself wrote %d objects", rbe.Total())
	}

	// A store of another Volume is refused before anything is read: shards and chunks
	// are sealed to the Volume that wrote them.
	obe := storetest.NewMem()
	oc := vfsAge(t)
	if err := vfs.Create(ctx, cfg(obe, oc, 0)); err != nil {
		t.Fatal(err)
	}
	other, err := vfs.Open(ctx, cfg(obe, oc, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.PlanDelta(ctx, vfs.Side{Volume: other}, vfs.Side{Volume: src, At: at2}); !errors.Is(err, vfs.ErrDelta) {
		t.Fatalf("a delta between Volumes: %v", err)
	}
}

// TestDeltaRestoreCopiesAWholeStore runs the whole of it — plan, copy, commit — into a
// store that holds nothing.
func TestDeltaRestoreCopiesAWholeStore(t *testing.T) {
	be := storetest.NewMem()
	c := vfsAge(t)
	if err := vfs.Create(ctx, cfg(be, c, 0)); err != nil {
		t.Fatal(err)
	}
	v, err := vfs.Attach(ctx, cfg(be, c, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	write(t, v, vfs.RootInode, "a", content(1, 3000))
	write(t, v, vfs.RootInode, "b", content(2, 64<<10))
	flush(t, v)

	rbe := storetest.NewMem()
	p, tr, err := vfs.Restore(ctx, cfg(rbe, c, 1), cfg(be, c, 0))
	if err != nil {
		t.Fatal(err)
	}
	if p.Objects() != tr.Shards+tr.Packs {
		t.Errorf("the plan names %d objects, the copy did %d", p.Objects(), tr.Shards+tr.Packs)
	}
	if tr.Bytes == 0 {
		t.Error("the copy moved no bytes")
	}
	sameSnapshot(t, "the copy", snapshot(t, reopen(t, rbe, c)), snapshot(t, v))

	// The Head is the one object that is not copied: it is sealed to its place, so the
	// two stores hold different bytes that open to the same checkpoint.
	sh, _, err := be.Get(ctx, "head", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rh, _, err := rbe.Get(ctx, "head", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sh, rh) {
		t.Error("the copy kept the source's Head, sealed to another place")
	}
}
