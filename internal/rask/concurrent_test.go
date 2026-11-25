package rask

import (
	"math/rand/v2"
	"sync"
	"testing"
)

// TestConcurrentReadersAgreeWithModel reads one index from eight goroutines and
// compares every answer with the model. Index takes no lock: like the engine's
// runs, it is written once and then published, and a reader is only safe
// because nothing writes. This test is the boundary of that claim, not a wider
// one — a concurrent writer is out of scope and would corrupt the log.
func TestConcurrentReadersAgreeWithModel(t *testing.T) {
	es := randExtents(rand.New(rand.NewPCG(11, 12)), 4000, pib)
	x := New(32)
	for _, e := range es {
		x.Insert(e)
	}
	m := model(es)
	q := probes(es, 8192, rand.New(rand.NewPCG(11, 13)))

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < len(q); i += 8 {
				got, ok := x.Lookup(q[i])
				want, wantOK := m.lookup(q[i])
				if ok != wantOK || (ok && got != want) {
					t.Errorf("Lookup(%d) = %v,%v; want %v,%v", q[i], got, ok, want, wantOK)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
