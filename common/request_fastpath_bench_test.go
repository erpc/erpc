package common

import "testing"

var forwardBodySink []byte

func legacyForwardBody(req *NormalizedRequest) ([]byte, error) {
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	if jrq == nil {
		return nil, NewErrInvalidRequest(nil)
	}

	jrq.RLock()
	body, err := SonicCfg.Marshal(JsonRpcRequest{
		JSONRPC: jrq.JSONRPC,
		Method:  jrq.Method,
		Params:  jrq.Params,
		ID:      jrq.ID,
	})
	jrq.RUnlock()
	if err != nil {
		return nil, err
	}

	return body, nil
}

func benchmarkRequestUnmodified(b *testing.B) *NormalizedRequest {
	b.Helper()

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","fromBlock":"0x15536ea","toBlock":"0x15536ff","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}`)
	req := NewNormalizedRequest(raw)
	_, err := req.JsonRpcRequest()
	if err != nil {
		b.Fatalf("failed to parse request: %v", err)
	}

	return req
}

func benchmarkRequestModified(b *testing.B) *NormalizedRequest {
	b.Helper()

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x70a082310000000000000000000000000000000000000000000000000000000000000001"}]}`)
	req := NewNormalizedRequest(raw)
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		b.Fatalf("failed to parse request: %v", err)
	}
	if err := jrq.AppendParam("latest"); err != nil {
		b.Fatalf("failed to mutate request: %v", err)
	}

	return req
}

func BenchmarkForwardBodyUnmodifiedFastPath(b *testing.B) {
	req := benchmarkRequestUnmodified(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		body, err := req.ForwardBody()
		if err != nil {
			b.Fatal(err)
		}
		forwardBodySink = body
	}
}

func BenchmarkForwardBodyUnmodifiedLegacyMarshal(b *testing.B) {
	req := benchmarkRequestUnmodified(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		body, err := legacyForwardBody(req)
		if err != nil {
			b.Fatal(err)
		}
		forwardBodySink = body
	}
}

func BenchmarkForwardBodyModifiedFastPath(b *testing.B) {
	req := benchmarkRequestModified(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		body, err := req.ForwardBody()
		if err != nil {
			b.Fatal(err)
		}
		forwardBodySink = body
	}
}

func BenchmarkForwardBodyModifiedLegacyMarshal(b *testing.B) {
	req := benchmarkRequestModified(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		body, err := legacyForwardBody(req)
		if err != nil {
			b.Fatal(err)
		}
		forwardBodySink = body
	}
}
