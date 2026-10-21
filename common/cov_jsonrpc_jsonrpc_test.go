package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func covJsonrpcWrite(t *testing.T, r *JsonRpcResponse) string {
	t.Helper()
	var buf bytes.Buffer
	n, err := r.WriteTo(&buf)
	require.NoError(t, err)
	require.Equal(t, int64(buf.Len()), n)
	return buf.String()
}

func TestCovJsonrpc_NewJsonRpcResponse(t *testing.T) {
	r, err := NewJsonRpcResponse(1, map[string]string{"a": "b"}, nil)
	require.NoError(t, err)
	assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":{"a":"b"}}`, covJsonrpcWrite(t, r))

	_, err = NewJsonRpcResponse(1, math.Inf(1), nil)
	assert.Error(t, err, "unmarshalable result must be rejected")
	_, err = NewJsonRpcResponse(make(chan int), "x", nil)
	assert.Error(t, err, "unmarshalable id must be rejected")

	assert.Panics(t, func() { MustNewJsonRpcResponse(1, make(chan int), nil) })
	assert.Panics(t, func() { MustNewJsonRpcResponseFromBytes([]byte(`{`), nil, nil) })
}

func TestCovJsonrpc_NewFromBytes_IdForms(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		wantID interface{}
		errSub string
	}{
		{"number", `42`, int64(42), ""},
		{"string", `"abc"`, "abc", ""},
		{"null", `null`, nil, ""},
		{"object", `{"x":1}`, nil, "unsupported ID type"},
		{"invalid", `{`, nil, "?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewJsonRpcResponseFromBytes([]byte(tc.id), []byte(`1`), nil)
			if tc.errSub != "" {
				require.Error(t, err)
				if tc.errSub != "?" {
					assert.Contains(t, err.Error(), tc.errSub)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, r.ID())
			// Wire output keeps the id bytes verbatim.
			assert.Contains(t, covJsonrpcWrite(t, r), `"id":`+tc.id+`,`)
		})
	}

	t.Run("large id keeps verbatim bytes on wire", func(t *testing.T) {
		r, err := NewJsonRpcResponseFromBytes([]byte(`18446744073709551615`), []byte(`1`), nil)
		require.NoError(t, err)
		assert.Contains(t, covJsonrpcWrite(t, r), `"id":18446744073709551615,`)
	})

	t.Run("no id at all", func(t *testing.T) {
		r, err := NewJsonRpcResponseFromBytes(nil, []byte(`1`), nil)
		require.NoError(t, err)
		assert.Nil(t, r.ID())
	})
}

func TestCovJsonrpc_SetIDAndSetIDBytes(t *testing.T) {
	r := &JsonRpcResponse{}
	require.NoError(t, r.SetID("x-1"))
	assert.Equal(t, "x-1", r.ID())
	assert.Error(t, r.SetID(make(chan int)))

	r2 := &JsonRpcResponse{}
	require.NoError(t, r2.SetIDBytes([]byte(`1.0`)))
	assert.Equal(t, int64(1), r2.ID())
	assert.Equal(t, `{"jsonrpc":"2.0","id":1.0}`, covJsonrpcWrite(t, r2), "fractional id written verbatim")
	assert.Error(t, r2.SetIDBytes([]byte(`[1]`)))

	// ID() lazily decodes idBytes when no typed id is set.
	r3 := &JsonRpcResponse{idBytes: []byte(`"lazy"`)}
	assert.Equal(t, "lazy", r3.ID())
	r4 := &JsonRpcResponse{idBytes: []byte(`{bad`)}
	assert.Nil(t, r4.ID(), "undecodable id bytes yield nil")
}

func TestCovJsonrpc_ParseFromStream(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		r := &JsonRpcResponse{}
		err := r.ParseFromStream(nil, covJsonrpcErrReader{}, 0)
		assert.ErrorContains(t, err, "cov-jsonrpc read failed")
	})

	t.Run("invalid json keeps raw data as result", func(t *testing.T) {
		r := &JsonRpcResponse{}
		err := r.ParseFromStream([]context.Context{context.Background()}, strings.NewReader(`oops`), 4)
		require.Error(t, err)
		assert.Equal(t, `oops`, r.GetResultString())
	})

	t.Run("unsupported id type", func(t *testing.T) {
		r := &JsonRpcResponse{}
		err := r.ParseFromStream(nil, strings.NewReader(`{"id":[1],"result":1}`), 0)
		assert.ErrorContains(t, err, "unsupported ID type")
	})

	t.Run("error object", func(t *testing.T) {
		r := &JsonRpcResponse{}
		require.NoError(t, r.ParseFromStream(nil, strings.NewReader(`{"id":3,"error":{"code":-32000,"message":"execution reverted","data":"0x08c379a0"}}`), 0))
		require.NotNil(t, r.Error)
		assert.Equal(t, -32000, r.Error.Code)
		assert.Equal(t, "execution reverted", r.Error.Message)
		assert.Equal(t, "0x08c379a0", r.Error.Data)
		assert.Equal(t, `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"execution reverted","data":"0x08c379a0"}}`, covJsonrpcWrite(t, r))
	})
}

