package upstream

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// svmVerifyGenesisHash guards against a mis-configured Solana upstream pointing
// at the wrong cluster (e.g. an upstream listed under mainnet-beta that actually
// serves devnet). It runs once at bootstrap.
//
// Known clusters (mainnet-beta, devnet, testnet) issue one getGenesisHash RPC
// at bootstrap and compare the result against the hardcoded genesis-hash
// table — this catches mis-pointed upstreams that a purely local check would
// miss. Unknown clusters are verified via the same RPC only when
// CheckGenesisHash:true is set; otherwise we skip silently to support
// private/local clusters where no genesis hash is known up front.
func (u *Upstream) svmVerifyGenesisHash(ctx context.Context) error {
	cfg := u.config
	if cfg == nil || cfg.Svm == nil {
		return nil
	}
	cluster := cfg.Svm.Cluster
	if cluster == "" {
		return nil
	}
	chain := cfg.Svm.Chain

	expected, known := common.KnownGenesisHash(chain, cluster)

	if !known && !cfg.Svm.CheckGenesisHash {
		u.logger.Debug().Str("cluster", cluster).
			Msg("skipping svm genesis hash validation: unknown cluster without checkGenesisHash")
		return nil
	}

	actual, err := u.svmFetchGenesisHash(ctx)
	if err != nil {
		// Treat fetch failures as non-fatal unless the operator explicitly opted in.
		if cfg.Svm.CheckGenesisHash {
			return common.NewErrUpstreamClientInitialization(
				fmt.Errorf("svm getGenesisHash failed: %w", err),
				u,
			)
		}
		u.logger.Warn().Err(err).Str("cluster", cluster).
			Msg("svm getGenesisHash failed; continuing (checkGenesisHash not set)")
		return nil
	}

	if known && expected != "" && !strings.EqualFold(actual, expected) {
		return common.NewErrUpstreamClientInitialization(
			fmt.Errorf("svm genesis hash mismatch for chain=%q cluster=%q: expected %s, got %s", common.ResolveSvmChain(chain), cluster, expected, actual),
			u,
		)
	}
	u.logger.Debug().Str("cluster", cluster).Str("genesisHash", actual).
		Msg("svm genesis hash validated")
	return nil
}

func (u *Upstream) svmFetchGenesisHash(ctx context.Context) (string, error) {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	resp, err := u.Forward(ctx, req, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return "", err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	var hash string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &hash); err != nil {
		return "", fmt.Errorf("decode genesis hash: %w", err)
	}
	return hash, nil
}

// SvmStatePoller exposes the per-upstream SVM slot tracker for hooks and tests.
func (u *Upstream) SvmStatePoller() common.SvmStatePoller {
	return u.svmStatePoller
}
