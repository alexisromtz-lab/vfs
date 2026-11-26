// Copyright 2026 Hanzo AI Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

// Command deltabench measures a delta restore against a full one (HIP-1344,
// Refs #64): the objects and bytes each moves, the time each takes and the CPU it
// spends. Each row builds a Volume of -files files of -size bytes, takes a
// checkpoint, writes -change percent of the files again, and brings a store to the
// second checkpoint twice — once from nothing (a full copy) and once from a store
// that already holds the first (a delta).
//
//	go run ./tools/deltabench
//	go run ./tools/deltabench -files 600 -size 49152 -change 0,1,5,20,100
//	go run ./tools/deltabench -csv docs/parasync.csv
//
// The store is an in-memory one, so the bytes and the object counts are exact and
// what a network would add is absent: the clock is this machine's memory and CPU, and
// it is not comparable across machines. What the table is for is the ratio between two
// restores measured in one run, interleaved, on one machine.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/luxfi/age"

	"github.com/hanzoai/vfs"
	"github.com/hanzoai/vfs/internal/storetest"
)

// row is one restore of one workload.
type row struct {
	Order   string // how the changed files are spread: spread or contiguous
	Change  int    // percent of the files written again between the checkpoints
	Mode    string // full, delta
	Files   int
	Size    int
	Streams int

	Objects, Shards, Packs, Pruned int
	Bytes                          int64

	PlanUS, ApplyUS, TotalUS, CPUUS int64
}

func main() {
	files := flag.Int("files", 600, "files in the Volume")
	size := flag.Int("size", 2048, "bytes of each file")
	changes := flag.String("change", "0,1,5,20,100", "percent of the files changed, comma separated")
	orders := flag.String("order", "spread", "how the changed files are spread: spread or contiguous")
	streams := flag.String("streams", "1,8", "objects copied at once, comma separated")
	reps := flag.Int("reps", 3, "runs of each row; the table reports the median")
	csvPath := flag.String("csv", "", "write every row to this CSV file")
	flag.Parse()

	ctx := context.Background()
	var rows []row
	for _, order := range strings.Split(*orders, ",") {
		for _, c := range numbers(*changes) {
			for _, st := range numbers(*streams) {
				var runs []row
				for range *reps {
					full, delta, err := run(ctx, *files, *size, c, order, st)
					if err != nil {
						fmt.Fprintf(os.Stderr, "deltabench: %v\n", err)
						os.Exit(1)
					}
					runs = append(runs, full, delta)
				}
				rows = append(rows, median(runs)...)
			}
		}
	}
	if *csvPath != "" {
		if err := dump(*csvPath, rows); err != nil {
			fmt.Fprintf(os.Stderr, "deltabench: %v\n", err)
			os.Exit(1)
		}
	}
	table(rows, *files, *size, *reps)
}

// run makes one workload and restores its second checkpoint twice: in full into an
// empty store, and as a delta into a store that holds the first.
func run(ctx context.Context, files, size, change int, order string, streams int) (row, row, error) {
	s, err := build(ctx, files, size, change, order)
	if err != nil {
		return row{}, row{}, err
	}
	defer s.close()
	to := vfs.Side{Volume: s.at2}

	full, err := restore(ctx, s.be, s.c, vfs.Side{}, to, storetest.NewMem(), streams, 1)
	if err != nil {
		return row{}, row{}, err
	}
	// The store of the delta holds the first checkpoint, as a replica does: bringing
	// it there is not part of the measurement.
	rep := storetest.NewMem()
	if _, err := restore(ctx, s.be, s.c, vfs.Side{}, vfs.Side{Volume: s.at1}, rep, streams, 1); err != nil {
		return row{}, row{}, err
	}
	delta, err := restore(ctx, s.be, s.c, vfs.Side{Volume: s.at1}, to, rep, streams, 2)
	if err != nil {
		return row{}, row{}, err
	}
	if delta.Objects > full.Objects {
		return row{}, row{}, fmt.Errorf("the delta copies %d objects, the full copy %d", delta.Objects, full.Objects)
	}
	base := row{Order: order, Change: change, Files: files, Size: size, Streams: streams}
	full.Mode, delta.Mode = "full", "delta"
	for _, r := range []*row{&full, &delta} {
		r.Order, r.Change, r.Files, r.Size, r.Streams = base.Order, base.Change, base.Files, base.Size, base.Streams
	}
	return full, delta, nil
}

