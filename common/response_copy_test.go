package common

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// copyDeadline bounds the deadlock tests below. A regression must fail in
// seconds. A test that finds a deadlock by hanging looks like a slow pass
// until the whole run times out.
const copyDeadline = 5 * time.Second

// runBounded runs fn on its own goroutine and fails the test if fn does not
// return within copyDeadline. A goroutine in a self-deadlock cannot be
// stopped, so the test leaves it behind and still reports the failure.
func runBounded(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(copyDeadline):
		t.Fatalf("%s did not return within %s", what, copyDeadline)
	}
}

func unparsedResponse(t *testing.T, body string) *NormalizedResponse {
	t.Helper()
	r := NewNormalizedResponse().
		WithBody(io.NopCloser(strings.NewReader(body))).
		WithExpectedSize(len(body))
	require.Nil(t, r.jsonRpcResponse.Load(), "the fixture must start unparsed")
	return r
}

// Multiplexing hands one shared response to many waiting requests. The first
// waiter to copy it finds it unparsed. Copying an unparsed response must
// complete and give the new request its own id.
func TestCopyResponseForRequest_UnparsedOriginalCompletes(t *testing.T) {
	ctx := context.Background()

	orig := unparsedResponse(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
	req := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{JSONRPC: "2.0", ID: 77, Method: "eth_blockNumber"})

	var copied *NormalizedResponse
	var cerr error
	runBounded(t, "CopyResponseForRequest on an unparsed response", func() {
		copied, cerr = CopyResponseForRequest(ctx, orig, req)
	})
	require.NoError(t, cerr)
	require.NotNil(t, copied)

	jrr, err := copied.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 77, jrr.ID())
	require.Equal(t, `"0x64"`, jrr.GetResultString())

	// The copy must release the lock and leave the original parsed and
	// readable for the next waiter.
	origJrr, err := orig.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, origJrr.ID())
	require.Nil(t, orig.Request(), "Request() takes the same read lock and must not block")
}

// A response with no body is also unparsed. Its copy must carry the parse
// error, not hang.
func TestCopyResponseForRequest_BodylessOriginalCompletes(t *testing.T) {
	ctx := context.Background()

	orig := NewNormalizedResponse()
	req := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{JSONRPC: "2.0", ID: 1, Method: "eth_call"})

	var copied *NormalizedResponse
	var cerr error
	runBounded(t, "CopyResponseForRequest on a bodyless response", func() {
		copied, cerr = CopyResponseForRequest(ctx, orig, req)
	})
	require.NoError(t, cerr)
	require.NotNil(t, copied)
	jrr, err := copied.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.NotNil(t, jrr.Error, "the parse failure must survive the copy")
}

// Many waiting requests copy one shared response at the same time. The copy
// takes a read lock, so a copy must not block another reader or another copy.
// The long-lived reader below makes this sharp: a copy that took the write
// lock would queue behind it and never finish.
func TestCopyResponseForRequest_FanOutDoesNotSerialise(t *testing.T) {
	ctx := context.Background()

	orig := unparsedResponse(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
	_, err := orig.JsonRpcResponse(ctx)
	require.NoError(t, err)

	// Stand in for the rest of the pipeline that still reads the response.
	orig.RLock()
	defer orig.RUnlock()

	const waiters = 16
	ids := make([]interface{}, waiters)
	errs := make([]error, waiters)

	runBounded(t, "a 16-way multiplexed fan-out", func() {
		var wg sync.WaitGroup
		wg.Add(waiters)
		for i := 0; i < waiters; i++ {
			go func(i int) {
				defer wg.Done()
				req := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{
					JSONRPC: "2.0", ID: i + 1, Method: "eth_blockNumber",
				})
				copied, cerr := CopyResponseForRequest(ctx, orig, req)
				if cerr != nil {
					errs[i] = cerr
					return
				}
				jrr, jerr := copied.JsonRpcResponse(ctx)
				if jerr != nil {
					errs[i] = jerr
					return
				}
				ids[i] = jrr.ID()
			}(i)
		}
		wg.Wait()
	})

	for i := 0; i < waiters; i++ {
		require.NoError(t, errs[i])
		require.EqualValues(t, i+1, ids[i], "each waiter must get its own id back")
	}
}
