package thirdparty

import "github.com/erpc/erpc/common"

type phonyUpstream struct {
	id string
	common.Upstream
}

var _ common.Upstream = (*phonyUpstream)(nil)

func (u *phonyUpstream) Id() string {
	return u.id
}

func (u *phonyUpstream) Name() string {
	return u.id
}

func (u *phonyUpstream) Type() common.UpstreamType {
	return common.UpstreamTypeEvm
}

func (u *phonyUpstream) Vendor() common.Vendor {
	return nil
}

func (u *phonyUpstream) VendorSettings() common.VendorSettings {
	return nil
}
