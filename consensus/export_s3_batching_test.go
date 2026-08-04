package consensus

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeS3 captures uploads instead of talking to AWS.
type fakeS3 struct {
	mu     sync.Mutex
	keys   []string
	bodies []string
	err    error
}

func (f *fakeS3) PutObject(in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	body, _ := io.ReadAll(in.Body)
	f.keys = append(f.keys, *in.Key)
	f.bodies = append(f.bodies, string(body))
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys)
}

func newTestExporter(t *testing.T, fake *fakeS3, maxRecords int, flush time.Duration) *s3MisbehaviorExporter {
	t.Helper()
	lg := zerolog.Nop()
	return &s3MisbehaviorExporter{
		cfg: &common.MisbehaviorsDestinationConfig{
			Type:        "s3",
			Path:        "s3://bucket/prefix/",
			FilePattern: "{timestampMs}-{method}-{networkId}",
			S3: &common.S3FlushConfig{
				MaxRecords:    maxRecords,
				MaxSize:       1 << 20,
				FlushInterval: common.Duration(flush),
				ContentType:   "application/jsonl",
			},
		},
		log:       &lg,
		s3Client:  fake,
		bucket:    "bucket",
		keyPrefix: "prefix/",
		batches:   make(map[string]*pendingBatch),
		flushCh:   make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
	}
}

// Records must ACCUMULATE into one object. The buffer used to be keyed by the
// resolved file name, and the default pattern embeds {timestampMs} resolved at
// append time — so every record landed in its own bucket. maxRecords/maxSize
// could then never be reached (count was always 1), which silently disabled
// batching entirely: one S3 object per catch, and the only remaining flush path
// was the periodic tick.
func TestS3ExporterBatchesRecordsIntoOneObject(t *testing.T) {
	fake := &fakeS3{}
	const records = 20
	e := newTestExporter(t, fake, records, time.Hour)

	for i := 0; i < records; i++ {
		// Spread the appends across milliseconds. Real catches arrive that far
		// apart at minimum, and it is exactly what defeated the old keying: each
		// record resolved a distinct {timestampMs} and got its own buffer. A
		// tight sub-millisecond loop would pass even against the old code.
		time.Sleep(1100 * time.Microsecond)
		require.NoError(t, e.AppendWithMetadata([]byte(fmt.Sprintf(`{"i":%d}`, i)), "eth_getBlockByNumber", "evm:1"))
	}

	require.Len(t, e.batches, 1, "records of the same method+network must share ONE batch regardless of arrival time")
	assert.True(t, e.shouldFlush(), "maxRecords must be reachable — it never was when each record had its own key")

	require.NoError(t, e.flush())

	require.Equal(t, 1, fake.uploads(), "must upload a single batched object, not one object per record")
	assert.Equal(t, records, strings.Count(fake.bodies[0], "\n"), "all records belong in that object")
	assert.Contains(t, fake.keys[0], "prefix/")
	assert.Contains(t, fake.keys[0], "eth_getBlockByNumber")
	assert.Empty(t, e.batches, "a flushed batch must be dropped, not kept — a Reset() buffer retains its capacity forever")
}

// Distinct (method, networkId) pairs stay in distinct objects.
func TestS3ExporterKeepsGroupsSeparate(t *testing.T) {
	fake := &fakeS3{}
	e := newTestExporter(t, fake, 100, time.Hour)

	require.NoError(t, e.AppendWithMetadata([]byte(`{"a":1}`), "eth_getLogs", "evm:1"))
	time.Sleep(1100 * time.Microsecond)
	require.NoError(t, e.AppendWithMetadata([]byte(`{"a2":1}`), "eth_getLogs", "evm:1"))
	require.NoError(t, e.AppendWithMetadata([]byte(`{"b":2}`), "eth_getLogs", "evm:137"))
	require.NoError(t, e.AppendWithMetadata([]byte(`{"c":3}`), "eth_getBlockByNumber", "evm:1"))

	require.Len(t, e.batches, 3, "grouping is by (method, networkId) — arrival time must not split a group")
	require.NoError(t, e.flush())
	assert.Equal(t, 3, fake.uploads())
	assert.Empty(t, e.batches)
}

// shouldFlush is about thresholds ONLY. It used to also return true once
// flushInterval had elapsed, and the ticker consulted it before flushing —
// which is what made the periodic flush skip every other tick (the previous
// flush stamped its completion just after its own tick, leaving the next tick a
// few microseconds short of the interval).
func TestS3ExporterShouldFlushIsThresholdOnly(t *testing.T) {
	fake := &fakeS3{}
	e := newTestExporter(t, fake, 100, time.Nanosecond) // interval already elapsed

	require.NoError(t, e.AppendWithMetadata([]byte(`{"only":1}`), "eth_getLogs", "evm:1"))
	time.Sleep(2 * time.Millisecond)

	assert.False(t, e.shouldFlush(), "a sub-threshold batch must not report ready just because time passed")
}

