package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func covJsonrpcRespFromBody(body string) (*NormalizedResponse, *covJsonrpcBody) {
	b := &covJsonrpcBody{Reader: strings.NewReader(body)}
	return NewNormalizedResponse().WithBody(b).WithExpectedSize(len(body)), b
}

func TestCovJsonrpcResponse_NilReceiverAccessors(t *testing.T) {
	var r *NormalizedResponse
	ctx := context.Background()

	assert.False(t, r.FromCache())
	assert.Nil(t, r.SetFromCache(true))
	assert.Nil(t, r.EvmBlockRef())
	r.SetEvmBlockRef("0x1")
	assert.Nil(t, r.EvmBlockNumber())
	r.SetEvmBlockNumber(int64(1))
	assert.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))
	assert.Equal(t, time.Duration(0), r.Duration())
	r.SetDuration(time.Second)
	assert.Equal(t, 0, r.Attempts())
	assert.Equal(t, 0, r.Retries())
	assert.Equal(t, 0, r.Hedges())
	assert.Nil(t, r.Upstream())
	assert.Equal(t, "", r.UpstreamId())
	assert.Nil(t, r.SetUpstream(&covJsonrpcUpstream{id: "a"}))
	assert.Nil(t, r.WithRequest(NewNormalizedRequest(nil)))
	assert.Nil(t, r.WithFromCache(true))
	assert.Nil(t, r.WithFinality(DataFinalityStateFinalized))
	jrr, err := r.JsonRpcResponse(ctx)
	assert.Nil(t, jrr)
	assert.NoError(t, err)
	assert.Nil(t, r.WithBody(io.NopCloser(strings.NewReader(""))))
	assert.Nil(t, r.WithExpectedSize(10))
	r.AddRef()
	r.DoneRef()
	assert.Nil(t, r.Request())
	r.MarkIntegrityRejected()
	assert.False(t, r.IsIntegrityRejected())
	assert.True(t, r.IsObjectNull(ctx))
	assert.True(t, r.IsResultEmptyish(ctx), "nil response has no result")
	n, err := r.WriteTo(&bytes.Buffer{})
	assert.Zero(t, n)
	assert.ErrorContains(t, err, "unexpected nil response")
	r.Release()
	r.closeBodyOnce()
	h, err := r.Hash()
	assert.Equal(t, "", h)
	assert.NoError(t, err)
	h, err = r.HashWithIgnoredFields([]string{"a"})
	assert.Equal(t, "", h)
	assert.NoError(t, err)
	sz, err := r.Size()
	assert.Zero(t, sz)
	assert.NoError(t, err)
}

func TestCovJsonrpcResponse_CountersAndFlags(t *testing.T) {
	r := NewNormalizedResponse()
	// Unset counters read as zero.
	assert.Equal(t, 0, r.Attempts())
	assert.Equal(t, 0, r.Retries())
	assert.Equal(t, 0, r.Hedges())

	assert.Same(t, r, r.SetAttempts(3).SetRetries(2).SetHedges(1))
	assert.Equal(t, 3, r.Attempts())
	assert.Equal(t, 2, r.Retries())
	assert.Equal(t, 1, r.Hedges())

	r.SetDuration(150 * time.Millisecond)
	assert.Equal(t, 150*time.Millisecond, r.Duration())

	assert.False(t, r.FromCache())
	r.SetFromCache(true)
	assert.True(t, r.FromCache())
	r.WithFromCache(false)
	assert.False(t, r.FromCache())

	// nil values never overwrite stored block refs/numbers.
	r.SetEvmBlockRef("0xabc")
	r.SetEvmBlockRef(nil)
	assert.Equal(t, "0xabc", r.EvmBlockRef())
	r.SetEvmBlockNumber(int64(42))
	r.SetEvmBlockNumber(nil)
	assert.Equal(t, int64(42), r.EvmBlockNumber())

	assert.False(t, r.IsIntegrityRejected())
	r.MarkIntegrityRejected()
	assert.True(t, r.IsIntegrityRejected())
}

