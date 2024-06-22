package common

type Upstream interface {
	Config() *UpstreamConfig
	Vendor() Vendor
}
