package erpc

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type PreparedProject struct {
	Config   *common.ProjectConfig
	Networks map[string]*Network
	Logger   *zerolog.Logger

	appCtx            context.Context
	networksMu        sync.RWMutex
	networksRegistry  *NetworksRegistry
	upstreamsRegistry *upstream.UpstreamsRegistry
	evmJsonRpcCache   *EvmJsonRpcCache
}

func (p *PreparedProject) GetNetwork(networkId string) (network *Network, err error) {
	p.networksMu.RLock()
	network, ok := p.Networks[networkId]
	p.networksMu.RUnlock()
	if !ok {
		p.networksMu.Lock()
		defer p.networksMu.Unlock()
		network, err = p.initializeNetwork(networkId)
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
	method, _ := nq.Method()

	timer := prometheus.NewTimer(health.MetricNetworkRequestDuration.WithLabelValues(
		network.ProjectId,
		network.NetworkId,
		method,
	))
	defer timer.ObserveDuration()

	health.MetricNetworkRequestsReceived.WithLabelValues(network.ProjectId, network.NetworkId, method).Inc()
	p.Logger.Debug().Str("method", method).Msgf("forwarding request to network")
	resp, err := network.Forward(ctx, nq)

	if err == nil {
		p.Logger.Info().Msgf("successfully forward request for network")
		health.MetricNetworkSuccessfulRequests.WithLabelValues(network.ProjectId, network.NetworkId, method).Inc()
		return resp, nil
	} else {
		p.Logger.Warn().Err(err).Msgf("failed to forward request for network")
		health.MetricNetworkFailedRequests.WithLabelValues(network.ProjectId, network.NetworkId, method, common.ErrorSummary(err)).Inc()
	}

	return nil, err
}

func (p *PreparedProject) initializeNetwork(networkId string) (*Network, error) {
	// 1) Find all upstreams that support this network
	err := p.upstreamsRegistry.PrepareUpstreamsForNetwork(networkId)
	if err != nil {
		return nil, err
	}

	// 2) Find if any network configs defined on project-level
	var nwCfg *common.NetworkConfig
	for _, n := range p.Config.Networks {
		if n.NetworkId() == networkId {
			nwCfg = n
			break
		}
	}

	if nwCfg == nil {
		nwCfg = &common.NetworkConfig{}

		s := strings.Split(networkId, ":")
		if len(s) != 2 {
			// TODO use more appropriate error for non-evm
			return nil, common.NewErrInvalidEvmChainId(networkId)
		}
		nwCfg.Architecture = common.NetworkArchitecture(s[0])
		switch nwCfg.Architecture {
		case common.ArchitectureEvm:
			c, e := strconv.Atoi(s[1])
			if e != nil {
				return nil, e
			}
			nwCfg.Evm = &common.EvmNetworkConfig{
				ChainId: c,
			}
		}
	}

	// 3) Register and prepare the network in registry
	nw, err := p.networksRegistry.RegisterNetwork(
		p.Logger,
		p.Config,
		nwCfg,
	)
	if err != nil {
		return nil, err
	}

	err = nw.Bootstrap(p.appCtx)
	if err != nil {
		return nil, err
	}

	return nw, nil
}