// restore brings dst to the checkpoint of to, copying only what the checkpoint of
// from lacks, and reports what the copy cost. Only Apply and the plan are measured.
// round is the commit number the Head is written at, which has to exceed the one the
// destination recorded, so the caller keeps count of them.
func restore(ctx context.Context, src *storetest.Mem, c vfs.Cipher, from, to vfs.Side, dst *storetest.Mem, streams int, round uint64) (row, error) {
	var r row
	start := time.Now()
	p, err := vfs.PlanDelta(ctx, from, to)
	if err != nil {
		return r, err
	}
	p.Streams = streams
	r.PlanUS = time.Since(start).Microseconds()
	r.Shards, r.Packs, r.Pruned, r.Objects = len(p.Shards), len(p.Packs), p.Pruned, p.Objects()

	cpu := cpuNow()
	start = time.Now()
	tr, err := p.Apply(ctx, src, dst)
	r.ApplyUS, r.CPUUS = time.Since(start).Microseconds(), (cpuNow()-cpu)/1000
	if err != nil {
		return r, err
	}
	r.Bytes = tr.Bytes
	wrote := dst.Puts()
	dst.Reset()
	start = time.Now()
	if err := p.Commit(ctx, cfg(dst, c, round)); err != nil {
		return r, err
	}
	r.TotalUS = r.PlanUS + r.ApplyUS + time.Since(start).Microseconds()
	if wrote["meta"] != len(p.Shards) || wrote["pack"] != len(p.Packs) {
		return r, fmt.Errorf("the copy wrote %d shards and %d packs, the plan names %d and %d",
			wrote["meta"], wrote["pack"], len(p.Shards), len(p.Packs))
	}
	return r, nil
}

// volume is a store with two checkpoints of it.
type volume struct {
	be      *storetest.Mem
	c       vfs.Cipher
	v       *vfs.Volume
	at1     *vfs.Volume // read-only views of the first and the second checkpoint
	at2     *vfs.Volume
	journal string
}

func (s *volume) close() {
	if s.v != nil {
		s.v.Close()
	}
	os.RemoveAll(s.journal)
}

// build makes a Volume of n files of size bytes: at1 at that state, at2 after change
// percent of the files were written again.
func build(ctx context.Context, n, size, change int, order string) (*volume, error) {
	dir, err := os.MkdirTemp("", "deltabench-*")
	if err != nil {
		return nil, err
	}
	k, err := age.GenerateHybridIdentity()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s := &volume{be: storetest.NewMem(), c: vfs.Hybrid(k), journal: dir}
	if err := vfs.Create(ctx, cfg(s.be, s.c, 0)); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	v, err := vfs.Attach(ctx, vfs.Config{Backend: s.be, Cipher: s.c, Round: 1, Journal: dir, JournalCapacity: 64 << 20})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s.v = v
	d, err := v.Mkdir(ctx, vfs.RootInode, "d", 0o755)
	if err != nil {
		return nil, err
	}
	inos := make([]vfs.Inode, n)
	for i := range n {
		if inos[i], err = put(ctx, v, d.ID, fmt.Sprintf("f%05d", i), content(uint64(i), size)); err != nil {
			return nil, err
		}
	}
	if err := v.Flush(ctx); err != nil {
		return nil, err
	}
	// The shards of a checkpoint stay reachable after the next one commits, so a
	// read-only view of the same store serves either checkpoint of it: At names
	// which.
	if s.at1, err = vfs.Open(ctx, cfg(s.be, s.c, 0)); err != nil {
		return nil, err
	}
	for _, i := range picked(n, change, order) {
		f, err := v.Open(ctx, inos[i].ID)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteAt(ctx, content(uint64(1e6+i), size), 0); err != nil {
			return nil, err
		}
		f.Close()
	}
	if err := v.Flush(ctx); err != nil {
		return nil, err
	}
	if s.at2, err = vfs.Open(ctx, cfg(s.be, s.c, 0)); err != nil {
		return nil, err
	}
	if change > 0 && s.at1.Checkpoint().Root == s.at2.Checkpoint().Root {
		return nil, fmt.Errorf("a change of %d%% left the checkpoint as it was", change)
	}
	return s, nil
}

// picked returns the indexes of the files a change of p percent writes again: spread
// over the Volume at a stride, or the first p percent of them.
func picked(n, p int, order string) []int {
	k := n * p / 100
	out := make([]int, 0, k)
	for i := range k {
		if order == "contiguous" {
			out = append(out, i)
		} else {
			out = append(out, i*n/max(k, 1))
		}
	}
	return out
}

func put(ctx context.Context, v *vfs.Volume, dir vfs.InodeID, name string, data []byte) (vfs.Inode, error) {
	in, err := v.Create(ctx, dir, name, 0o644)
	if err != nil {
		return in, err
	}
	f, err := v.Open(ctx, in.ID)
	if err != nil {
		return in, err
	}
	defer f.Close()
	_, err = f.WriteAt(ctx, data, 0)
	return in, err
}

func cfg(be *storetest.Mem, c vfs.Cipher, round uint64) vfs.Config {
	return vfs.Config{Backend: be, Cipher: c, Round: round}
}

