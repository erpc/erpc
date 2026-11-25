package common

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Multiplexing hands one response to many waiting clients, and each client
// renders it at the same time. This response carries a typed Error and no
// errBytes, so every WriteTo takes the marshal-on-demand branch. WriteTo
// holds read locks only, so it must not write a shared field there. Run with
// -race: the race detector catches the write.
func TestJsonRpcResponse_WriteTo_ConcurrentClientsDoNotRace(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{
		idBytes: []byte(`"abc"`),
		Error:   NewErrJsonRpcExceptionExternal(-32005, "limit exceeded", "retry later"),
	}

	const clients = 8
	start := make(chan struct{})
	outs := make([]string, clients)
	var wg sync.WaitGroup
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			var buf bytes.Buffer
			n, err := jr.WriteTo(&buf)
			assert.NoError(t, err)
			assert.Equal(t, int64(buf.Len()), n)
			outs[i] = buf.String()
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < clients; i++ {
		require.Equal(t, outs[0], outs[i], "every client must receive the same bytes")
	}
	require.Contains(t, outs[0], `-32005`)
}

// WriteTo must not cache the marshalled error in the shared errBytes field.
// A second render marshals again and gives the same bytes.
func TestJsonRpcResponse_WriteTo_DoesNotMutateErrBytes(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{
		idBytes: []byte(`1`),
		Error:   NewErrJsonRpcExceptionExternal(-32000, "boom", ""),
	}

	var first, second bytes.Buffer
	_, err := jr.WriteTo(&first)
	require.NoError(t, err)
	require.Empty(t, jr.errBytes, "WriteTo must not write shared state under a read lock")

	_, err = jr.WriteTo(&second)
	require.NoError(t, err)
	require.Equal(t, first.String(), second.String())
}
