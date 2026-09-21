package common

import (
	"strings"
	"sync"
	"testing"
)

// marshalUpstreamBody rebuilds the body exactly the way the HTTP and gRPC
// clients do before sending it to an upstream, so these tests compare against
// the bytes that actually leave the process rather than a stand-in.
func marshalUpstreamBody(t *testing.T, jrq *JsonRpcRequest) []byte {
	t.Helper()
	out, err := SonicCfg.Marshal(JsonRpcRequest{
		JSONRPC: jrq.JSONRPC,
		Method:  jrq.Method,
		Params:  jrq.Params,
		ID:      jrq.ID,
	})
	if err != nil {
		t.Fatalf("marshal upstream body: %v", err)
	}
	return out
}

// TestEnvelope_MethodMatchesForwardedMethod pins the contract the whole request
// path depends on: the method that gates authentication, rate limits, and
// method allow/ignore lists is the same method that reaches the upstream.
// Method() and JsonRpcRequest() read one parse, so they cannot drift apart.
func TestEnvelope_MethodMatchesForwardedMethod(t *testing.T) {
	bodies := map[string]struct {
		body   string
		method string
	}{
		"plain":              {`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`, "eth_call"},
		"method last":        {`{"jsonrpc":"2.0","id":1,"params":[],"method":"eth_getBalance"}`, "eth_getBalance"},
		"escaped spelling":   {`{"jsonrpc":"2.0","\u006dethod":"eth_chainId","id":1}`, "eth_chainId"},
		"nested in params":   {`{"jsonrpc":"2.0","method":"eth_call","params":[{"method":"eth_sendRawTransaction"}],"id":1}`, "eth_call"},
		"with networkId":     {`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1,"networkId":"evm:42161"}`, "eth_blockNumber"},
		"string id":          {`{"jsonrpc":"2.0","method":"eth_syncing","params":[],"id":"abc"}`, "eth_syncing"},
		"unknown extra keys": {`{"jsonrpc":"2.0","method":"debug_traceCall","params":[],"id":1,"extra":{"method":"nope"}}`, "debug_traceCall"},
	}

	for name, tc := range bodies {
		t.Run(name, func(t *testing.T) {
			nq := NewNormalizedRequest([]byte(tc.body))

			if err := nq.Validate(); err != nil {
				t.Fatalf("Validate(): %v", err)
			}

			// The value auth, rate limiting and allow/ignore lists see.
			gated, err := nq.Method()
			if err != nil {
				t.Fatalf("Method(): %v", err)
			}
			if gated != tc.method {
				t.Fatalf("Method() = %q, want %q", gated, tc.method)
			}

			jrq, err := nq.JsonRpcRequest()
			if err != nil {
				t.Fatalf("JsonRpcRequest(): %v", err)
			}
			if jrq.Method != gated {
				t.Fatalf("forwarded method %q disagrees with gated method %q", jrq.Method, gated)
			}

			// And the value that actually goes out on the wire.
			sent := string(marshalUpstreamBody(t, jrq))
			if !strings.Contains(sent, `"method":"`+tc.method+`"`) {
				t.Fatalf("upstream body %s does not carry method %q", sent, tc.method)
			}
		})
	}
}

