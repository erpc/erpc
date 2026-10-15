package consensus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// Two named-policy exporters inheriting the same file destination must not
// tear NDJSON frames under concurrent AppendWithMetadata — the shared path
// lock serializes framed writes across independent exporter instances.
func TestFileExporter_SharedPathConcurrentAppendsAreValidNDJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := &common.MisbehaviorsDestinationConfig{
		Type:        common.MisbehaviorsDestinationTypeFile,
		Path:        dir,
		FilePattern: "shared.ndjson",
	}
	log := zerolog.Nop()

	a, err := newFileMisbehaviorExporter(cfg, &log, "policy-a")
	require.NoError(t, err)
	b, err := newFileMisbehaviorExporter(cfg, &log, "policy-b")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			// Large-ish payload so a torn write would be visible even if
			// O_APPEND atomicity covered small PIPE_BUF writes.
			line := []byte(fmt.Sprintf(`{"policy":"a","i":%d,"pad":%q}`, i, fmt.Sprintf("%0800d", i)))
			require.NoError(t, a.AppendWithMetadata(line, "eth_call", "evm:1"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			line := []byte(fmt.Sprintf(`{"policy":"b","i":%d,"pad":%q}`, i, fmt.Sprintf("%0800d", i)))
			require.NoError(t, b.AppendWithMetadata(line, "eth_call", "evm:1"))
		}
	}()
	wg.Wait()

	f, err := os.Open(filepath.Join(dir, "shared.ndjson"))
	require.NoError(t, err)
	defer f.Close()

	counts := map[string]int{}
	sc := bufio.NewScanner(f)
	// Records are ~800+ bytes; raise the token limit past the default 64KiB.
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		var row map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &row), "line must be valid JSON: %q", sc.Text())
		p, _ := row["policy"].(string)
		counts[p]++
	}
	require.NoError(t, sc.Err())
	require.Equal(t, n, counts["a"])
	require.Equal(t, n, counts["b"])
}

func TestFileExporter_CloseReleasesSharedHub(t *testing.T) {
	dir := t.TempDir()
	cfg := &common.MisbehaviorsDestinationConfig{
		Type:        common.MisbehaviorsDestinationTypeFile,
		Path:        dir,
		FilePattern: "close-test.ndjson",
	}
	log := zerolog.Nop()

	a, err := newFileMisbehaviorExporter(cfg, &log, "policy-a")
	require.NoError(t, err)
	b, err := newFileMisbehaviorExporter(cfg, &log, "policy-b")
	require.NoError(t, err)

	require.NoError(t, a.AppendWithMetadata([]byte(`{"a":1}`), "eth_call", "evm:1"))
	require.NoError(t, b.AppendWithMetadata([]byte(`{"b":1}`), "eth_call", "evm:1"))

	base := filepath.Clean(dir)
	hubLive, ok := sharedFileHubs.Load(base)
	require.True(t, ok, "hub should exist while exporters are live")

	require.NoError(t, a.Close())
	_, ok = sharedFileHubs.Load(base)
	require.True(t, ok, "hub must stay while a sibling exporter is still live")

	require.NoError(t, b.Close())
	hubAfter, ok := sharedFileHubs.Load(base)
	require.True(t, ok, "hub must be retained at zero refs so the path keeps one lock")
	require.Same(t, hubLive, hubAfter)

	hubAfter.(*sharedFileHub).mu.Lock()
	require.Equal(t, 0, hubAfter.(*sharedFileHub).refs)
	require.Empty(t, hubAfter.(*sharedFileHub).handles)
	hubAfter.(*sharedFileHub).mu.Unlock()

	// A later exporter for the same path must reuse the retained hub — never
	// split appends across two mutexes after a Close/LoadOrStore race.
	c, err := newFileMisbehaviorExporter(cfg, &log, "policy-c")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Same(t, hubAfter, c.hub)
	require.NoError(t, c.AppendWithMetadata([]byte(`{"c":1}`), "eth_call", "evm:1"))
}

// Append after Close must fail; a new exporter for the same path reuses the
// retained zero-ref hub rather than installing a second lock.
func TestFileExporter_AppendAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	cfg := &common.MisbehaviorsDestinationConfig{
		Type:        common.MisbehaviorsDestinationTypeFile,
		Path:        dir,
		FilePattern: "closed.ndjson",
	}
	log := zerolog.Nop()

	a, err := newFileMisbehaviorExporter(cfg, &log, "policy-a")
	require.NoError(t, err)
	hubBefore := a.hub
	require.NoError(t, a.Close())
	require.Error(t, a.AppendWithMetadata([]byte(`{"a":1}`), "eth_call", "evm:1"))

	b, err := newFileMisbehaviorExporter(cfg, &log, "policy-b")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.Same(t, hubBefore, b.hub)
	require.NoError(t, b.AppendWithMetadata([]byte(`{"b":1}`), "eth_call", "evm:1"))
}
