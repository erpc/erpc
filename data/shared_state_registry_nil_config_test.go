package data

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewSharedStateRegistry_NilConfig_FallsBackToMemoryWithWarn verifies that
// passing a nil SharedStateConfig no longer panics (as it did previously when
// the constructor dereferenced cfg.Connector). Instead it falls back to an
// in-memory connector and logs a WARN so operators who intended a distributed
// backend don't silently lose cross-replica state propagation.
func TestNewSharedStateRegistry_NilConfig_FallsBackToMemoryWithWarn(t *testing.T) {
	// Other tests in this package call util.ConfigureTestLogger in init(),
	// which disables the global level. Re-enable warn-level for this test.
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
	defer zerolog.SetGlobalLevel(prev)

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	reg, err := NewSharedStateRegistry(context.Background(), &logger, nil)
	require.NoError(t, err, "nil config must no longer return an error / panic")
	require.NotNil(t, reg, "registry must be non-nil")

	// The registry must still be usable — a trivial counter get should work.
	counter := reg.GetCounterInt64("test-key", 0)
	require.NotNil(t, counter)
	assert.Equal(t, int64(0), counter.GetValue())

	// Verify the WARN log fired with a message operators can grep for.
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["level"] == "warn" && strings.Contains(entry["message"].(string), "in-memory") {
			found = true
			break
		}
	}
	assert.True(t, found,
		"a WARN log must surface the nil-config fallback so operators notice cross-replica state won't propagate; got logs: %s",
		buf.String())
}
