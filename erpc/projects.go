package erpc

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog"
)

type PreparedProject struct {
	Config   *config.ProjectConfig
	Networks map[string]*PreparedNetwork
	Logger   *zerolog.Logger
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

func (p *PreparedProject) GetNetwork(networkId string) (*PreparedNetwork, error) {
	network, ok := p.Networks[networkId]
	if !ok {
		return nil, common.NewErrNetworkNotFound(networkId)
	}
	return network, nil
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *common.NormalizedRequest, w common.ResponseWriter) error {
	network, err := p.GetNetwork(networkId)
	if err != nil {
		return err
	}

	m, _ := nq.Method()
	p.Logger.Debug().Str("method", m).Msgf("forwarding request to network")
	err = network.Forward(ctx, nq, w)

	if err == nil {
		p.Logger.Info().Msgf("successfully forward request for network")
		return nil
	} else {
		p.Logger.Warn().Err(err).Msgf("failed to forward request for network")
	}

	return err
}
