package adapter

import (
	"fmt"

	"github.com/flair-sdk/erpc/common"
)

type Adapter interface {
	Name() string
	SupportsNetwork(networkId string) (bool, error)
	PreRequestHook(req common.NormalizedRequest) error
	AfterResponseHook(req common.NormalizedRequest, resp common.NormalizedResponse, respErr error) error
}

func CreateAdapter(ups common.Upstream) (Adapter, error) {
	switch ups.Config().Type {
	case common.UpstreamTypeEvm:
		return NewEvmUpstreamAdapter(ups), nil
	case common.UpstreamTypeEvmErpc:
		return NewErpcUpstreamAdapter(ups), nil
	case common.UpstreamTypeEvmAlchemy:
		return NewAlchemyUpstreamAdapter(ups), nil
	default:
		return nil, fmt.Errorf("unknown upstream type to while getting adapter: %s", ups.Config().Type)
	}
}