// median reduces the runs of a row to one, field by field, so that one slow run does
// not move a row.
func median(rs []row) []row {
	byMode := map[string][]row{}
	for _, r := range rs {
		byMode[r.Mode] = append(byMode[r.Mode], r)
	}
	var out []row
	for _, mode := range []string{"full", "delta"} {
		group := byMode[mode]
		m := group[0]
		m.Objects = midInt(group, func(r row) int { return r.Objects })
		m.Shards = midInt(group, func(r row) int { return r.Shards })
		m.Packs = midInt(group, func(r row) int { return r.Packs })
		m.Pruned = midInt(group, func(r row) int { return r.Pruned })
		m.Bytes = mid(group, func(r row) int64 { return r.Bytes })
		m.PlanUS = mid(group, func(r row) int64 { return r.PlanUS })
		m.ApplyUS = mid(group, func(r row) int64 { return r.ApplyUS })
		m.TotalUS = mid(group, func(r row) int64 { return r.TotalUS })
		m.CPUUS = mid(group, func(r row) int64 { return r.CPUUS })
		out = append(out, m)
	}
	return out
}

func mid(rs []row, get func(row) int64) int64 {
	vs := make([]int64, len(rs))
	for i := range rs {
		vs[i] = get(rs[i])
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
	return vs[len(vs)/2]
}

func midInt(rs []row, get func(row) int) int {
	vs := make([]int, len(rs))
	for i := range rs {
		vs[i] = get(rs[i])
	}
	sort.Ints(vs)
	return vs[len(vs)/2]
}

func table(rows []row, files, size, reps int) {
	fmt.Printf("Delta restore (HIP-1344, Refs #64): %d files of %d bytes, median of %d runs\n\n", files, size, reps)
	fmt.Printf("| order | change | mode | objects | shards | packs | pruned | bytes | plan ms | apply ms | restore ms | cpu ms |\n")
	fmt.Printf("|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Printf("| %s | %d%% | %s | %d | %d | %d | %d | %d | %.2f | %.2f | %.2f | %.2f |\n",
			r.Order, r.Change, r.Mode, r.Objects, r.Shards, r.Packs, r.Pruned, r.Bytes,
			ms(r.PlanUS), ms(r.ApplyUS), ms(r.TotalUS), ms(r.CPUUS))
	}
	fmt.Printf("\nthe delta as a percent of the full copy of the same checkpoint\n\n")
	fmt.Printf("| order | streams | change | objects | bytes | restore | cpu |\n|---|---:|---:|---:|---:|---:|---:|\n")
	for i := 0; i+1 < len(rows); i += 2 {
		f, d := rows[i], rows[i+1]
		fmt.Printf("| %s | %d | %d%% | %s | %s | %s | %s |\n", f.Order, f.Streams, f.Change,
			pct(int64(d.Objects), int64(f.Objects)), pct(d.Bytes, f.Bytes), pct(d.TotalUS, f.TotalUS), pct(d.CPUUS, f.CPUUS))
	}
	fmt.Printf("\nthe store is in memory: bytes and objects are exact, requests are one GET and one PUT per object,\n")
	fmt.Printf("and the clock is this machine: not a network, and not another machine.\n")
}

func dump(path string, rows []row) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"order", "change_pct", "mode", "files", "size", "streams", "objects", "shards", "packs",
		"pruned", "bytes", "plan_us", "apply_us", "restore_us", "cpu_us"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write([]string{r.Order, strconv.Itoa(r.Change), r.Mode, strconv.Itoa(r.Files), strconv.Itoa(r.Size),
			strconv.Itoa(r.Streams), strconv.Itoa(r.Objects), strconv.Itoa(r.Shards), strconv.Itoa(r.Packs), strconv.Itoa(r.Pruned),
			strconv.FormatInt(r.Bytes, 10), strconv.FormatInt(r.PlanUS, 10), strconv.FormatInt(r.ApplyUS, 10),
			strconv.FormatInt(r.TotalUS, 10), strconv.FormatInt(r.CPUUS, 10)}); err != nil {
			return err
		}
	}
	return w.Error()
}

func numbers(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			fmt.Fprintf(os.Stderr, "deltabench: %q is not a number\n", f)
			os.Exit(2)
		}
		out = append(out, n)
	}
	return out
}

func content(seed uint64, n int) []byte {
	b := make([]byte, n)
	x := seed*0x9e3779b97f4a7c15 + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x)
	}
	return b
}

// cpuNow is the CPU time this process has spent, in nanoseconds: a restore is mostly
// work on one core, so it is the column that says how much of the clock a copy burns
// rather than waits through.
func cpuNow() int64 {
	var ru syscall.Rusage
	syscall.Getrusage(0, &ru) // 0 is RUSAGE_SELF
	return ru.Utime.Nano() + ru.Stime.Nano()
}

func ms(us int64) float64 { return float64(us) / 1000 }

func pct(a, b int64) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}