func TestCovJsonrpc_ParseError_Shapes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		code    int
		message string
		data    interface{}
		keepRaw bool
	}{
		{"standard", `{"code":-32602,"message":"bad params"}`, -32602, "bad params", nil, true},
		{"nested data", `{"code":3,"message":"reverted","data":{"x":[1]}}`, 3, "reverted", map[string]interface{}{"x": []interface{}{float64(1)}}, true},
		{"empty", ``, int(JsonRpcErrorServerSideException), "unexpected empty response from upstream endpoint", "", false},
		// A literal null error body is decoded as an all-zero object, so it
		// falls through to the raw-text message rather than the empty-response one.
		{"null", `null`, int(JsonRpcErrorServerSideException), "null", "", false},
		{"only error string", `{"error":"rate limited"}`, int(JsonRpcErrorServerSideException), "rate limited", "", false},
		{"plain string", `"boom"`, int(JsonRpcErrorServerSideException), `"boom"`, "", false},
		{"empty object", `{}`, int(JsonRpcErrorServerSideException), `{}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &JsonRpcResponse{}
			require.NoError(t, r.ParseError(tc.raw))
			require.NotNil(t, r.Error)
			assert.Equal(t, tc.code, r.Error.Code)
			assert.Equal(t, tc.message, r.Error.Message)
			assert.Equal(t, tc.data, r.Error.Data)
			if tc.keepRaw {
				assert.Equal(t, tc.raw, string(r.errBytes))
			} else {
				assert.Nil(t, r.errBytes)
			}
		})
	}

	t.Run("from bytes ctor", func(t *testing.T) {
		r, err := NewJsonRpcResponseFromBytes([]byte(`1`), nil, []byte(`{"error":"nope"}`))
		require.NoError(t, err)
		assert.Equal(t, "nope", r.Error.Message)
	})
}

func TestCovJsonrpc_PeekByPath(t *testing.T) {
	ctx := context.Background()
	r := MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"number":"0x10","tx":[{"h":"0xa"}]}`), nil)

	s, err := r.PeekStringByPath(ctx, "number")
	require.NoError(t, err)
	assert.Equal(t, "0x10", s)

	s, err = r.PeekStringByPath(ctx, "tx", 0, "h")
	require.NoError(t, err)
	assert.Equal(t, "0xa", s)

	_, err = r.PeekStringByPath(ctx, "missing")
	assert.Error(t, err)

	b, err := r.PeekBytesByPath(ctx, "tx", 0)
	require.NoError(t, err)
	assert.Equal(t, `{"h":"0xa"}`, string(b))

	_, err = r.PeekBytesByPath(ctx, "nope")
	assert.Error(t, err)

	t.Run("materializes from result writer", func(t *testing.T) {
		w := &covJsonrpcByteWriter{data: []byte(`{"blockNumber":"0x5"}`)}
		r := &JsonRpcResponse{}
		r.SetResultWriter(w)
		s, err := r.PeekStringByPath(ctx, "blockNumber")
		require.NoError(t, err)
		assert.Equal(t, "0x5", s)
		assert.Equal(t, `{"blockNumber":"0x5"}`, r.GetResultString(), "writer is materialized into result")
	})

	t.Run("writer failure is surfaced", func(t *testing.T) {
		r := &JsonRpcResponse{}
		r.SetResultWriter(&covJsonrpcByteWriter{writeErr: errors.New("writer dead")})
		_, err := r.PeekStringByPath(ctx, "x")
		assert.ErrorContains(t, err, "writer dead")
		_, err = r.PeekBytesByPath(ctx, "x")
		assert.ErrorContains(t, err, "writer dead")
	})

	t.Run("empty result", func(t *testing.T) {
		r := &JsonRpcResponse{}
		_, err := r.PeekStringByPath(ctx, "x")
		assert.Error(t, err)
		_, err = r.PeekBytesByPath(ctx, "x")
		assert.Error(t, err)
	})
}

func TestCovJsonrpc_SizeAndResultLength(t *testing.T) {
	var nilR *JsonRpcResponse
	sz, err := nilR.Size()
	assert.Zero(t, sz)
	assert.NoError(t, err)

	r := &JsonRpcResponse{}
	sz, err = r.Size()
	assert.Zero(t, sz)
	assert.NoError(t, err)
	assert.Zero(t, r.ResultLength())

	r.SetResultWriter(&covJsonrpcByteWriter{data: []byte(`[1,2,3]`)})
	sz, err = r.Size()
	require.NoError(t, err)
	assert.Equal(t, 7, sz)
	assert.Equal(t, 7, r.ResultLength())

	bad := &JsonRpcResponse{}
	bad.SetResultWriter(&covJsonrpcByteWriter{data: []byte(`x`), sizeErr: errors.New("size unknown")})
	_, err = bad.Size()
	assert.ErrorContains(t, err, "size unknown")
	assert.Zero(t, bad.ResultLength(), "size error reports zero length")

	r.SetResult([]byte(`"0x1"`))
	assert.Equal(t, 5, r.ResultLength())
}

