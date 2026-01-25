package thirdparty

import "github.com/erpc/erpc/common"

type VendorsRegistry struct {
	thirdparty []common.Vendor
}

func NewVendorsRegistry() *VendorsRegistry {
	r := &VendorsRegistry{}

	r.Register(CreateAlchemyVendor())
	r.Register(CreateBlastApiVendor())
	r.Register(CreateConduitVendor())
	r.Register(CreateDrpcVendor())
	r.Register(CreateDwellirVendor())
	r.Register(CreateEnvioVendor())
	r.Register(CreateEtherspotVendor())
	r.Register(CreateInfuraVendor())
	r.Register(CreatePimlicoVendor())
	r.Register(CreateQuicknodeVendor())
	r.Register(CreateLlamaVendor())
	r.Register(CreateThirdwebVendor())
	r.Register(CreateRepositoryVendor())
	r.Register(CreateSuperchainVendor())
	r.Register(CreateTenderlyVendor())
	r.Register(CreateChainstackVendor())
	r.Register(CreateOnFinalityVendor())
	r.Register(CreateErpcVendor())
	r.Register(CreateBlockPiVendor())
	r.Register(CreateAnkrVendor())
	r.Register(CreateRoutemeshVendor())
	r.Register(CreateSqdVendor())
	return r
}

func (r *VendorsRegistry) SupportedVendors() []string {
	vendors := []string{}
	for _, vendor := range r.thirdparty {
		vendors = append(vendors, vendor.Name())
	}
	return vendors
}

func (r *VendorsRegistry) LookupByName(name string) common.Vendor {
	for _, vendor := range r.thirdparty {
		if vendor.Name() == name {
			return vendor
		}
	}
	return nil
}

func (r *VendorsRegistry) LookupByUpstream(ups *common.UpstreamConfig) common.Vendor {
	if ups.VendorName != "" {
		for _, vendor := range r.thirdparty {
			if vendor.Name() == ups.VendorName {
				return vendor
			}
		}
		return nil
	} else {
		for _, vendor := range r.thirdparty {
			if vendor.OwnsUpstream(ups) {
				return vendor
			}
		}
	}

	return nil
}

func (r *VendorsRegistry) Register(vendor common.Vendor) {
	r.thirdparty = append(r.thirdparty, vendor)
}