func TestCovJsonrpcResponse_UpstreamAccessors(t *testing.T) {
	r := NewNormalizedResponse()
	assert.Nil(t, r.Upstream())
	assert.Equal(t, "", r.UpstreamId())

	r.SetUpstream(nil)
	assert.Nil(t, r.Upstream(), "nil upstream must not be stored")

	noCfg := &covJsonrpcUpstream{id: "no-cfg"}
	r.SetUpstream(noCfg)
	assert.Same(t, noCfg, r.Upstream())
	assert.Equal(t, "", r.UpstreamId(), "upstream without config has no reportable id")

	r2 := NewNormalizedResponse().SetUpstream(&covJsonrpcUpstream{id: "alchemy", cfg: &UpstreamConfig{Id: "alchemy"}})
	assert.Equal(t, "alchemy", r2.UpstreamId())
}

func TestCovJsonrpcResponse_Finality(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit finality wins and is cached", func(t *testing.T) {
		r := NewNormalizedResponse().WithFinality(DataFinalityStateUnfinalized)
		assert.Equal(t, DataFinalityStateUnfinalized, r.Finality(ctx))
	})

	t.Run("no request yields unknown", func(t *testing.T) {
		assert.Equal(t, DataFinalityStateUnknown, NewNormalizedResponse().Finality(ctx))
	})

	t.Run("realtime request finality is adopted", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_gasPrice"}`))
		req.finality.Store(DataFinalityStateRealtime)
		r := NewNormalizedResponse().WithRequest(req)
		assert.Equal(t, DataFinalityStateRealtime, r.Finality(ctx))
	})

	t.Run("network finality is cached only when definitive", func(t *testing.T) {
		nw := &covJsonrpcNetwork{id: "evm:1", finality: DataFinalityStateFinalized}
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))
		req.SetNetwork(nw)
		r := NewNormalizedResponse().WithRequest(req)
		assert.Equal(t, DataFinalityStateFinalized, r.Finality(ctx))
		calls := nw.finalityCalls
		assert.Equal(t, DataFinalityStateFinalized, r.Finality(ctx))
		assert.Equal(t, calls, nw.finalityCalls, "definitive finality must be served from cache")

		nwU := &covJsonrpcNetwork{id: "evm:1", finality: DataFinalityStateUnknown}
		reqU := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"trace_transaction"}`))
		reqU.SetNetwork(nwU)
		rU := NewNormalizedResponse().WithRequest(reqU)
		assert.Equal(t, DataFinalityStateUnknown, rU.Finality(ctx))
		before := nwU.finalityCalls
		assert.Equal(t, DataFinalityStateUnknown, rU.Finality(ctx))
		assert.Greater(t, nwU.finalityCalls, before, "unknown finality must be recomputed")
	})
}

func TestCovJsonrpcResponse_JsonRpcResponseParsing(t *testing.T) {
	ctx := context.Background()

	t.Run("valid body is parsed once and body closed", func(t *testing.T) {
		r, body := covJsonrpcRespFromBody(`{"jsonrpc":"2.0","id":7,"result":"0x10"}`)
		jrr, err := r.JsonRpcResponse(ctx)
		require.NoError(t, err)
		assert.Equal(t, `"0x10"`, jrr.GetResultString())
		assert.Equal(t, int64(7), jrr.ID())
		assert.Equal(t, 1, body.closed)

		again, err := r.JsonRpcResponse()
		require.NoError(t, err)
		assert.Same(t, jrr, again)
		r.Release()
		assert.Equal(t, 1, body.closed, "body is closed exactly once")
	})

	t.Run("invalid body yields parse exception with raw text as data", func(t *testing.T) {
		r, _ := covJsonrpcRespFromBody(`<html>bad gateway</html>`)
		jrr, err := r.JsonRpcResponse(ctx)
		require.NoError(t, err)
		require.NotNil(t, jrr.Error)
		assert.Equal(t, int(JsonRpcErrorParseException), jrr.Error.Code)
		assert.Contains(t, jrr.Error.Message, "cannot parse json-rpc response")
		assert.Equal(t, `<html>bad gateway</html>`, jrr.Error.Data)
	})

	t.Run("missing body yields parse exception", func(t *testing.T) {
		jrr, err := NewNormalizedResponse().JsonRpcResponse()
		require.NoError(t, err)
		require.NotNil(t, jrr.Error)
		assert.Equal(t, int(JsonRpcErrorParseException), jrr.Error.Code)
		assert.Equal(t, "no body available to parse JsonRpcResponse", jrr.Error.Message)
	})

	t.Run("close error is logged through the network logger", func(t *testing.T) {
		covJsonrpcEnableLogs(t)
		for _, closeErr := range []error{context.Canceled, errors.New("disk on fire")} {
			var logBuf bytes.Buffer
			nw := &covJsonrpcNetwork{id: "evm:1", logger: zerolog.New(&logBuf).Level(zerolog.DebugLevel)}
			req := NewNormalizedRequest([]byte(`{"method":"eth_chainId"}`))
			req.SetNetwork(nw)
			b := &covJsonrpcBody{Reader: strings.NewReader(`{"id":1,"result":"0x1"}`), closeErr: closeErr}
			r := NewNormalizedResponse().WithRequest(req).WithBody(b)
			_, err := r.JsonRpcResponse()
			require.NoError(t, err)
			if errors.Is(closeErr, context.Canceled) {
				assert.Contains(t, logBuf.String(), "response body close on canceled request")
				assert.Contains(t, logBuf.String(), `"level":"debug"`)
			} else {
				assert.Contains(t, logBuf.String(), "failed to close response body")
				assert.Contains(t, logBuf.String(), `"level":"warn"`)
			}
		}
	})

	t.Run("close error without network is swallowed", func(t *testing.T) {
		b := &covJsonrpcBody{Reader: strings.NewReader(`{"id":1,"result":"0x1"}`), closeErr: errors.New("x")}
		r := NewNormalizedResponse().WithBody(b)
		jrr, err := r.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0x1"`, jrr.GetResultString())
		assert.Equal(t, 1, b.closed)
	})
}