func TestCovJsonrpc_Free(t *testing.T) {
	var nilR *JsonRpcResponse
	nilR.Free()

	w := &covJsonrpcByteWriter{data: []byte(`1`)}
	r := MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"a":"0x1"}`), []byte(`{"code":1,"message":"m"}`))
	_, err := r.CanonicalHash()
	require.NoError(t, err)
	r.SetResultWriter(w)
	r.Free()
	assert.True(t, w.released, "releasable writer is released")
	assert.Nil(t, r.GetResultBytes())
	assert.Zero(t, r.ResultLength())
	assert.Equal(t, int64(1), r.ID(), "typed id survives Free")
	_, ok := r.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	assert.False(t, ok, "hash cache cleared")
}

func TestCovJsonrpc_MarshalZerologObject(t *testing.T) {
	covJsonrpcEnableLogs(t)
	render := func(r *JsonRpcResponse) string {
		var buf bytes.Buffer
		lg := zerolog.New(&buf)
		lg.Info().Object("r", r).Send()
		return buf.String()
	}
	var nilR *JsonRpcResponse
	assert.Contains(t, render(nilR), `"r":{}`)

	r := MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"x":1}`), []byte(`{"code":1,"message":"m"}`))
	out := render(r)
	assert.Contains(t, out, `"error":{"code":1,"message":"m"}`)
	assert.Contains(t, out, `"result":{"x":1}`)

	notJson := &JsonRpcResponse{errBytes: []byte(`<html>`), result: []byte(`123`)}
	out = render(notJson)
	assert.Contains(t, out, `"error":"<html>"`)
	assert.Contains(t, out, `"result":"123"`)

	internalErr := &JsonRpcResponse{Error: NewErrJsonRpcExceptionExternal(-1, "internal", "")}
	assert.Contains(t, render(internalErr), `"message":"internal"`)

	big := bytes.Repeat([]byte("a"), 320*1024)
	bigR := &JsonRpcResponse{result: big}
	out = render(bigR)
	assert.Contains(t, out, `"resultHead"`)
	assert.Contains(t, out, `"resultTail"`)
	assert.NotContains(t, out, `"result":`)

	wr := &JsonRpcResponse{}
	wr.SetResultWriter(&covJsonrpcByteWriter{emptyish: true})
	assert.Contains(t, render(wr), `"resultWriterEmpty":true`)
}

func TestCovJsonrpc_WriteTo(t *testing.T) {
	t.Run("internal error marshalled lazily", func(t *testing.T) {
		r := &JsonRpcResponse{idBytes: []byte(`1`), Error: NewErrJsonRpcExceptionExternal(-32000, "boom", "")}
		assert.Equal(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom","data":""}}`, covJsonrpcWrite(t, r))
	})

	t.Run("streamed result", func(t *testing.T) {
		r := &JsonRpcResponse{idBytes: []byte(`"a"`)}
		r.SetResultWriter(&covJsonrpcByteWriter{data: []byte(`[1]`)})
		assert.Equal(t, `{"jsonrpc":"2.0","id":"a","result":[1]}`, covJsonrpcWrite(t, r))
	})

	t.Run("streamed result failure", func(t *testing.T) {
		r := &JsonRpcResponse{idBytes: []byte(`1`)}
		r.SetResultWriter(&covJsonrpcByteWriter{writeErr: errors.New("stream fail")})
		_, err := r.WriteTo(&bytes.Buffer{})
		assert.ErrorContains(t, err, "stream fail")
	})

	t.Run("unmarshalable internal error", func(t *testing.T) {
		r := &JsonRpcResponse{idBytes: []byte(`1`), Error: &ErrJsonRpcExceptionExternal{Code: 1, Data: make(chan int)}}
		_, err := r.WriteTo(&bytes.Buffer{})
		assert.Error(t, err)
	})

	// Each write step must propagate a writer failure.
	type shape struct {
		name   string
		mk     func() *JsonRpcResponse
		writes int
	}
	shapes := []shape{
		{"errBytes+result", func() *JsonRpcResponse {
			return MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`2`), []byte(`{"code":1,"message":"m"}`))
		}, 6},
		{"lazy error", func() *JsonRpcResponse {
			return &JsonRpcResponse{idBytes: []byte(`1`), Error: NewErrJsonRpcExceptionExternal(1, "m", "")}
		}, 4},
		{"writer result", func() *JsonRpcResponse {
			r := &JsonRpcResponse{idBytes: []byte(`1`)}
			r.SetResultWriter(&covJsonrpcByteWriter{data: []byte(`3`)})
			return r
		}, 4},
	}
	for _, s := range shapes {
		for i := 1; i <= s.writes; i++ {
			w := &covJsonrpcFailWriter{failAt: i}
			_, err := s.mk().WriteTo(w)
			assert.ErrorIs(t, err, errCovJsonrpcWrite, "%s: failing write #%d must propagate", s.name, i)
		}
	}
}

