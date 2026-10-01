package health

import (
	"sync"
	"sync/atomic"
	"testing"
)

// A lag recompute that reads an observation while another goroutine records
// one must see both fields from the same record; a new time paired with an
// old network head understates the lag.
func TestHeadObservationLoadNeverTearsAPair(t *testing.T) {
	var obs headObservation
	obs.record(1, 10)

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int64) {
			defer wg.Done()
			for i := int64(1); !stop.Load(); i++ {
				obs.record(i*2+w, (i*2+w)*10)
			}
		}(int64(w))
	}

	torn := 0
	for i := 0; i < 2_000_000 && torn == 0; i++ {
		if at, head := obs.load(); head != at*10 {
			torn++
			t.Errorf("torn observation: atMs=%d networkHead=%d", at, head)
		}
	}
	stop.Store(true)
	wg.Wait()
}
