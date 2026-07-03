package clients

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// TestTransportPool_SameKeyReturnsSameTransport is the core invariant: repeated
// GetOrCreate for the same key returns the SAME *http.Transport (one connection
// pool), and the pool holds exactly one entry.
func TestTransportPool_SameKeyReturnsSameTransport(t *testing.T) {
	p := NewTransportPool()

	t1 := p.GetOrCreate("key-a")
	t2 := p.GetOrCreate("key-a")

	require.NotNil(t, t1)
	require.Same(t, t1, t2, "same key must return the identical transport pointer")
	require.Equal(t, 1, p.size(), "same key must not create a second transport")
}

// TestTransportPool_DifferentKeysReturnDistinctTransports verifies distinct keys
// get distinct transports, so per-upstream isolation WITHIN a project is
// preserved (two different upstreams never share a pool).
func TestTransportPool_DifferentKeysReturnDistinctTransports(t *testing.T) {
	p := NewTransportPool()

	ta := p.GetOrCreate("key-a")
	tb := p.GetOrCreate("key-b")

	require.NotNil(t, ta)
	require.NotNil(t, tb)
	require.NotSame(t, ta, tb, "different keys must return different transports")
	require.Equal(t, 2, p.size())

	// Re-fetching each key is still stable.
	require.Same(t, ta, p.GetOrCreate("key-a"))
	require.Same(t, tb, p.GetOrCreate("key-b"))
	require.Equal(t, 2, p.size())
}

// TestTransportPool_EmptyKey ensures the pool is robust to an empty key (treated
// as any other distinct key) rather than panicking.
func TestTransportPool_EmptyKey(t *testing.T) {
	p := NewTransportPool()
	t1 := p.GetOrCreate("")
	t2 := p.GetOrCreate("")
	require.NotNil(t, t1)
	require.Same(t, t1, t2)
	require.Equal(t, 1, p.size())
}

// TestTransportPool_ConcurrentSameKey hammers a single key from many goroutines.
// Run with -race, this proves the pool is safe for concurrent use and never
// creates a duplicate transport under a lost-update race.
func TestTransportPool_ConcurrentSameKey(t *testing.T) {
	p := NewTransportPool()

	const goroutines = 200
	var wg sync.WaitGroup
	results := make([]*http.Transport, goroutines)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = p.GetOrCreate("hot-key")
		}(i)
	}
	wg.Wait()

	require.Equal(t, 1, p.size(), "concurrent GetOrCreate of one key must yield exactly one transport")
	first := results[0]
	require.NotNil(t, first)
	for i, got := range results {
		require.Samef(t, first, got, "goroutine %d observed a different transport pointer", i)
	}
}

// TestTransportPool_ConcurrentMixedKeys exercises many keys concurrently and
// asserts each key resolves to exactly one stable transport.
func TestTransportPool_ConcurrentMixedKeys(t *testing.T) {
	p := NewTransportPool()

	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7"}
	const perKey = 50

	var mu sync.Mutex
	seen := make(map[string]*http.Transport)

	var wg sync.WaitGroup
	for _, k := range keys {
		for i := 0; i < perKey; i++ {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				tr := p.GetOrCreate(key)
				mu.Lock()
				if prev, ok := seen[key]; ok {
					require.Same(t, prev, tr)
				} else {
					seen[key] = tr
				}
				mu.Unlock()
			}(k)
		}
	}
	wg.Wait()

	require.Equal(t, len(keys), p.size(), "each distinct key must create exactly one transport")
	// Every key's transport must be distinct from the others.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			require.NotSame(t, seen[keys[i]], seen[keys[j]], "keys %q and %q must not share a transport", keys[i], keys[j])
		}
	}
}

// TestNewDefaultTransport_Config pins the transport tuning so an accidental edit
// (e.g. dropping the idle-conn caps that keep connection churn down under
// high-RPS edge load) is caught by CI rather than in production.
func TestNewDefaultTransport_Config(t *testing.T) {
	tr := newDefaultTransport()

	require.NotNil(t, tr.DialContext, "DialContext must be set (kernel TCP keepalive)")
	require.Equal(t, 1024, tr.MaxIdleConns)
	require.Equal(t, 256, tr.MaxIdleConnsPerHost)
	require.Equal(t, 0, tr.MaxConnsPerHost, "active connections must stay unlimited")
	require.Equal(t, 90*time.Second, tr.IdleConnTimeout)
	require.Equal(t, 30*time.Second, tr.ResponseHeaderTimeout)
	require.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
	require.Equal(t, 1*time.Second, tr.ExpectContinueTimeout)

	// Each call builds a fresh transport (the pool is what makes them shared).
	require.NotSame(t, tr, newDefaultTransport())
}

// TestSharedTransportPool_Global verifies the package-global instance the HTTP
// client uses is initialized and behaves like any other pool.
func TestSharedTransportPool_Global(t *testing.T) {
	require.NotNil(t, sharedTransportPool)
	a := sharedTransportPool.GetOrCreate("global-test-key-A")
	b := sharedTransportPool.GetOrCreate("global-test-key-A")
	require.Same(t, a, b)
}

// TestTransportPool_DedupByUniqueUpstreamKey is the end-to-end intent: when two
// projects declare the SAME upstream (identical id + endpoint + headers), their
// upstreams resolve to the same UniqueUpstreamKey and therefore share ONE
// transport — while an upstream with a different endpoint gets its own. This is
// the co-resident multi-project connection-pool deduplication.
func TestTransportPool_DedupByUniqueUpstreamKey(t *testing.T) {
	p := NewTransportPool()

	// Same id + endpoint + network → identical UniqueUpstreamKey (the same
	// upstream declared by two different projects).
	projectAUps := common.NewFakeUpstream("alchemy-eth")
	projectAUps.Config().Endpoint = "https://eth.example.com/v2/key"
	projectBUps := common.NewFakeUpstream("alchemy-eth")
	projectBUps.Config().Endpoint = "https://eth.example.com/v2/key"

	require.Equal(t, common.UniqueUpstreamKey(projectAUps), common.UniqueUpstreamKey(projectBUps),
		"identical upstream config must yield identical UniqueUpstreamKey")

	tA := p.GetOrCreate(common.UniqueUpstreamKey(projectAUps))
	tB := p.GetOrCreate(common.UniqueUpstreamKey(projectBUps))
	require.Same(t, tA, tB, "two projects on the same endpoint must share one connection pool")

	// A genuinely different endpoint keeps its own pool.
	otherUps := common.NewFakeUpstream("alchemy-base")
	otherUps.Config().Endpoint = "https://base.example.com/v2/key"
	tOther := p.GetOrCreate(common.UniqueUpstreamKey(otherUps))
	require.NotSame(t, tA, tOther)
	require.Equal(t, 2, p.size(), "one shared transport for the shared endpoint + one for the distinct endpoint")
}