func TestCovJsonrpc_WriteResultTo(t *testing.T) {
	var buf bytes.Buffer
	r := &JsonRpcResponse{result: []byte(`[1,2]`)}
	n, err := r.WriteResultTo(&buf, false)
	require.NoError(t, err)
	assert.Equal(t, int64(5), n)
	assert.Equal(t, `[1,2]`, buf.String())

	buf.Reset()
	_, err = r.WriteResultTo(&buf, true)
	require.NoError(t, err)
	assert.Equal(t, `1,2`, buf.String())

	buf.Reset()
	n, err = (&JsonRpcResponse{result: []byte(`[]`)}).WriteResultTo(&buf, true)
	require.NoError(t, err)
	assert.Zero(t, n)

	n, err = (&JsonRpcResponse{}).WriteResultTo(&buf, false)
	require.NoError(t, err)
	assert.Zero(t, n)

	buf.Reset()
	wr := &JsonRpcResponse{}
	wr.SetResultWriter(&covJsonrpcByteWriter{data: []byte(`[7,8]`)})
	_, err = wr.WriteResultTo(&buf, true)
	require.NoError(t, err)
	assert.Equal(t, `7,8`, buf.String())
}

func TestCovJsonrpc_Clone(t *testing.T) {
	var nilR *JsonRpcResponse
	c, err := nilR.Clone()
	assert.Nil(t, c)
	assert.NoError(t, err)

	orig := MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"a":"0x1"}`), []byte(`{"code":5,"message":"m"}`))
	h, err := orig.CanonicalHash()
	require.NoError(t, err)
	c, err = orig.Clone()
	require.NoError(t, err)
	assert.Equal(t, covJsonrpcWrite(t, orig), covJsonrpcWrite(t, c))
	ch, ok := c.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	assert.True(t, ok)
	assert.Equal(t, h, ch)
	// Deep copy: mutating clone bytes never touches the original.
	c.result[1] = 'Z'
	c.errBytes[0] = '['
	assert.Equal(t, `{"a":"0x1"}`, orig.GetResultString())
	assert.Equal(t, byte('{'), orig.errBytes[0])

	wr := &JsonRpcResponse{idBytes: []byte(`2`)}
	w := &covJsonrpcByteWriter{data: []byte(`"0xff"`)}
	wr.SetResultWriter(w)
	c2, err := wr.Clone()
	require.NoError(t, err)
	assert.Equal(t, `"0xff"`, c2.GetResultString())
	assert.Nil(t, c2.resultWriter)
}

func TestCovJsonrpc_IsResultEmptyish(t *testing.T) {
	ctx := context.Background()
	var nilR *JsonRpcResponse
	assert.True(t, nilR.IsResultEmptyish())
	assert.True(t, nilR.IsResultEmptyish(ctx))
	assert.True(t, (&JsonRpcResponse{}).IsResultEmptyish(ctx))
	assert.False(t, (&JsonRpcResponse{result: []byte(`"0x1"`)}).IsResultEmptyish(ctx))
	assert.True(t, (&JsonRpcResponse{result: []byte(`"0x"`)}).IsResultEmptyish(ctx))

	w := &JsonRpcResponse{}
	w.SetResultWriter(&covJsonrpcByteWriter{emptyish: true})
	assert.True(t, w.IsResultEmptyish(ctx))
	w.SetResultWriter(&covJsonrpcByteWriter{emptyish: false})
	assert.False(t, w.IsResultEmptyish())
}

func TestCovJsonrpc_CanonicalHashEdges(t *testing.T) {
	var nilR *JsonRpcResponse
	h, err := nilR.CanonicalHash()
	assert.Equal(t, "", h)
	assert.NoError(t, err)
	h, err = nilR.CanonicalHashWithIgnoredFields([]string{"a"})
	assert.Equal(t, "", h)
	assert.NoError(t, err)

	bad := &JsonRpcResponse{result: []byte(`{bad`)}
	_, err = bad.CanonicalHash()
	assert.Error(t, err)
	_, err = bad.CanonicalHashWithIgnoredFields([]string{"a"})
	assert.Error(t, err)

	// No ignore fields delegates to CanonicalHash.
	r := &JsonRpcResponse{result: []byte(`{"a":"0x1"}`)}
	h1, err := r.CanonicalHash()
	require.NoError(t, err)
	h2, err := r.CanonicalHashWithIgnoredFields(nil)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// Cached with-ignored result is reused for the same slice.
	ign := []string{"a"}
	a1, err := r.CanonicalHashWithIgnoredFields(ign)
	require.NoError(t, err)
	r.SetResult([]byte(`{"a":"0x2","b":"0x3"}`))
	a2, err := r.CanonicalHashWithIgnoredFields(ign)
	require.NoError(t, err)
	assert.Equal(t, a1, a2, "cache keyed by ignore slice")
}

func TestCovJsonrpc_CanonicalizeEquivalences(t *testing.T) {
	hash := func(s string) string {
		r := &JsonRpcResponse{result: []byte(s)}
		h, err := r.CanonicalHash()
		require.NoError(t, err)
		return h
	}
	// Leading zeroes in hex strings, empties and key order do not matter.
	assert.Equal(t, hash(`{"a":"0x01","b":[1,"",null,{}],"c":"0x0"}`), hash(`{"b":[1],"a":"0x1"}`))
	// Arrays of only emptyish elements collapse like the empty array.
	assert.Equal(t, hash(`{"a":[null,"0x00",[]],"b":1}`), hash(`{"b":1}`))
	// Booleans are never empty.
	assert.NotEqual(t, hash(`{"a":false}`), hash(`{}`))
	// Numbers: zero vanishes, others kept.
	assert.Equal(t, hash(`{"a":0,"b":2}`), hash(`{"b":2}`))
	assert.NotEqual(t, hash(`{"b":2}`), hash(`{"b":3}`))
	// Nested arrays.
	assert.NotEqual(t, hash(`[[1,2],[3]]`), hash(`[[1],[2,3]]`))
}

func TestCovJsonrpc_CanonicalizeTo_Direct(t *testing.T) {
	cases := []struct {
		in    interface{}
		out   string
		wrote bool
	}{
		{nil, "", false},
		{"0x00ab", "ab", true},
		{"", "", false},
		{[]byte("0x0012"), "12", true},
		{[]byte(`"0x00"`), "", false},
		{[]byte{}, "", false},
		{int64(7), "7", true},
		{json.Number("0"), "", false},
		{map[string]interface{}{"k": "v"}, `{"k":v}`, true},
		{map[string]interface{}{"k": []interface{}{}}, "", false},
		{[]interface{}{"x", true}, "[x,true]", true},
		{[]interface{}{map[string]interface{}{"k": ""}, "y"}, "[y]", true},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		wrote, err := canonicalizeTo(&buf, tc.in)
		require.NoError(t, err, "%#v", tc.in)
		assert.Equal(t, tc.wrote, wrote, "%#v", tc.in)
		if tc.wrote {
			assert.Equal(t, tc.out, buf.String(), "%#v", tc.in)
		}
	}

	_, err := canonicalizeTo(&bytes.Buffer{}, make(chan int))
	assert.Error(t, err)
	_, err = canonicalizeTo(&bytes.Buffer{}, map[string]interface{}{"k": make(chan int)})
	assert.Error(t, err)
	_, err = canonicalizeTo(&bytes.Buffer{}, []interface{}{make(chan int)})
	assert.Error(t, err)

	// Writer failures propagate from every structural write.
	inputs := []interface{}{
		map[string]interface{}{"a": "1", "b": "2"},
		[]interface{}{"1", "2"},
		"abc",
		[]byte("abc"),
		int64(5),
	}
	for _, in := range inputs {
		for i := 1; i <= 8; i++ {
			w := &covJsonrpcFailWriter{failAt: i}
			_, err := canonicalizeTo(w, in)
			if w.calls >= i {
				assert.ErrorIs(t, err, errCovJsonrpcWrite, "%#v write #%d", in, i)
			}
		}
	}
}

func TestCovJsonrpc_IsEmptyishValue(t *testing.T) {
	cases := []struct {
		v    interface{}
		want bool
	}{
		{nil, true},
		{"", true},
		{"0x", true},
		{"0x000", true},
		{"0x010", false},
		{"abc", false},
		{[]interface{}{}, true},
		{[]interface{}{1}, false},
		{map[string]interface{}{}, true},
		{float64(0), true},
		{float64(1), false},
		{false, false},
		{int64(0), true},
		{int64(3), false},
		{make(chan int), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isEmptyishValue(tc.v), "%#v", tc.v)
	}
}

func TestCovJsonrpc_RemoveFieldsByPaths_Edges(t *testing.T) {
	assert.Nil(t, removeFieldsByPaths(nil, []string{"a"}))
	obj := map[string]interface{}{"a": 1}
	assert.Equal(t, obj, removeFieldsByPaths(obj, nil))

	// Wildcard on map keys applies nested removal to every value.
	got := removeFieldsByPaths(map[string]interface{}{
		"x": map[string]interface{}{"ts": 1, "keep": 2},
		"y": map[string]interface{}{"ts": 3},
	}, []string{"*.ts"})
	assert.Equal(t, map[string]interface{}{
		"x": map[string]interface{}{"keep": 2},
		"y": map[string]interface{}{},
	}, got)

	// Array without wildcard rule is left untouched; primitives pass through.
	arr := []interface{}{1, 2}
	assert.Equal(t, map[string]interface{}{"a": arr}, removeFieldsByPaths(map[string]interface{}{"a": arr}, []string{"a.b"}))
	assert.Equal(t, "s", removeFieldsByPaths("s", []string{"a"}))
	// Wildcard leaf "*" on an array (not a nested tree) returns the array unchanged.
	assert.Equal(t, arr, removeFieldsByPaths(arr, []string{"*"}))
}

func TestCovJsonrpc_RemoveLeadingZeroes(t *testing.T) {
	assert.Equal(t, "ab", string(removeLeadingZeroes([]byte("0x00ab"))))
	assert.Equal(t, "ab", string(removeLeadingZeroes([]byte("0Xab"))))
	assert.Equal(t, `1"`, string(removeLeadingZeroes([]byte(`"0x01"`))))
	assert.Nil(t, removeLeadingZeroes([]byte(`"0x00"`)))
	assert.Equal(t, "12", string(removeLeadingZeroes([]byte("12"))))
}

