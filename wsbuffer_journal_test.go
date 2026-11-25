package vfs_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hanzoai/vfs"
	"github.com/hanzoai/vfs/internal/storetest"
	"github.com/hanzoai/vfs/journal"
	"github.com/hanzoai/vfs/pkg/wsbuffer"
)

// layers collects the Disks a Config.Disk hands out: a Volume opens one per journal directory
// it touches, and what reach the media across all of them is one measurement.
type layers struct {
	mu sync.Mutex
	ds []*wsbuffer.Disk
}

func (l *layers) disk(run int) func(string) (journal.Disk, error) {
	return func(dir string) (journal.Disk, error) {
		d, err := storetest.Disk(dir)
		if err != nil {
			return nil, err
		}
		w := wsbuffer.New(d, wsbuffer.Options{Run: run})
		l.mu.Lock()
		l.ds = append(l.ds, w)
		l.mu.Unlock()
		return w, nil
	}
}

func (l *layers) stats() wsbuffer.Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s wsbuffer.Stats
	add := func(to *uint64, from uint64) { *to += from }
	for _, d := range l.ds {
		t := d.Stats()
		add(&s.Calls, t.Calls)
		add(&s.Bytes, t.Bytes)
		add(&s.Pages, t.Pages)
		add(&s.Partial, t.Partial)
		add(&s.RefCalls, t.RefCalls)
		add(&s.RefBytes, t.RefBytes)
		add(&s.RefPages, t.RefPages)
		add(&s.RefPartial, t.RefPartial)
		add(&s.Absorbed, t.Absorbed)
		add(&s.Flushed, t.Flushed)
	}
	return s
}

