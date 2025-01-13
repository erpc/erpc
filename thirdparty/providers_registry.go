package thirdparty

import (
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type ProvidersRegistry struct {
	vendorReg *VendorsRegistry
	providers []*Provider
}

func NewProvidersRegistry(
	logger *zerolog.Logger,
	vendorReg *VendorsRegistry,
	providerCfgs []*common.ProviderConfig,
) (*ProvidersRegistry, error) {
	var providers []*Provider
	for _, cfg := range providerCfgs {
		vnd := vendorReg.LookupByName(cfg.Vendor)
		if vnd == nil {
			supportedVendors := vendorReg.SupportedVendors()
			return nil, fmt.Errorf("vendor '%s' not found for provider '%s', supported vendors: %v", cfg.Vendor, cfg.Id, supportedVendors)
		}
		providers = append(providers, NewProvider(logger, cfg, vnd))
	}
	return &ProvidersRegistry{
		vendorReg: vendorReg,
		providers: providers,
	}, nil
}

func (pr *ProvidersRegistry) GetAllProviders() []*Provider {
	return pr.providers
}