func TestCovJsonrpcRequest_Basics(t *testing.T) {
	var nilR *JsonRpcRequest
	assert.Equal(t, "", nilR.NetworkIdHint())
	ch, err := nilR.CacheHash()
	assert.Equal(t, "", ch)
	assert.NoError(t, err)
	nilR.InvalidateCacheHash()
	_, err = nilR.PeekByPath(0)
	assert.ErrorContains(t, err, "empty params")

	r := NewJsonRpcRequest("eth_call", []interface{}{"0x1"})
	assert.Equal(t, "2.0", r.JSONRPC)
	assert.False(t, r.IsNotification())
	ctx := context.Background()
	r.LockWithTrace(ctx)
	r.Unlock()
	r.RLockWithTrace(ctx)
	r.RUnlock()
	assert.Nil(t, r.IDRawBytes(), "programmatic request has no wire id")
}

func TestCovJsonrpcRequest_UnmarshalIdForms(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantID       interface{}
		randomID     bool
		raw          string
		notification bool
		errSub       string
	}{
		{"int", `{"method":"m","id":5}`, int64(5), false, "5", false, ""},
		{"string", `{"method":"m","id":"abc"}`, "abc", false, `"abc"`, false, ""},
		{"empty string", `{"method":"m","id":""}`, nil, true, `""`, false, ""},
		{"null", `{"method":"m","id":null}`, nil, true, "null", false, ""},
		{"missing", `{"method":"m"}`, nil, true, "", true, ""},
		{"fractional", `{"method":"m","id":1.5}`, nil, false, "", false, "cannot be represented as a 64-bit integer"},
		{"too large", `{"method":"m","id":99999999999999999999}`, nil, false, "", false, "cannot be represented"},
		{"no method", `{"id":1}`, nil, false, "", false, `no "method" member`},
		{"bad json", `{`, nil, false, "", false, "?"},
		{"bool id ignored", `{"method":"m","id":true}`, nil, true, "true", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &JsonRpcRequest{}
			err := SonicCfg.Unmarshal([]byte(tc.body), r)
			if tc.errSub != "" {
				require.Error(t, err)
				if tc.errSub != "?" {
					assert.Contains(t, err.Error(), tc.errSub)
				}
				return
			}
			require.NoError(t, err)
			if tc.randomID {
				_, ok := r.ID.(int64)
				assert.True(t, ok, "random id assigned, got %T", r.ID)
			} else {
				assert.Equal(t, tc.wantID, r.ID)
			}
			if tc.raw == "" {
				assert.Nil(t, r.IDRawBytes())
			} else {
				assert.Equal(t, tc.raw, string(r.IDRawBytes()))
			}
			assert.Equal(t, tc.notification, r.IsNotification())
		})
	}
}

