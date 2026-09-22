package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// TestSvmVerifyGenesisHash_UnknownClusterWithoutCheck_Skips guards the invariant
// the fail-closed simplification depends on: an unknown cluster with
// checkGenesisHash unset returns nil WITHOUT attempting a getGenesisHash fetch.
// (u.Client is nil here, so any Forward would panic — reaching it at all would
// fail this test.) This is the ONLY non-fatal genesis path; every case that
// reaches the fetch is fail-closed, so a fetch error there is always fatal.
func TestSvmVerifyGenesisHash_UnknownClusterWithoutCheck_Skips(t *testing.T) {
	t.Parallel()
	u := &Upstream{
		config: &common.UpstreamConfig{
			Id:   "svm-test",
			Type: common.UpstreamTypeSvm,
			Svm: &common.SvmUpstreamConfig{
				Chain:   "fogo",    // not in knownSvmGenesisHashes
				Cluster: "mainnet", // unknown cluster
			},
		},
		logger: &zerolog.Logger{},
	}
	if err := u.svmVerifyGenesisHash(context.Background()); err != nil {
		t.Fatalf("unknown cluster without checkGenesisHash should skip silently, got: %v", err)
	}
}

// TestSvmVerifyGenesisHash_NoSvmConfig_Skips: a non-SVM / unconfigured upstream
// is a no-op (defensive — the caller only invokes this for SVM upstreams).
func TestSvmVerifyGenesisHash_NoSvmConfig_Skips(t *testing.T) {
	t.Parallel()
	u := &Upstream{
		config: &common.UpstreamConfig{Id: "evm-test"},
		logger: &zerolog.Logger{},
	}
	if err := u.svmVerifyGenesisHash(context.Background()); err != nil {
		t.Fatalf("nil Svm config should be a no-op, got: %v", err)
	}
}
