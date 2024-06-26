package vendors

import "github.com/flair-sdk/erpc/common"

type VendorsRegistry struct {
	vendors []common.Vendor
}

func NewVendorsRegistry() *VendorsRegistry {
	r := &VendorsRegistry{}

	r.Register(CreateAlchemyVendor())
	r.Register(CreateDrpcVendor())
	r.Register(CreateInfuraVendor())
	r.Register(CreateQuicknodeVendor())

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
