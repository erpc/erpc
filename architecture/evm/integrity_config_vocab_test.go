package evm

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Drift guard: config validation (common) must accept exactly the behavior
// vocabulary the runtime (parseBehavior) understands — otherwise validation
// either rejects working configs or lets a silently-ignored value through.
func TestIntegrityBehaviorVocabMatchesValidation(t *testing.T) {
	accepted := []string{"reject", "error", "hard-fail", "soft-flag", "softflag", "record", "warn", "off", "ignore", "none", " Reject "}
	for _, v := range accepted {
		_, ok := parseBehavior(v)
		require.True(t, ok, "runtime must parse %q", v)
		cfg := &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
			InvalidBehavior: &common.IntegrityInvalidBehaviorConfig{Finalized: v},
		}}
		assert.NoError(t, cfg.Validate(), "validation must accept %q (runtime parses it)", v)
	}
	for _, v := range []string{"rejct", "soft flag", "flag", "true"} {
		_, ok := parseBehavior(v)
		require.False(t, ok, "runtime must NOT parse %q", v)
		cfg := &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
			InvalidBehavior: &common.IntegrityInvalidBehaviorConfig{Finalized: v},
		}}
		assert.Error(t, cfg.Validate(), "validation must reject %q (runtime silently ignores it)", v)
	}
}

// Every registered check id must be accepted by config validation (the ids are
// registered into common's catalog at init), and case matters.
func TestIntegrityCheckIDCatalogRegistered(t *testing.T) {
	cfg := &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
		Checks: map[string]*common.IntegrityCheckConfig{"transactionsRootConsistency": {}},
	}}
	assert.NoError(t, cfg.Validate())

	bad := &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
		Checks: map[string]*common.IntegrityCheckConfig{"transactionsrootconsistency": {}},
	}}
	assert.Error(t, bad.Validate(), "check ids are case-sensitive in config")
}
