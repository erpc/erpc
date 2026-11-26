package clients

import (
	"bytes"
	"sync"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
)

// renderPayload is receipts unique to i, of a size that grows with i, so a
// result that shared memory with another render would differ from want.
func renderPayload(i int) []*evm.Receipt {
	rs := make([]*evm.Receipt, 1+i%5)
	for j := range rs {
		rs[j] = &evm.Receipt{
			TransactionHash:   bytes.Repeat([]byte{byte(i)}, 32),
			BlockNumber:       uint64(i),
			TransactionIndex:  uint32(j),
			EffectiveGasPrice: "1000000000",
			Logs: []*evm.Log{{
				Address: bytes.Repeat([]byte{byte(i + j)}, 20),
				Data:    bytes.Repeat([]byte{byte(i)}, 1+i*97),
			}},
		}
	}
	return rs
}

// renderResult hands out buffers from a pool. Each result must own its
// memory: one aliasing the scratch buffer would carry one request's data into
// another request's response once the buffer is reused.
func TestRenderResult_ConcurrentResultsStayIntact(t *testing.T) {
	const payloads, workers, rounds = 48, 32, 64
	in := make([][]*evm.Receipt, payloads)
	want := make([][]byte, payloads)
	for i := range in {
		in[i] = renderPayload(i)
		b, err := evm.AppendReceiptsJsonRpc(nil, in[i])
		if err != nil {
			t.Fatal(err)
		}
		want[i] = b
	}

	type kept struct {
		i   int
		out []byte
	}
	results := make([][]kept, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range rounds {
				i := (w*7 + k*13) % payloads
				var err error
				out := renderResult(func(b []byte) []byte {
					b, err = evm.AppendReceiptsJsonRpc(b, in[i])
					return b
				})
				if err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(out, want[i]) {
					t.Errorf("worker %d round %d: payload %d rendered wrong", w, k, i)
					return
				}
				results[w] = append(results[w], kept{i, out})
			}
		}()
	}
	wg.Wait()

	// Every result handed out must still hold its own payload after the pool
	// has reused the scratch buffers for later renders.
	for w, ks := range results {
		for _, k := range ks {
			if !bytes.Equal(k.out, want[k.i]) {
				t.Fatalf("worker %d: a payload %d result changed after its render", w, k.i)
			}
		}
	}
}