func TestCovJsonrpcResponse_EmptyishAndObjectNull(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		jrr        *JsonRpcResponse
		emptyish   bool
		objectNull bool
	}{
		{"real result", MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`"0x1"`), nil), false, false},
		{"null result with id", MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`null`), nil), true, false},
		{"empty array", MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`[]`), nil), true, false},
		// IsObjectNull compares ID() to untyped 0 (an int), so only a typed int
		// zero id with no result and no error is reported as a null object.
		{"empty with int zero id", &JsonRpcResponse{id: 0}, true, true},
		{"error only", &JsonRpcResponse{Error: NewErrJsonRpcExceptionExternal(-32000, "x", "")}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewNormalizedResponse().WithJsonRpcResponse(tc.jrr)
			assert.Equal(t, tc.emptyish, r.IsResultEmptyish(ctx))
			assert.Equal(t, tc.objectNull, r.IsObjectNull(ctx))
			assert.Equal(t, tc.objectNull, r.IsObjectNull())
		})
	}
}

func TestCovJsonrpcResponse_MarshalAndWriteTo(t *testing.T) {
	r := NewNormalizedResponse()
	b, err := r.MarshalJSON()
	assert.Nil(t, b)
	assert.NoError(t, err)
	_, err = r.WriteTo(&bytes.Buffer{})
	assert.ErrorContains(t, err, "unexpected empty response")

	jrr := MustNewJsonRpcResponseFromBytes([]byte(`5`), []byte(`{"a":1}`), nil)
	r.WithJsonRpcResponse(jrr)
	var buf bytes.Buffer
	n, err := r.WriteTo(&buf)
	require.NoError(t, err)
	assert.Equal(t, `{"jsonrpc":"2.0","id":5,"result":{"a":1}}`, buf.String())
	assert.Equal(t, int64(buf.Len()), n)

	// JsonRpcResponse forbids MarshalJSON, so the normalized wrapper surfaces it.
	_, err = r.MarshalJSON()
	assert.ErrorContains(t, err, "MarshalJSON must not be used")
}