func TestCovJsonrpcRequest_EthQueryNonArrayParams(t *testing.T) {
	r := &JsonRpcRequest{}
	err := SonicCfg.Unmarshal([]byte(`{"jsonrpc":"2.0","method":"eth_queryLogs","params":{"a":1},"id":3,"networkId":"evm:1"}`), r)
	require.NoError(t, err, "eth_query* with object params decodes so the query parser can answer -32602")
	assert.Equal(t, "eth_queryLogs", r.Method)
	assert.Nil(t, r.Params)
	assert.Equal(t, int64(3), r.ID)
	assert.Equal(t, "evm:1", r.NetworkIdHint())

	other := &JsonRpcRequest{}
	assert.Error(t, SonicCfg.Unmarshal([]byte(`{"method":"eth_call","params":{"a":1},"id":3}`), other),
		"non eth_query methods keep failing on object params")
}

func TestCovJsonrpcRequest_ResponseID(t *testing.T) {
	parse := func(body string) *JsonRpcRequest {
		r := &JsonRpcRequest{}
		require.NoError(t, SonicCfg.Unmarshal([]byte(body), r))
		return r
	}
	big := parse(`{"method":"m","id":9007199254740993}`)
	assert.Equal(t, json.RawMessage(`9007199254740993`), big.ResponseID(false))

	null := parse(`{"method":"m","id":null}`)
	assert.Equal(t, json.RawMessage(`null`), null.ResponseID(true), "solana echoes null")
	assert.IsType(t, int64(0), null.ResponseID(false), "evm gets the random id")

	prog := NewJsonRpcRequest("m", nil)
	prog.ID = "p"
	assert.Equal(t, "p", prog.ResponseID(false))

	// SetID drops the captured wire bytes.
	require.NoError(t, big.SetID(int64(1)))
	assert.Nil(t, big.IDRawBytes())
	assert.Equal(t, int64(1), big.ResponseID(false))
}