// The periodic tick must upload sub-threshold batches — repeatedly. This is the
// path every rare catch depends on: with maxRecords unreachable (see above), a
// lone integrity catch is written ONLY by this tick, and if it is skipped the
// record sits in memory and is lost outright when the pod restarts.
func TestS3ExporterPeriodicFlushUploadsSubThresholdRecords(t *testing.T) {
	fake := &fakeS3{}
	e := newTestExporter(t, fake, 100, 20*time.Millisecond)

	e.closeWg.Add(1)
	go e.flushWorker()
	defer func() {
		close(e.closeCh)
		e.closeWg.Wait()
	}()

	require.NoError(t, e.AppendWithMetadata([]byte(`{"first":1}`), "eth_getLogs", "evm:1"))
	require.Eventually(t, func() bool { return fake.uploads() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"the first sub-threshold record must be flushed by the ticker")

	require.NoError(t, e.AppendWithMetadata([]byte(`{"second":2}`), "eth_getLogs", "evm:1"))
	require.Eventually(t, func() bool { return fake.uploads() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"consecutive ticks must each flush; the periodic path must not stall")
}

// A failed upload keeps the batch so the next flush retries it.
func TestS3ExporterRetainsBatchOnUploadError(t *testing.T) {
	fake := &fakeS3{err: fmt.Errorf("boom")}
	e := newTestExporter(t, fake, 100, time.Hour)

	require.NoError(t, e.AppendWithMetadata([]byte(`{"x":1}`), "eth_getLogs", "evm:1"))
	require.NoError(t, e.flush())
	require.Len(t, e.batches, 1, "a failed upload must not drop the records")

	fake.mu.Lock()
	fake.err = nil
	fake.mu.Unlock()

	require.NoError(t, e.flush())
	assert.Equal(t, 1, fake.uploads())
	assert.Empty(t, e.batches)
}

// A pattern with no {method}/{networkId} resolves every batch to the same name.
// An S3 PUT overwrites, so without disambiguation the second batch would
// silently erase the first.
func TestS3ExporterDoesNotOverwriteWithinAFlush(t *testing.T) {
	fake := &fakeS3{}
	e := newTestExporter(t, fake, 100, time.Hour)
	e.cfg.FilePattern = "{dateByDay}"

	require.NoError(t, e.AppendWithMetadata([]byte(`{"a":1}`), "eth_getLogs", "evm:1"))
	require.NoError(t, e.AppendWithMetadata([]byte(`{"b":2}`), "eth_getBlockByNumber", "evm:137"))
	require.NoError(t, e.flush())

	require.Equal(t, 2, fake.uploads())
	assert.NotEqual(t, fake.keys[0], fake.keys[1], "each object needs its own key or one silently overwrites the other")
}

// Buffered records must survive a graceful shutdown. Before Close() existed,
// a pod roll dropped everything written since the last flush interval — which
// is how a month of integrity catches vanished while the archive still looked
// healthy: the only batches that ever landed were those whose ticker happened
// to fire before the pod was replaced.
func TestS3ExporterClosePerformsFinalFlush(t *testing.T) {
	fake := &fakeS3{}
	// A long interval and a high record threshold so NEITHER the ticker nor the
	// size/count trigger can flush — only Close() can.
	e := newTestExporter(t, fake, 10_000, time.Hour)

	require.NoError(t, e.AppendWithMetadata([]byte(`{"catch":1}`), "eth_getBlockByNumber", "evm:999"))
	require.NoError(t, e.AppendWithMetadata([]byte(`{"catch":2}`), "eth_getBlockByNumber", "evm:999"))
	require.Equal(t, 0, fake.uploads(), "nothing should have flushed yet")

	require.NoError(t, e.Close())

	require.Equal(t, 1, fake.uploads(), "Close must flush the buffered batch")
	fake.mu.Lock()
	body := fake.bodies[0]
	fake.mu.Unlock()
	assert.Contains(t, body, `{"catch":1}`)
	assert.Contains(t, body, `{"catch":2}`, "no buffered record may be dropped")
}

func TestS3ExporterCloseIsIdempotent(t *testing.T) {
	fake := &fakeS3{}
	e := newTestExporter(t, fake, 10_000, time.Hour)
	require.NoError(t, e.AppendWithMetadata([]byte(`{"catch":1}`), "m", "evm:1"))
	require.NoError(t, e.Close())
	require.NoError(t, e.Close(), "second Close must not panic on a closed channel")
	assert.Equal(t, 1, fake.uploads(), "and must not re-upload")
}

// With a worker running, Close must still flush exactly once and not deadlock.
func TestS3ExporterCloseWithWorkerRunning(t *testing.T) {
	fake := &fakeS3{}
	e := newTestExporter(t, fake, 10_000, time.Hour)
	e.closeWg.Add(1)
	go e.flushWorker()

	require.NoError(t, e.AppendWithMetadata([]byte(`{"catch":9}`), "m", "evm:1"))
	done := make(chan error, 1)
	go func() { done <- e.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked with a worker running")
	}
	assert.Equal(t, 1, fake.uploads(), "exactly one upload despite worker + Close both flushing")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Contains(t, fake.bodies[0], `{"catch":9}`)
}
