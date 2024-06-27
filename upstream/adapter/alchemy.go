package adapter

import "github.com/flair-sdk/erpc/common"

type AlchemyUpstreamAdapter struct {
	ups common.Upstream
}

func NewAlchemyUpstreamAdapter(ups common.Upstream) *AlchemyUpstreamAdapter {
	return &AlchemyUpstreamAdapter{ups: ups}
}

func (a *AlchemyUpstreamAdapter) Name() string {
	return "alchemy"
}

func (a *AlchemyUpstreamAdapter) SupportsNetwork(networkId string) (bool, error) {
	// TODO check against a hard-coded list of chainId -> domain for alchemy
	return false, nil
}

func (a *AlchemyUpstreamAdapter) PreRequestHook(req common.NormalizedRequest) error {
	return nil
}

func (a *AlchemyUpstreamAdapter) AfterResponseHook(req common.NormalizedRequest, resp common.NormalizedResponse, respErr error) error {
	return nil
}
