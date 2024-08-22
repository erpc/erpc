package vendors

import "github.com/erpc/erpc/common"

type VendorsRegistry struct {
	vendors []common.Vendor
}

func NewVendorsRegistry() *VendorsRegistry {
	r := &VendorsRegistry{}

	r.Register(CreateAlchemyVendor())
	r.Register(CreateBlastApiVendor())
	r.Register(CreateDrpcVendor())
	r.Register(CreateEnvioVendor())
	r.Register(CreateEtherspotVendor())
	r.Register(CreateInfuraVendor())
	r.Register(CreatePimlicoVendor())
	r.Register(CreateQuicknodeVendor())
	r.Register(CreateLlamaVendor())
	r.Register(CreateThirdwebVendor())

	return r
}

func (r *VendorsRegistry) LookupByUpstream(ups *common.UpstreamConfig) common.Vendor {
	if ups.VendorName != "" {
		for _, vendor := range r.vendors {
			if vendor.Name() == ups.VendorName {
				return vendor
			}
		}
		return nil
	} else {
		for _, vendor := range r.vendors {
			if vendor.OwnsUpstream(ups) {
				return vendor
			}
		}
	}

	return nil
}

func (r *VendorsRegistry) Register(vendor common.Vendor) {
	r.vendors = append(r.vendors, vendor)
}
