package adapter

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/util"
)

type EvmUpstreamAdapter struct {
	ups common.Upstream
}

func NewEvmUpstreamAdapter(ups common.Upstream) *EvmUpstreamAdapter {
	return &EvmUpstreamAdapter{ups: ups}
}

func (a *EvmUpstreamAdapter) Name() string {
	return "evm"
}

func (a *EvmUpstreamAdapter) SupportsNetwork(networkId string) (bool, error) {
	cfg := a.ups.Config()
	if cfg.Evm != nil {
		if cfg.Evm.ChainId > 0 && util.EvmNetworkId(cfg.Evm.ChainId) == networkId {
			return true, nil
		}
	}

	if cfg.Evm == nil || cfg.Evm.ChainId == 0 {
		cid, err := a.ups.EvmGetChainId(context.Background())
		if err != nil {
			return false, err
		}
		return util.EvmNetworkId(cid) == networkId, nil
	}

	return false, nil
}

func (a *EvmUpstreamAdapter) PreRequestHook(req common.NormalizedRequest) error {
	return nil
}

func (a *EvmUpstreamAdapter) AfterResponseHook(req common.NormalizedRequest, resp common.NormalizedResponse, respErr error) error {
	return nil
}
