package integrity

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// splitDupReference is the original pairwise duplicate check splitTxObject
// must agree with: two keys collide when equal under ASCII case folding.
func splitDupReference(raw string) (isObj, dup bool) {
	var seen []string
	isObj = forEachPair(raw, 0, func(k, _ string) {
		for _, p := range seen {
			if asciiFoldEq(p, k) {
				dup = true
			}
		}
		seen = append(seen, k)
	})
	return isObj, dup
}

func objectWithKeys(keys []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:\"0x%x\"", k, i)
	}
	b.WriteByte('}')
	return b.String()
}

func TestSplitTxObject_DuplicateKeysMatchPairwiseReference(t *testing.T) {
	alphabet := []byte("aAbBzZ_0é")
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 3000; iter++ {
		n := rng.Intn(80)
		keys := make([]string, n)
		for i := range keys {
			l := 1 + rng.Intn(3)
			var kb []byte
			for j := 0; j < l; j++ {
				kb = append(kb, alphabet[rng.Intn(len(alphabet))])
			}
			keys[i] = string(kb)
		}
		raw := objectWithKeys(keys)
		isObj, dup := splitDupReference(raw)
		require.Equal(t, isObj && !dup, splitTxObject(raw).ok, raw)
	}

	for _, tc := range []struct {
		keys []string
		ok   bool
	}{
		{[]string{"hash", "Hash"}, false},
		{[]string{"hash", "HASH"}, false},
		{[]string{"nonce", "gas", "nonce"}, false},
		// Non-ASCII bytes are not folded: 'é' and 'É' are distinct keys.
		{[]string{"é", "É"}, true},
		{[]string{"k", "K\u212a"}, true},
	} {
		raw := objectWithKeys(tc.keys)
		require.Equal(t, tc.ok, splitTxObject(raw).ok, raw)
	}

	// Past the linear threshold, collisions with keys added before and after
	// the switch to the map are still found, case-insensitively.
	for _, at := range []int{0, foldedKeyLinearMax - 1, foldedKeyLinearMax, foldedKeyLinearMax + 5} {
		keys := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			keys = append(keys, fmt.Sprintf("key%d", i))
		}
		require.True(t, splitTxObject(objectWithKeys(keys)).ok)
		keys = append(keys, strings.ToUpper(keys[at]))
		require.False(t, splitTxObject(objectWithKeys(keys)).ok, "dup of key %d", at)
	}
}

func manyKeyTxObject(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%06d", i)
	}
	return objectWithKeys(keys)
}

// A hostile transaction object with thousands of keys is split in linear
// time (the pairwise scan took ~100ms for 8000 keys).
func TestSplitTxObject_ManyKeysIsLinear(t *testing.T) {
	raw := manyKeyTxObject(20000)
	start := time.Now()
	st := splitTxObject(raw)
	require.True(t, st.ok)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func BenchmarkSplitTxObjectManyKeys(b *testing.B) {
	for _, n := range []int{20, 2000, 8000} {
		raw := manyKeyTxObject(n)
		b.Run(fmt.Sprintf("keys=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				splitTxObject(raw)
			}
		})
	}
}
