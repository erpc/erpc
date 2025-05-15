package common

import (
	"context"

	"github.com/rs/zerolog"
)

type FakeUpstream struct {
	id      string
	config  *UpstreamConfig
	network Network
}

func NewFakeUpstream(id string) Upstream {
	return &FakeUpstream{
		id: id,
		config: &UpstreamConfig{
			Id: id,
		},
	}
}

func (u *FakeUpstream) Id() string {
	return u.id
}

func (u *FakeUpstream) VendorName() string {
	return u.config.VendorName
}

func (u *FakeUpstream) Config() *UpstreamConfig {
	return u.config
}

func (u *FakeUpstream) Logger() *zerolog.Logger {
	return &zerolog.Logger{}
}

func (u *FakeUpstream) EvmGetChainId(context.Context) (string, error) {
	return "123", nil
}

func (u *FakeUpstream) NetworkId() string {
	return "evm:123"
}

func (u *FakeUpstream) SetNetwork(network Network) {
	u.network = network
}

func (u *FakeUpstream) Network() Network {
	return u.network
}

func (u *FakeUpstream) EvmSyncingState() EvmSyncingState {
	return EvmSyncingStateUnknown
}

func (u *FakeUpstream) Vendor() Vendor {
	return nil
}

func (u *FakeUpstream) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
	return true, nil
}

func (u *FakeUpstream) EvmIsBlockFinalized(blockNumber int64) (bool, error) {
	return false, nil
}

func (u *FakeUpstream) EvmStatePoller() EvmStatePoller {
	return nil
}

func (u *FakeUpstream) Forward(ctx context.Context, nq *NormalizedRequest, skipSyncingCheck bool) (*NormalizedResponse, error) {
	return nil, nil
}
