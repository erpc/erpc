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

// Config must be overridden: the embedded common.Upstream is nil, so the
// promoted Config() would panic when callers (e.g. the EVM error extractor)
// read upstream.Config().Type during a SupportsNetwork probe.
func (u *phonyUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: u.id, Type: common.UpstreamTypeEvm}
}

func (u *phonyUpstream) Vendor() common.Vendor {
	return nil
}

func (u *phonyUpstream) VendorSettings() common.VendorSettings {
	return nil
}