func TestCovJsonrpcRequest_CloneKeepsIdRaw(t *testing.T) {
	r := &JsonRpcRequest{}
	require.NoError(t, SonicCfg.Unmarshal([]byte(`{"method":"m","id":12345678901234567,"params":[null,{"a":[1]}]}`), r))
	c := r.Clone()
	assert.Equal(t, r.IDRawBytes(), c.IDRawBytes())
	c.idRaw[0] = '9'
	assert.Equal(t, byte('1'), r.idRaw[0], "idRaw deep-copied")
	assert.Nil(t, c.Params[0])

	empty := NewJsonRpcRequest("m", nil).Clone()
	assert.Nil(t, empty.Params)
	assert.Nil(t, empty.IDRawBytes())
}

func TestCovJsonrpcRequest_MarshalZerologObject(t *testing.T) {
	covJsonrpcEnableLogs(t)
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	var nilR *JsonRpcRequest
	lg.Info().Object("r", nilR).Send()
	assert.Contains(t, buf.String(), `"r":{}`)

	buf.Reset()
	r := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabc", "latest"})
	r.ID = int64(9)
	lg.Info().Object("r", r).Send()
	assert.Contains(t, buf.String(), `"r":{"method":"eth_getBalance","params":["0xabc","latest"],"id":9}`)
}

func TestCovJsonrpcRequest_CacheHashTypes(t *testing.T) {
	r := NewJsonRpcRequest("m", []interface{}{1, true, false, nil, map[string]interface{}{"k": []interface{}{"A"}}})
	h1, err := r.CacheHash(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h1, "m:"))

	// int and float64 of same value encode identically.
	r2 := NewJsonRpcRequest("m", []interface{}{float64(1), true, false, nil, map[string]interface{}{"k": []interface{}{"a"}}})
	h2, err := r2.CacheHash()
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// Memoized until invalidated.
	r.Params[1] = false
	h3, _ := r.CacheHash()
	assert.Equal(t, h1, h3)
	r.InvalidateCacheHash()
	h4, err := r.CacheHash()
	require.NoError(t, err)
	assert.NotEqual(t, h1, h4)

	for _, bad := range []interface{}{struct{}{}, []interface{}{struct{}{}}, map[string]interface{}{"k": struct{}{}}} {
		_, err := NewJsonRpcRequest("m", []interface{}{bad}).CacheHash()
		assert.ErrorContains(t, err, "unsupported type")
	}

	// writeUint zero and multi-digit counts.
	var buf bytes.Buffer
	writeUint(&buf, 0)
	writeUint(&buf, 1234)
	assert.Equal(t, "01234", buf.String())
}

func TestCovJsonrpcRequest_PeekByPath(t *testing.T) {
	r := NewJsonRpcRequest("m", []interface{}{map[string]interface{}{"to": "0x1", "arr": []interface{}{"z"}}})
	v, err := r.PeekByPath(0, "to")
	require.NoError(t, err)
	assert.Equal(t, "0x1", v)
	v, err = r.PeekByPath(0, "arr", 0)
	require.NoError(t, err)
	assert.Equal(t, "z", v)

	_, err = r.PeekByPath(5)
	assert.ErrorContains(t, err, "out of bounds")
	_, err = r.PeekByPath(0, 0)
	assert.ErrorContains(t, err, "expected array")
	_, err = r.PeekByPath("x")
	assert.ErrorContains(t, err, "expected map")
	_, err = r.PeekByPath(0, "missing")
	assert.ErrorContains(t, err, "not found")
	_, err = r.PeekByPath(1.5)
	assert.ErrorContains(t, err, "unsupported path element type")
	_, err = NewJsonRpcRequest("m", nil).PeekByPath(0)
	assert.ErrorContains(t, err, "empty params")
}