func TestCovJsonrpcResponse_HashAndSize(t *testing.T) {
	r := NewNormalizedResponse().WithJsonRpcResponse(
		MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"b":"0x2","a":"0x1","ts":"0x5"}`), nil))
	h, err := r.Hash(context.Background())
	require.NoError(t, err)
	assert.Len(t, h, 64)

	other := NewNormalizedResponse().WithJsonRpcResponse(
		MustNewJsonRpcResponseFromBytes([]byte(`2`), []byte(`{"a":"0x1","b":"0x2","ts":"0x9"}`), nil))
	oh, err := other.Hash()
	require.NoError(t, err)
	assert.NotEqual(t, h, oh)

	ignore := []string{"ts"}
	hi, err := r.HashWithIgnoredFields(ignore)
	require.NoError(t, err)
	ohi, err := other.HashWithIgnoredFields(ignore)
	require.NoError(t, err)
	assert.Equal(t, hi, ohi, "ignoring the differing field equalizes hashes")

	sz, err := r.Size()
	require.NoError(t, err)
	assert.Equal(t, len(`{"b":"0x2","a":"0x1","ts":"0x5"}`), sz)
}

func TestCovJsonrpcResponse_MarshalZerologObject(t *testing.T) {
	covJsonrpcEnableLogs(t)
	render := func(r *NormalizedResponse) string {
		var buf bytes.Buffer
		lg := zerolog.New(&buf)
		lg.Info().Object("resp", r).Send()
		return buf.String()
	}

	var nilResp *NormalizedResponse
	assert.Contains(t, render(nilResp), `"resp":{}`)

	r := NewNormalizedResponse().SetAttempts(2).SetRetries(1).SetHedges(1)
	out := render(r)
	assert.Contains(t, out, `"upstream":"nil"`)
	assert.Contains(t, out, `"attempts":2`)
	assert.NotContains(t, out, `"jsonRpc"`)

	r.SetUpstream(&covJsonrpcUpstream{id: "x"})
	assert.Regexp(t, `"upstream":"0x[0-9a-f]+"`, render(r), "upstream without config is logged by pointer")

	r2 := NewNormalizedResponse().
		SetUpstream(&covJsonrpcUpstream{id: "up-1", cfg: &UpstreamConfig{Id: "up-1"}}).
		WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), nil))
	r2.SetFromCache(true)
	out2 := render(r2)
	assert.Contains(t, out2, `"upstream":"up-1"`)
	assert.Contains(t, out2, `"fromCache":true`)
	assert.Contains(t, out2, `"jsonRpc":{"id":1,"resultSize":5,"result":"0x1"}`)
}

func TestCovJsonrpcResponse_ReleaseWaitsForRefs(t *testing.T) {
	jrr := MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), nil)
	req := NewNormalizedRequest([]byte(`{"method":"eth_chainId"}`))
	r := NewNormalizedResponse().WithJsonRpcResponse(jrr).WithRequest(req)
	r.AddRef()

	released := make(chan struct{})
	go func() {
		r.Release()
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("Release returned while a ref was still held")
	case <-time.After(20 * time.Millisecond):
	}
	r.DoneRef()
	<-released

	assert.Nil(t, r.Request(), "release detaches the request")
	assert.Nil(t, jrr.GetResultBytes(), "release frees the json-rpc result")
	_, err := r.WriteTo(&bytes.Buffer{})
	assert.ErrorContains(t, err, "unexpected empty response")
	r.Release() // idempotent
}

func TestCovJsonrpcResponse_LookupResponseMetadata(t *testing.T) {
	assert.Nil(t, LookupResponseMetadata(nil))
	assert.Nil(t, LookupResponseMetadata(errors.New("plain")))

	// StandardError without a cause and without metadata.
	assert.Nil(t, LookupResponseMetadata(NewErrNotImplemented("x")))

	// A NormalizedResponse carried as a cause is found through the chain.
	resp := NewNormalizedResponse().SetAttempts(3).SetRetries(2)
	md := LookupResponseMetadata(&covJsonrpcMetaErr{NormalizedResponse: resp})
	require.NotNil(t, md)
	assert.Equal(t, 3, md.Attempts())

	wrapped := NewErrFailsafeRetryExceeded(ScopeNetwork, &covJsonrpcMetaErr{NormalizedResponse: resp}, nil)
	md2 := LookupResponseMetadata(wrapped)
	require.NotNil(t, md2, "metadata must be found through the cause chain")
	assert.Equal(t, 2, md2.Retries())
}

// covJsonrpcMetaErr is an error that also exposes response metadata.
type covJsonrpcMetaErr struct{ *NormalizedResponse }

func (covJsonrpcMetaErr) Error() string { return "meta" }

func TestCovJsonrpcResponse_CopyResponseForRequest(t *testing.T) {
	ctx := context.Background()

	t.Run("nil response", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"method":"eth_chainId","id":1}`))
		out, err := CopyResponseForRequest(ctx, nil, req)
		assert.Nil(t, out)
		assert.NoError(t, err)
	})

	t.Run("copies metadata and rebinds id from parsed envelope", func(t *testing.T) {
		src := NewNormalizedResponse().
			SetAttempts(2).SetRetries(1).SetHedges(1).
			SetUpstream(&covJsonrpcUpstream{id: "u", cfg: &UpstreamConfig{Id: "u"}}).
			WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`99`), []byte(`"0xabc"`), nil))
		src.SetFromCache(true)
		src.SetEvmBlockRef("0x5")
		src.SetEvmBlockNumber(int64(5))

		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","id":"client-1"}`))
		out, err := CopyResponseForRequest(ctx, src, req)
		require.NoError(t, err)
		assert.Same(t, req, out.Request())
		assert.Equal(t, 2, out.Attempts())
		assert.Equal(t, 1, out.Retries())
		assert.Equal(t, 1, out.Hedges())
		assert.True(t, out.FromCache())
		assert.Equal(t, "u", out.UpstreamId())
		assert.Equal(t, "0x5", out.EvmBlockRef())
		assert.Equal(t, int64(5), out.EvmBlockNumber())
		var buf bytes.Buffer
		_, err = out.WriteTo(&buf)
		require.NoError(t, err)
		assert.Equal(t, `{"jsonrpc":"2.0","id":"client-1","result":"0xabc"}`, buf.String())
	})

	t.Run("unparseable request is reported", func(t *testing.T) {
		src := NewNormalizedResponse().WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`1`), nil))
		req := NewNormalizedRequest([]byte(`not json`))
		_, err := CopyResponseForRequest(ctx, src, req)
		assert.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal), "got %v", err)
	})

	t.Run("source with failing result writer", func(t *testing.T) {
		jrr := &JsonRpcResponse{}
		jrr.SetResultWriter(&covJsonrpcByteWriter{writeErr: fmt.Errorf("stream broken")})
		src := NewNormalizedResponse().WithJsonRpcResponse(jrr)
		req := NewNormalizedRequest([]byte(`{"method":"eth_x","id":1}`))
		_, err := CopyResponseForRequest(ctx, src, req)
		assert.ErrorContains(t, err, "stream broken")
	})
}
