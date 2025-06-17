package thirdparty

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type Provider struct {
	logger           *zerolog.Logger
	config           *common.ProviderConfig
	vendor           common.Vendor
	upstreamDefaults *common.UpstreamConfig
}

func NewProvider(logger *zerolog.Logger, cfg *common.ProviderConfig, vendor common.Vendor, upstreamDefaults *common.UpstreamConfig) *Provider {
	return &Provider{
		logger:           logger,
		config:           cfg,
		vendor:           vendor,
		upstreamDefaults: upstreamDefaults,
	}
}

func (p *Provider) Id() string {
	return p.config.Id
}

func (p *Provider) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
	if p.config.IgnoreNetworks != nil {
		for _, n := range p.config.IgnoreNetworks {
			if n == networkId {
				return false, nil
			}
		}
	}

	if p.config.OnlyNetworks != nil {
		for _, n := range p.config.OnlyNetworks {
			if n == networkId {
				return true, nil
			}
		}
		return false, nil
	}

	return p.vendor.SupportsNetwork(ctx, p.logger, p.config.Settings, networkId)
}

func (p *Provider) GenerateUpstreamConfigs(ctx context.Context, logger *zerolog.Logger, networkId string) ([]*common.UpstreamConfig, error) {
	upsCfg, err := p.buildBaseUpstreamConfig(networkId)
	if err != nil {
		return nil, err
	}
	upsCfgs, err := p.vendor.GenerateConfigs(ctx, logger, upsCfg, p.config.Settings)
	if err != nil {
		return nil, err
	}
	p.expandEnvVars(upsCfgs)
	return upsCfgs, nil
}

func (p *Provider) expandEnvVars(upsCfgs []*common.UpstreamConfig) {
	for _, upsCfg := range upsCfgs {
		upsCfg.Endpoint = os.ExpandEnv(upsCfg.Endpoint)
	}
}

// buildBaseUpstreamConfig uses the ProviderConfig's Overrides map to find an
// upstream override whose key can wildcard-match the given networkId. If it finds
// a match, it copies that UpstreamConfig. Otherwise, it creates an empty base
// config. Then it applies the UpstreamIdTemplate to generate the new upstream ID.
func (p *Provider) buildBaseUpstreamConfig(networkId string) (*common.UpstreamConfig, error) {
	var baseCfg *common.UpstreamConfig

	// Look for a matching override in the ProviderConfig.Overrides using wildcard match.
	for pattern, override := range p.config.Overrides {
		matches, err := common.WildcardMatch(pattern, networkId)
		if err != nil {
			// If there's an error in matching logic, log or handle as you see fit; skip in this example.
			continue
		}
		if matches {
			baseCfg = override.Copy()
			break
		}
	}

	// If no override matched, create a fresh UpstreamConfig as a baseline.
	if baseCfg == nil {
		baseCfg = &common.UpstreamConfig{}
		err := baseCfg.SetDefaults(p.upstreamDefaults)
		baseCfg.Id = ""
		if err != nil {
			return nil, err
		}
	}
	baseCfg.VendorName = p.config.Vendor

	// Substitute into the UpstreamIdTemplate. We replace:
	//    <VENDOR>    with the provider's Vendor name (p.config.Vendor)
	//    <PROVIDER>  with the provider ID from config (p.config.Id)
	//    <NETWORK>   with the entire networkId string
	//    <EVM_CHAIN_ID> with the number part of networkId if it's in "evm:####" format, else empty
	baseCfg.Id = applyUpstreamIDTemplate(
		p.config.UpstreamIdTemplate,
		p.config.Vendor,
		p.config.Id,
		networkId,
	)

	if strings.HasPrefix(networkId, "evm:") {
		baseCfg.Type = common.UpstreamTypeEvm
		chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
		if err != nil {
			return nil, err
		}
		if baseCfg.Evm == nil {
			baseCfg.Evm = &common.EvmUpstreamConfig{}
		}
		baseCfg.Evm.ChainId = chainId

		var defs *common.EvmUpstreamConfig
		if p.upstreamDefaults != nil && p.upstreamDefaults.Evm != nil {
			defs = p.upstreamDefaults.Evm
		}
		err = baseCfg.Evm.SetDefaults(defs)
		if err != nil {
			return nil, err
		}
	}

	return baseCfg, nil
}

// applyUpstreamIDTemplate handles the keyword replacements in the UpstreamIdTemplate string.
func applyUpstreamIDTemplate(
	template string,
	vendorName string,
	providerId string,
	networkId string,
) string {
	result := template
	result = strings.ReplaceAll(result, "<VENDOR>", vendorName)
	result = strings.ReplaceAll(result, "<PROVIDER>", providerId)
	result = strings.ReplaceAll(result, "<NETWORK>", networkId)

	// If network is in "evm:<someChainId>" format, then <EVM_CHAIN_ID> = <someChainId>.
	// Otherwise, we replace that placeholder with an empty string.
	if strings.HasPrefix(networkId, "evm:") {
		evmChainId := strings.TrimPrefix(networkId, "evm:")
		result = strings.ReplaceAll(result, "<EVM_CHAIN_ID>", evmChainId)
	} else {
		result = strings.ReplaceAll(result, "<EVM_CHAIN_ID>", "N/A")
	}

	return result
}