// scrapWorkload writes seven files of deterministic bytes, each by its own goroutine in chunks
// whose lengths are no page size, so the journal is asked to write batches that begin and end
// inside a page and fall inside one sync round.
func scrapWorkload(t testing.TB, v *vfs.Volume) {
	t.Helper()
	const files, chunks = 7, 16
	inos := make([]vfs.InodeID, files)
	for i := range inos {
		in, err := v.Create(ctx, vfs.RootInode, fmt.Sprintf("f%d", i), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		inos[i] = in.ID
	}
	var wg sync.WaitGroup
	for i, ino := range inos {
		wg.Add(1)
		go func(i int, ino vfs.InodeID) {
			defer wg.Done()
			f, err := v.Open(ctx, ino)
			if err != nil {
				t.Errorf("open f%d: %v", i, err)
				return
			}
			defer f.Close()
			var off int64
			for j := 0; j < chunks; j++ {
				b := content(uint64(i*100+j), 512+j*333)
				if _, err := f.WriteAt(ctx, b, off); err != nil {
					t.Errorf("write f%d chunk %d: %v", i, j, err)
					return
				}
				off += int64(len(b))
			}
		}(i, ino)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

// TestScrapJournalHoldsTheSameVolume runs one workload twice: on a Volume whose journal writes
// straight to the files, and on one whose journal writes through a wsbuffer layer that holds a
// mebibyte before it writes. Neither is flushed, so the changes reach the files and only the
// files, and a Volume that takes over has to replay them through the layer; both must end with
// the same tree, the same journal watermarks and the same inode count, and the layer must write
// fewer pages than the journal asked for.
func TestScrapJournalHoldsTheSameVolume(t *testing.T) {
	type result struct {
		snap    map[string]string
		state   vfs.State
		stats   vfs.Stats
		scrap   wsbuffer.Stats
		recover vfs.Recovery
	}
	run := func(mode string, window int) result {
		be := storetest.NewMem()
		c := vfsAge(t)
		l := &layers{}
		// Bound an hour: nothing publishes on its own, so Close leaves the workload in the
		// journal, which is the only place a replay can find it.
		mk := func(round uint64) vfs.Config {
			return vfs.Config{
				Backend: be, Cipher: c, Round: round,
				Journal: storetest.Journals(), JournalCapacity: 64 << 20,
				Disk: l.disk(window), Bound: time.Hour,
			}
		}
		if err := vfs.Create(ctx, mk(0)); err != nil {
			t.Fatal(err)
		}
		v, err := vfs.Attach(ctx, mk(1))
		if err != nil {
			t.Fatal(err)
		}
		scrapWorkload(t, v)
		if err := v.Close(); err != nil {
			t.Fatalf("%s: close writer: %v", mode, err)
		}

		w, err := vfs.Attach(ctx, mk(2))
		if err != nil {
			t.Fatalf("%s: attach to replay: %v", mode, err)
		}
		r := result{snap: snapshot(t, w), state: w.State(), stats: w.Stats(), recover: w.Recovery()}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: close reader: %v", mode, err)
		}
		r.scrap = l.stats()
		t.Logf("%s: set %d..%d replayed %d · %d calls wrote %d B in %d pages; the journal asked for %d calls, %d B, %d pages · ratio %.3f, without the layer %.3f · absorbed %d, flushed %d",
			mode, r.state.Durable, r.state.Visible, r.recover.Replayed,
			r.scrap.Calls, r.scrap.Bytes, r.scrap.Pages,
			r.scrap.RefCalls, r.scrap.RefBytes, r.scrap.RefPages,
			r.scrap.Ratio(), r.scrap.RefRatio(), r.scrap.Absorbed, r.scrap.Flushed)
		return r
	}

	through := run("through", -1)
	window := run("window", wsbuffer.DefaultRun)

	for _, r := range []struct {
		name string
		res  result
	}{{"through", through}, {"window", window}} {
		if r.res.recover.Replayed == 0 {
			t.Errorf("%s: nothing was replayed, so the layer was not on the path that wrote it", r.name)
		}
	}
	sameSnapshot(t, "window", window.snap, through.snap)
	if window.state != through.state {
		t.Errorf("the journals ended at different watermarks: window %+v, through %+v", window.state, through.state)
	}
	if window.stats.Inodes != through.stats.Inodes || window.stats.Seq != through.stats.Seq {
		t.Errorf("the volumes ended differently: window inodes %d seq %d, through inodes %d seq %d",
			window.stats.Inodes, window.stats.Seq, through.stats.Inodes, through.stats.Seq)
	}
	if window.scrap.Pages >= window.scrap.RefPages {
		t.Errorf("the layer wrote %d pages for %d the journal asked for: it removed nothing",
			window.scrap.Pages, window.scrap.RefPages)
	}
	if window.scrap.Bytes != window.scrap.RefBytes {
		t.Errorf("the layer wrote %d bytes for %d the journal asked for: it changed the stream",
			window.scrap.Bytes, window.scrap.RefBytes)
	}
	if through.scrap.Pages != through.scrap.RefPages || through.scrap.Bytes != through.scrap.RefBytes {
		t.Errorf("through wrote %d pages and %d B for %d pages and %d B: the baseline is not the identity",
			through.scrap.Pages, through.scrap.Bytes, through.scrap.RefPages, through.scrap.RefBytes)
	}
}

// TestScrapJournalAfterACrashReplaysNothingTwice: a Volume that closes with nothing unpublished
// leaves no journal behind, and the next Volume must not replay what the store already holds.
func TestScrapJournalAfterACleanClose(t *testing.T) {
	be := storetest.NewMem()
	c := vfsAge(t)
	l := &layers{}
	mk := func(round uint64) vfs.Config {
		return vfs.Config{
			Backend: be, Cipher: c, Round: round,
			Journal: storetest.Journals(), JournalCapacity: 64 << 20, Disk: l.disk(wsbuffer.DefaultRun),
		}
	}
	if err := vfs.Create(ctx, mk(0)); err != nil {
		t.Fatal(err)
	}
	v, err := vfs.Attach(ctx, mk(1))
	if err != nil {
		t.Fatal(err)
	}
	scrapWorkload(t, v)
	flush(t, v)
	want := snapshot(t, v)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	w, err := vfs.Attach(ctx, mk(2))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if r := w.Recovery(); r.Replayed != 0 {
		t.Errorf("replayed %d changes of a journal that ended with everything published", r.Replayed)
	}
	sameSnapshot(t, "after a clean close", snapshot(t, w), want)
}