func TestCovJsonrpc_TranslateToJsonRpcException(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code JsonRpcErrorNumber
		msg  string
	}{
		{"auth rate limit", NewErrAuthRateLimitRuleExceeded("p", "s", "b", "r", "u", "ip"), JsonRpcErrorCapacityExceeded, "rate-limit exceeded"},
		{"project rate limit", NewErrProjectRateLimitRuleExceeded("p", "b", "r"), JsonRpcErrorCapacityExceeded, "rate-limit exceeded"},
		{"network rate limit", NewErrNetworkRateLimitRuleExceeded("p", "n", "b", "r"), JsonRpcErrorCapacityExceeded, "rate-limit exceeded"},
		{"upstream rate limit", NewErrUpstreamRateLimitRuleExceeded("u", "b", "r"), JsonRpcErrorCapacityExceeded, "rate-limit exceeded"},
		{"unauthorized", NewErrAuthUnauthorized("secret", "bad token", "nope"), JsonRpcErrorUnauthorized, "unauthorized"},
		{"method ignored", NewErrUpstreamMethodIgnored("eth_x", "u1"), JsonRpcErrorUnsupportedException, "method ignored by upstream"},
		{"subscription not found", NewErrSubscriptionNotFound("0x1"), JsonRpcErrorCallException, "subscription not found"},
		{"request unmarshal", NewErrJsonRpcRequestUnmarshal(errors.New("bad"), []byte("{")), JsonRpcErrorParseException, "failed to parse json-rpc request"},
		{"invalid request", NewErrInvalidRequest(errors.New("bad")), JsonRpcErrorInvalidArgument, "invalid request url and/or body"},
		{"invalid url path", NewErrInvalidUrlPath("bad", "/x"), JsonRpcErrorInvalidArgument, "invalid request url and/or body"},
		{"getLogs range", NewErrGetLogsExceededMaxAllowedRange(100, 10), JsonRpcErrorEvmLargeRange, "getLogs request exceeded max allowed range"},
		{"block unavailable", NewErrUpstreamBlockUnavailable("u", 10, 5, 3), JsonRpcErrorMissingData, "block not available"},
		{"plain error", errors.New("kaboom"), JsonRpcErrorServerSideException, "kaboom"},
		{"standard error", NewErrEndpointServerSideException(errors.New("deep cause"), nil, 500), JsonRpcErrorServerSideException, "deep cause"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := TranslateToJsonRpcException(tc.err)
			jre := &ErrJsonRpcExceptionInternal{}
			require.True(t, errors.As(out, &jre))
			assert.Equal(t, tc.code, jre.NormalizedCode())
			assert.Contains(t, jre.Message, tc.msg)
			assert.True(t, errors.Is(out, tc.err) || out == tc.err || jre.Cause == tc.err)
		})
	}

	t.Run("existing json-rpc exception passes through", func(t *testing.T) {
		in := NewErrJsonRpcExceptionInternal(0, JsonRpcErrorEvmReverted, "reverted", nil, nil)
		assert.Same(t, in, TranslateToJsonRpcException(in))
	})
}

func TestCovJsonrpc_TranslateExhaustedDominance(t *testing.T) {
	// Two rate-limited upstreams outvote one server error.
	ex := newExhausted(t, []string{"a", "b", "c"}, map[string]error{
		"a": NewErrUpstreamRateLimitRuleExceeded("a", "b", "r"),
		"b": NewErrUpstreamRateLimitRuleExceeded("b", "b", "r"),
		"c": NewErrEndpointServerSideException(errors.New("x"), nil, 500),
	})
	assert.EqualValues(t, JsonRpcErrorCapacityExceeded, clientWireCode(t, TranslateToJsonRpcException(ex)))

	// Only skipped/unsupported causes: the first one is the representative.
	ex2 := newExhausted(t, []string{"a", "b"}, map[string]error{
		"a": NewErrUpstreamRequestSkipped(errors.New("skip"), "a"),
		"b": NewErrEndpointUnsupported(errors.New("unsupported")),
	})
	out := TranslateToJsonRpcException(ex2)
	assert.EqualValues(t, JsonRpcErrorServerSideException, clientWireCode(t, out))

	// Block-unavailable is a verdict only when every non-skipped cause agrees.
	ex3 := newExhausted(t, []string{"a", "b"}, map[string]error{
		"a": NewErrUpstreamBlockUnavailable("a", 10, 5, 3),
		"b": NewErrUpstreamRequestSkipped(errors.New("skip"), "b"),
	})
	assert.EqualValues(t, JsonRpcErrorMissingData, clientWireCode(t, TranslateToJsonRpcException(ex3)))

	// Plain (non-standard) causes are ignored by the scan.
	ex4 := newExhausted(t, []string{"a"}, map[string]error{"a": errors.New("plain")})
	assert.EqualValues(t, JsonRpcErrorServerSideException, clientWireCode(t, TranslateToJsonRpcException(ex4)))
}

func TestCovJsonrpc_HasLinearErrorCode(t *testing.T) {
	assert.False(t, hasLinearErrorCode(errors.New("plain"), ErrCodeNotImplemented))
	assert.True(t, hasLinearErrorCode(NewErrFailsafeRetryExceeded(ScopeNetwork, NewErrNotImplemented("x"), nil), ErrCodeNotImplemented))
	assert.False(t, hasLinearErrorCode(nil, ErrCodeNotImplemented))
	assert.Nil(t, findUpstreamsExhausted(errors.New("plain")))
}
