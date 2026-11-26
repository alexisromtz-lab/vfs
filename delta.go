package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"sync"

	"github.com/hanzoai/vfs/internal/blake3"
	"github.com/hanzoai/vfs/pkg/backend"
	"github.com/hanzoai/vfs/pkg/fence"
)

// A delta restore makes a second store hold a checkpoint a first store already
// holds, copying only the objects the second one lacks.
//
// The metadata tree is a Merkle tree of immutable shards: two trees that name the
// same ref name the same subtree, byte for byte. So a restore does not diff
// records: it walks the two trees in lockstep and stops at every pair of equal
// refs, which is a subtree it does not read and does not copy. Where the trees
// diverge, the shards of the newer one are copied, and the chunk records they hold
// name the packs it needs; a pack the older tree's leaves also name is already
// there.
//
// Nothing is opened, re-sealed, re-packed or renamed on the way: a shard is
// written under the name its bytes hash to, which is the name it has, and a pack
// under its own id. The target therefore holds exactly the source's ciphertext, and
// a chunk read from it is opened and checked against its own hash as it would be
// from the source. The one object whose bytes differ is the Head: it is sealed to
// its place (docs/SPECIFICATION), so the restore seals the source's Head to the
// target's place and commits it there at a Round of the caller's choosing.
//
// The planner reads and opens the metadata it walks, since a ref says nothing
// about the shard under it; the key that opens the metadata is the one the caller
// already has. The objects it copies are not opened.

// ErrDelta is returned for a delta between ends that are not of one Volume: shards
// and chunks are sealed to the Volume that wrote them, so a copy between two
// Volumes would not open at its destination.
var ErrDelta = errors.New("vfs: the ends of a delta are not of one Volume")

// Side is one end of a delta: the store it reads through a Volume, and the
// checkpoint of it that takes part. The zero At means the end holds nothing, so a
// zero Side is an empty store and the delta is a full copy.
type Side struct {
	Volume *Volume
	At     Head
}

// Checkpoint returns the committed Head of the Volume: the state of it that a
// delta restore between stores is measured against. A change that has been
// applied but not published is not in it.
func (v *Volume) Checkpoint() Head {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.head
}

// Plan is what a delta restore copies. It is names and counts: what the transfer
// costs is measured as it happens (Transfer), never predicted from the tree.
type Plan struct {
	From, To Head

	// Shards are the keys (meta/<2 hex>/<hex>) of the metadata shards To reaches
	// and From does not, and Packs the keys (pack/<2 hex>/<hex>) of the packs the
	// chunk records in those shards name and the ones in From's leaves do not.
	// Both are sorted, so a plan of the same two checkpoints is the same plan.
	Shards, Packs []string

	Pruned  int // subtrees the walk skipped because both ends name the same ref
	Leaves  int // leaves the walk compared record by record
	Height  int // levels of To's tree
	Streams int // objects Apply copies at once
}

// Objects is the number of objects Apply copies.
func (p *Plan) Objects() int { return len(p.Shards) + len(p.Packs) }

// Transfer is what Apply did: the objects it read from the source and wrote to the
// target, and the bytes of them. It is measured at the objects, not at the tree.
type Transfer struct {
	Shards, Packs int
	Bytes         int64
}

// PlanDelta plans the copy that makes the store of to hold to's checkpoint, given
// that the store of from holds from's. Either Volume may be the read-only view of
// a store; the two may be the same Volume at two checkpoints of one store, which is
// what a store that keeps its history can answer.
//
// from is a Side{} for a full copy. The copy is complete only if the store that
// holds from is whole, which `vfs reseal --verify` checks.
func PlanDelta(ctx context.Context, from, to Side) (*Plan, error) {
	if to.Volume == nil {
		return nil, fmt.Errorf("%w: the target names no store", ErrDelta)
	}
	if from.At.Root != none && from.Volume == nil {
		return nil, fmt.Errorf("%w: the source names a checkpoint but no store", ErrDelta)
	}
	at := to.At
	if at == (Head{}) {
		at = to.Volume.Checkpoint()
	}
	p := &Plan{From: from.At, To: at, Streams: to.Volume.cfg.Streams}
	var fromShards *shards
	var a ref
	if from.Volume != nil {
		if from.Volume.id != to.Volume.id || from.Volume.head.Seed != to.Volume.head.Seed {
			return nil, fmt.Errorf("%w: %x and %x", ErrDelta, from.Volume.id[:6], to.Volume.id[:6])
		}
		if from.At == (Head{}) {
			from.At = from.Volume.Checkpoint()
			p.From = from.At
		}
		fromShards, a = from.Volume.t.sh, from.At.Root
	}
	p.From = from.At
	return planDelta(ctx, fromShards, to.Volume.t.sh, a, at.Root, p)
}

