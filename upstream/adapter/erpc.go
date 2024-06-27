package adapter

import "github.com/flair-sdk/erpc/common"

type ErpcUpstreamAdapter struct {
	ups common.Upstream
}

func NewErpcUpstreamAdapter(ups common.Upstream) *ErpcUpstreamAdapter {
	return &ErpcUpstreamAdapter{ups: ups}
}

func (a *ErpcUpstreamAdapter) Name() string {
	return "erpc"
}

func (a *ErpcUpstreamAdapter) SupportsNetwork(networkId string) (bool, error) {
	// TODO check against a cached call towards erpc_supportedNetworks
	return false, nil
}

func (a *ErpcUpstreamAdapter) PreRequestHook(req common.NormalizedRequest) error {
	return nil
}

func (a *ErpcUpstreamAdapter) AfterResponseHook(req common.NormalizedRequest, resp common.NormalizedResponse, respErr error) error {
	return nil
}