// TestEnvelope_RejectsRepeatedMethodMember covers the spellings that different
// JSON decoders resolve differently. erpc reports the object as malformed
// instead of picking one, so no component downstream has to agree on a rule.
func TestEnvelope_RejectsRepeatedMethodMember(t *testing.T) {
	bodies := map[string]string{
		"repeated key":       `{"jsonrpc":"2.0","method":"eth_chainId","method":"eth_sendRawTransaction","id":1}`,
		"escaped repeat":     `{"jsonrpc":"2.0","\u006dethod":"eth_chainId","method":"eth_sendRawTransaction","id":1}`,
		"case variant":       `{"jsonrpc":"2.0","method":"eth_chainId","Method":"eth_sendRawTransaction","id":1}`,
		"upper case variant": `{"jsonrpc":"2.0","method":"eth_chainId","METHOD":"eth_sendRawTransaction","id":1}`,
		"three of them":      `{"jsonrpc":"2.0","method":"eth_chainId","method":"eth_call","Method":"eth_sendRawTransaction","id":1}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			nq := NewNormalizedRequest([]byte(body))

			if err := nq.Validate(); err == nil {
				t.Fatal("Validate() accepted an object stating method more than once")
			} else if !strings.Contains(err.Error(), "method") {
				t.Fatalf("Validate() error should name the member, got: %v", err)
			}

			if m, err := nq.Method(); err == nil {
				t.Fatalf("Method() resolved %q instead of reporting the object as malformed", m)
			}

			if jrq, err := nq.JsonRpcRequest(); err == nil {
				t.Fatalf("JsonRpcRequest() resolved %q instead of reporting the object as malformed", jrq.Method)
			}
		})
	}
}

// TestEnvelope_NetworkIdHint covers body-based routing reading the hint off the
// same parse, and confirms the hint stays server-side.
func TestEnvelope_NetworkIdHint(t *testing.T) {
	t.Run("captured from the body", func(t *testing.T) {
		nq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1,"networkId":"evm:42161"}`))
		if got := nq.NetworkIdHint(); got != "evm:42161" {
			t.Fatalf("NetworkIdHint() = %q, want evm:42161", got)
		}
	})

	t.Run("empty when absent", func(t *testing.T) {
		nq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		if got := nq.NetworkIdHint(); got != "" {
			t.Fatalf("NetworkIdHint() = %q, want empty", got)
		}
	})

	t.Run("survives an unparseable body", func(t *testing.T) {
		nq := NewNormalizedRequest([]byte(`{"jsonrpc":`))
		if got := nq.NetworkIdHint(); got != "" {
			t.Fatalf("NetworkIdHint() = %q, want empty", got)
		}
	})

	t.Run("not forwarded upstream", func(t *testing.T) {
		nq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1,"networkId":"evm:42161"}`))
		jrq, err := nq.JsonRpcRequest()
		if err != nil {
			t.Fatalf("JsonRpcRequest(): %v", err)
		}
		if sent := string(marshalUpstreamBody(t, jrq)); strings.Contains(sent, "networkId") {
			t.Fatalf("routing hint leaked into the upstream body: %s", sent)
		}
	})
}

// TestEnvelope_BodyLifetime pins when the raw body is released. Routing reads
// nq.Body() after method resolution, so only the explicit forward-time call may
// drop it.
func TestEnvelope_BodyLifetime(t *testing.T) {
	nq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1,"networkId":"evm:42161"}`))

	if err := nq.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if _, err := nq.Method(); err != nil {
		t.Fatalf("Method(): %v", err)
	}
	if nq.NetworkIdHint() != "evm:42161" {
		t.Fatal("routing hint lost before the body was released")
	}
	if len(nq.Body()) == 0 {
		t.Fatal("Validate()/Method() released the body that routing still reads")
	}

	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		t.Fatalf("JsonRpcRequest(): %v", err)
	}
	if nq.Body() != nil {
		t.Fatal("JsonRpcRequest() should release the body")
	}

	// Everything still answers from the cached envelope.
	if m, err := nq.Method(); err != nil || m != "eth_chainId" {
		t.Fatalf("Method() after release = (%q, %v)", m, err)
	}
	if nq.NetworkIdHint() != "evm:42161" {
		t.Fatal("NetworkIdHint() after release lost the hint")
	}
	if again, err := nq.JsonRpcRequest(); err != nil || again != jrq {
		t.Fatalf("JsonRpcRequest() must keep returning the same envelope, got (%p, %v)", again, err)
	}
}

// TestEnvelope_ConcurrentResolveSharesOneEnvelope covers hedge and consensus
// fan-out: every goroutine must observe the same *JsonRpcRequest, because
// callers mutate it (params rewrites, cache-hash memoization). Run with -race.
func TestEnvelope_ConcurrentResolveSharesOneEnvelope(t *testing.T) {
	nq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))

	const goroutines = 32
	var wg sync.WaitGroup
	seen := make([]*JsonRpcRequest, goroutines)
	methods := make([]string, goroutines)
	errs := make([]error, goroutines)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			jrq, err := nq.JsonRpcRequest()
			if err != nil {
				errs[idx] = err
				return
			}
			seen[idx] = jrq
			methods[idx], errs[idx] = nq.Method()
		}(i)
	}
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if seen[i] != seen[0] {
			t.Fatalf("goroutine %d observed a different envelope (%p vs %p)", i, seen[i], seen[0])
		}
		if methods[i] != "eth_call" {
			t.Fatalf("goroutine %d resolved method %q", i, methods[i])
		}
	}
}