// planDelta walks the tree a of from against the tree b of to, through the shards
// that hold each, and fills p. A nil from, or a zero a, is an empty source.
func planDelta(ctx context.Context, from, to *shards, a, b ref, p *Plan) (*Plan, error) {
	w := &walker{ctx: ctx, from: from, to: to, plan: p, seen: map[ref]struct{}{}, packs: map[packID]struct{}{}}
	if err := w.walk(a, b, true); err != nil {
		return nil, err
	}
	sort.Strings(p.Shards)
	sort.Strings(p.Packs)
	return p, nil
}

// walker walks two trees in lockstep. It is one goroutine: a walk is reads of
// metadata and the plan it leaves is a set of names, so the cost of a restore is in
// the copying, which Apply does in parallel.
type walker struct {
	ctx  context.Context
	from *shards // nil when the source holds nothing
	to   *shards
	plan *Plan

	seen  map[ref]struct{}
	packs map[packID]struct{}
}

// walk visits b against a: the pair (a, b) is the same subtree of both trees, or a
// is none.
func (w *walker) walk(a, b ref, top bool) error {
	if b == none {
		return nil
	}
	if a == b {
		w.plan.Pruned++
		return nil
	}
	bn, err := w.to.load(w.ctx, b)
	if err != nil {
		return err
	}
	if top {
		w.plan.Height = bn.level + 1
	}
	// an is the other end's node at the same place, if it has one at this level. A
	// tree that changed height has no node to compare with, and everything below is
	// copied: a plan copies more than it must, never less.
	var an *node
	if a != none && w.from != nil {
		if an, err = w.from.load(w.ctx, a); err != nil {
			return err
		}
		if an.level != bn.level {
			an = nil
		}
	}
	w.addShard(b)
	if bn.level == 0 {
		w.plan.Leaves++
		return w.addPacks(bn, an)
	}
	for i, k := range bn.keys {
		ac := none
		if an != nil {
			ac = refOf(an.vals[child(an, k)])
		}
		if err := w.walk(ac, refOf(bn.vals[i]), false); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) addShard(r ref) {
	if _, ok := w.seen[r]; ok {
		return
	}
	w.seen[r] = struct{}{}
	w.plan.Shards = append(w.plan.Shards, shardKey(r))
}

// addPacks records the packs the chunk records of bn name, less the ones an names:
// a pack that a leaf of the other end also points into is reachable from it, so the
// target already holds it.
func (w *walker) addPacks(bn, an *node) error {
	var have map[packID]struct{}
	if an != nil {
		var err error
		if have, err = chunkPacks(an); err != nil {
			return err
		}
	}
	need, err := chunkPacks(bn)
	if err != nil {
		return err
	}
	for id := range need {
		if _, ok := have[id]; ok {
			continue
		}
		if _, ok := w.packs[id]; ok {
			continue
		}
		w.packs[id] = struct{}{}
		w.plan.Packs = append(w.plan.Packs, packKey(id))
	}
	return nil
}

// chunkPacks returns the packs named by the chunk records of a leaf.
func chunkPacks(n *node) (map[packID]struct{}, error) {
	var out map[packID]struct{}
	for i, k := range n.keys {
		if len(k) != 1+len(chunkID{}) || k[0] != tChunk {
			continue
		}
		where, err := decodeWhere(n.vals[i])
		if err != nil {
			return nil, fmt.Errorf("vfs: chunk record %x: %w", k[1:7], err)
		}
		if out == nil {
			out = make(map[packID]struct{})
		}
		out[where.pack] = struct{}{}
	}
	return out, nil
}

// refOf reads a ref out of a value that holds one.
func refOf(b []byte) (r ref) {
	copy(r[:], b)
	return r
}

// Apply copies the plan's objects from src to dst, verbatim, Streams of them at a
// time. A shard is checked against its name before it is written: its bytes hash to
// the ref under which it lives, so a read that came back wrong is refused instead
// of being copied into the target under a name that promises other bytes. A pack is
// not checked here; every chunk in it is checked against its own hash when it is
// read (docs/SPECIFICATION).
func (p *Plan) Apply(ctx context.Context, src, dst backend.Backend) (Transfer, error) {
	if src == nil || dst == nil {
		return Transfer{}, fmt.Errorf("vfs: delta apply needs a source and a target store")
	}
	jobs := make(chan obj, 16)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		tr    Transfer
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	workers := p.Streams
	if workers <= 0 {
		workers = 8
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range jobs {
				if ctx.Err() != nil {
					return
				}
				data, _, err := src.Get(ctx, o.key, o.max)
				if err != nil {
					fail(fmt.Errorf("vfs: delta read %s: %w", o.key, err))
					return
				}
				if o.shard {
					if shardKey(blake3.Sum(data)) != o.key {
						fail(fmt.Errorf("%w: shard %s does not hash to its name", ErrCorrupt, o.key))
						return
					}
				}
				if err := dst.Put(ctx, o.key, data); err != nil {
					fail(fmt.Errorf("vfs: delta write %s: %w", o.key, err))
					return
				}
				mu.Lock()
				tr.Bytes += int64(len(data))
				if o.shard {
					tr.Shards++
				} else {
					tr.Packs++
				}
				mu.Unlock()
			}
		}()
	}
	for _, o := range p.objects() {
		select {
		case jobs <- o:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return tr, first
	}
	return tr, nil
}

// obj is one object to copy: its key, the largest it can be, and whether it is a
// shard, whose bytes are checked against its name.
type obj struct {
	key   string
	max   int64
	shard bool
}

func (p *Plan) objects() []obj {
	out := make([]obj, 0, p.Objects())
	for _, k := range p.Shards {
		out = append(out, obj{key: k, max: maxShard, shard: true})
	}
	for _, k := range p.Packs {
		out = append(out, obj{key: k, max: packTarget + packRoom})
	}
	return out
}

// Commit writes the plan's Head at cfg.Backend's place, sealed there, claiming that
// place at cfg.Round. The Round must exceed the one recorded where the Head goes,
// as a claim does: a restore onto a store that already holds a checkpoint writes
// Head + 1, and one onto an empty store writes Head 1.
//
// The objects the Head names must be there: Apply copies them, and the Head is the
// last thing written, so a restore that fails part way leaves the target at the
// checkpoint it had.
func (p *Plan) Commit(ctx context.Context, cfg Config) error {
	cfg, err := cfg.valid()
	if err != nil {
		return err
	}
	if cfg.Round == 0 || cfg.Round == math.MaxUint64 {
		return fmt.Errorf("%w: Commit needs a Round above 0 and below 2^64-1", fs.ErrInvalid)
	}
	sealed, err := seal(cfg.Cipher, headAAD(cfg.Backend.String()), p.To.marshal())
	if err != nil {
		return err
	}
	return fence.New(cfg.Backend, maxHead).Rewrite(ctx, headKey, cfg.Round, func(cur []byte, recorded uint64) ([]byte, error) {
		if cur != nil && cfg.Round <= recorded {
			return nil, fmt.Errorf("%w: round %d does not exceed the recorded %d", ErrStaleRound, cfg.Round, recorded)
		}
		return sealed, nil
	})
}

// Restore brings the store of dst to the checkpoint the store of src holds, copying
// only what dst lacks, and commits it at dst.Round. It is PlanDelta, Apply and
// Commit in that order: a plan between the two checkpoints, the copy of the objects
// it names, and the Head last.
//
// A dst that holds no Volume is a full copy. Neither store is closed: Config owns
// its Backend, and the caller does.
func Restore(ctx context.Context, dst, src Config) (*Plan, Transfer, error) {
	to, err := Open(ctx, src)
	if err != nil {
		return nil, Transfer{}, fmt.Errorf("vfs: delta source: %w", err)
	}
	from := Side{}
	if v, err := Open(ctx, dst); err == nil {
		from = Side{Volume: v}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, Transfer{}, fmt.Errorf("vfs: delta target: %w", err)
	}
	p, err := PlanDelta(ctx, from, Side{Volume: to})
	if err != nil {
		return nil, Transfer{}, err
	}
	tr, err := p.Apply(ctx, src.Backend, dst.Backend)
	if err != nil {
		return p, tr, err
	}
	if err := p.Commit(ctx, dst); err != nil {
		return p, tr, err
	}
	return p, tr, nil
}
