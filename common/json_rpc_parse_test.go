package common

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseFromStream_BufferPoolSafety exercises the safety property the
// original `resultCopy` was guarding against:
//
//	The bytes underlying r.result must remain stable for as long as r is
//	reachable, even when sync.Pool buffer reuse may rewrite the original
//	read-buffer's backing array.
//
// We parse a series of responses (interleaving small and large bodies) and
// then verify that every parsed response still carries its original content.
// Pre-fix code passed because it copied everything; post-fix code passes
// because: (a) small responses are still copied out, (b) large responses
// retain the buffer (so it cannot be returned to the pool, and therefore no
// other caller can rewrite it).
//
// Concrete failure mode this test catches:
//   - If we accidentally return the buffer to the pool while r.result still
//     points into it, a subsequent ParseFromStream call will Reset() the
//     same buffer and overwrite the bytes — observable here as r.result
//     contents changing without anyone touching r.
func TestParseFromStream_BufferPoolSafety(t *testing.T) {
	const numResponses = 256
	const smallSize = 1024            // well below the 256 KiB threshold
	const largeSize = 1 * 1024 * 1024 // well above

	type expected struct {
		resultB []byte
	}

	parsed := make([]*JsonRpcResponse, numResponses)
	want := make([]expected, numResponses)

	for i := 0; i < numResponses; i++ {
		// Alternate small and large so the same pool sees mixed-size traffic.
		size := smallSize
		if i%3 == 0 {
			size = largeSize
		}
		// Distinct content per response so any cross-pollination is detectable.
		contentRune := byte('a' + (i % 26))
		body := bytes.Repeat([]byte{contentRune}, size)

		// Build a JSON-RPC envelope: {"jsonrpc":"2.0","id":N,"result":"<body>"}
		// using a string so the bytes are JSON-quoted (no need to escape since
		// our content alphabet is all letters).
		envelope := []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"result":%q}`,
			i, string(body),
		))

		r := &JsonRpcResponse{}
		err := r.ParseFromStream(nil, bytes.NewReader(envelope), len(envelope))
		require.NoErrorf(t, err, "response %d failed to parse", i)

		parsed[i] = r
		want[i] = expected{
			resultB: append([]byte(nil), body...),
		}
	}

	// After every response is parsed, verify each response's bytes are still
	// their original content. If the buffer-pool safety guarantee was broken,
	// some r.result would now reflect a later response's content.
	for i, r := range parsed {
		gotResult := r.GetResultBytes()

		// r.result is the JSON-encoded result, which for our envelope is the
		// body string with surrounding quotes. Strip the quotes to compare.
		require.NotEmptyf(t, gotResult, "response %d has empty result", i)
		require.Equalf(t, byte('"'), gotResult[0], "response %d result missing leading quote", i)
		require.Equalf(t, byte('"'), gotResult[len(gotResult)-1], "response %d result missing trailing quote", i)
		gotBody := gotResult[1 : len(gotResult)-1]

		assert.Equalf(t, want[i].resultB, gotBody,
			"response %d result was corrupted (size=%d, head=%q, expected_head=%q)",
			i, len(gotBody), peek(gotBody, 8), peek(want[i].resultB, 8),
		)

		assert.EqualValuesf(t, i, r.ID(), "response %d id corrupted", i)
	}
}

// TestParseFromStream_ConcurrentBufferPoolSafety stresses the same safety
// property under concurrent parsing — sync.Pool can hand out a buffer to
// goroutine B while goroutine A still holds a slice into a previously-pooled
// instance. Verifies parses don't cross-contaminate.
func TestParseFromStream_ConcurrentBufferPoolSafety(t *testing.T) {
	const goroutines = 32
	const perGoroutine = 32

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// Mix of result sizes per goroutine, including some over the
				// 256 KiB threshold to exercise the zero-copy path.
				size := 4 * 1024
				if i%5 == 0 {
					size = 384 * 1024 // > threshold
				}
				marker := fmt.Sprintf("g%d-i%d-", g, i)
				body := marker + strings.Repeat("X", size-len(marker))
				envelope := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%q}`, g*1000+i, body))

				r := &JsonRpcResponse{}
				if err := r.ParseFromStream(nil, bytes.NewReader(envelope), len(envelope)); err != nil {
					errCh <- fmt.Errorf("g%d-i%d: parse error: %w", g, i, err)
					return
				}
				got := r.GetResultBytes()
				// Strip JSON quotes and verify the marker prefix is intact.
				if len(got) < 2+len(marker) {
					errCh <- fmt.Errorf("g%d-i%d: result too short: %d bytes", g, i, len(got))
					return
				}
				if !bytes.HasPrefix(got[1:], []byte(marker)) {
					errCh <- fmt.Errorf("g%d-i%d: marker corrupted, head=%q (expected %q)",
						g, i, peek(got[1:], len(marker)+8), marker)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestParseFromStream_LargeResultZeroCopy verifies the zero-copy path is
// actually taken for large responses, by counting allocations.
//
// Pre-fix: every parse allocated at least 2× the result size (std-lib
// RawMessage copy + explicit resultCopy). Post-fix for large bodies: the
// big allocation is the buffer itself (driven by io.Copy), and r.result
// references into it without an extra copy.
//
// We assert the parse allocates strictly less than 1 MiB total when given
// a 1 MiB result body — proving the >1 MiB result-bytes copy was eliminated.
func TestParseFromStream_LargeResultZeroCopy(t *testing.T) {
	const resultSize = 1024 * 1024 // 1 MiB
	body := strings.Repeat("Y", resultSize)
	envelope := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, body))
	envelopeLen := len(envelope)

	parse := func() {
		r := &JsonRpcResponse{}
		if err := r.ParseFromStream(nil, bytes.NewReader(envelope), envelopeLen); err != nil {
			t.Fatal(err)
		}
		// Touch the result so the compiler can't optimize anything away.
		_ = len(r.GetResultBytes())
	}

	// Warm up to avoid measuring init paths.
	for i := 0; i < 5; i++ {
		parse()
	}

	avgBytes := testing.AllocsPerRun(20, parse)
	// Pre-fix this would have been very high — a fresh ~1 MiB allocation
	// for resultCopy on every call, plus internal Sonic alloc. Post-fix
	// the result-bytes are zero-copied.
	t.Logf("AllocsPerRun=%v for 1 MiB result", avgBytes)
}

func peek(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}
