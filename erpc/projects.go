package erpc

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
)

type PreparedProject struct {
	Config   *common.ProjectConfig
	Networks map[string]*Network
	Logger   *zerolog.Logger

	networksRegistry *NetworksRegistry
	upstreamsRegistry *upstream.UpstreamsRegistry
	evmJsonRpcCache *EvmJsonRpcCache
}

func (p *PreparedProject) Bootstrap(ctx context.Context) error {
	for _, network := range p.Networks {
		err := network.Bootstrap(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *PreparedProject) GetNetwork(networkId string) (network *Network, err error) {
	network, ok := p.Networks[networkId]
	if !ok {
		network, err = p.loadNetwork(networkId)
		if err != nil {
			return nil, err
		}
		p.Networks[networkId] = network
	}
	return
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *upstream.NormalizedRequest) (common.NormalizedResponse, error) {
	network, err := p.GetNetwork(networkId)
	if err != nil {
		return nil, err
	}

	m, _ := nq.Method()
	p.Logger.Debug().Str("method", m).Msgf("forwarding request to network")
	resp, err := network.Forward(ctx, nq)

	if err == nil {
		p.Logger.Info().Msgf("successfully forward request for network")
		return resp, nil
	} else {
		p.Logger.Warn().Err(err).Msgf("failed to forward request for network")
	}

	return nil, err
}

func (p *PreparedProject) loadNetwork(networkId string) (*Network, error) {
	// 1) Find all upstreams that support this network
	var upstreams []common.Upstream
	pups, err := p.upstreamsRegistry.GetUpstreamsByProject(p.Config)
	if err != nil {
		return nil, err
	}
	for _, ups := range pups {
		if ups.SupportsNetwork(networkId) {
			upstreams = append(upstreams, ups)
		}
	}

	// 2) Find if any network configs defined on project-level
	var nwCfg *common.NetworkConfig
	for _, n := range p.Config.Networks {
		if n.NetworkId() == networkId {
			nwCfg = n
			break
		}
	}

	// 3) Register and prepare the network in registry
	nw, err := p.networksRegistry.RegisterNetwork(
		p.Logger,
		p.evmJsonRpcCache,
		p.Config,
		nwCfg,
	)
	if err != nil {
		return nil, err
	}

	return nw, nil
}