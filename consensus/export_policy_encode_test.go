package consensus

import (
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestResolveFilePattern_PolicyHexIsInjective(t *testing.T) {
	ts := time.UnixMilli(1_700_000_000_000).UTC()
	a := resolveFilePattern("{timestampMs}-{policy}-{method}", "eth_call", "evm:1", ts, "a/b")
	b := resolveFilePattern("{timestampMs}-{policy}-{method}", "eth_call", "evm:1", ts, "a_b")
	require.NotEqual(t, a, b, "sanitize collisions must not collapse {policy}")
	require.True(t, strings.Contains(a, common.ConsensusPolicyObjectKeyComponent("a/b")))
	require.True(t, strings.Contains(b, common.ConsensusPolicyObjectKeyComponent("a_b")))
}
