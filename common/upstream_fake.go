package common

import (
	"context"
	"sync"

	"github.com/rs/zerolog"
)

var _ EvmStatePoller = &FakeEvmStatePoller{}
var _ Upstream = &FakeUpstream{}
var _ EvmUpstream = &FakeUpstream{}

type FakeUpstream struct {
	id             string
	config         *UpstreamConfig
	network        Network
	evmStatePoller EvmStatePoller
	cordoned       bool
	cordonedReason string
	cordonMu       sync.RWMutex
}

func NewFakeUpstream(id string, opts ...func(*FakeUpstream)) Upstream {
	u := &FakeUpstream{
		id: id,
		config: &UpstreamConfig{
			Id: id,
		},
	}

	for _, opt := range opts {
		opt(u)
	}

	return u
}

func WithEvmStatePoller(evmStatePoller EvmStatePoller) func(*FakeUpstream) {
	return func(u *FakeUpstream) {
		u.evmStatePoller = evmStatePoller
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

func (u *FakeUpstream) IgnoreMethod(method string) {
	// No-op for testing
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

func (u *FakeUpstream) EvmIsBlockFinalized(ctx context.Context, blockNumber int64, forceFresh bool) (bool, error) {
	return false, nil
}

func (u *FakeUpstream) EvmStatePoller() EvmStatePoller {
	return u.evmStatePoller
}

func (u *FakeUpstream) Forward(ctx context.Context, nq *NormalizedRequest, skipSyncingCheck bool) (*NormalizedResponse, error) {
	return nil, nil
}

func (u *FakeUpstream) Cordon(method string, reason string) {
	u.cordonMu.Lock()
	defer u.cordonMu.Unlock()
	u.cordoned = true
	u.cordonedReason = reason
}

func (u *FakeUpstream) Uncordon(method string) {
	u.cordonMu.Lock()
	defer u.cordonMu.Unlock()
	u.cordoned = false
	u.cordonedReason = ""
}

func (u *FakeUpstream) CordonedReason() (string, bool) {
	u.cordonMu.RLock()
	defer u.cordonMu.RUnlock()
	return u.cordonedReason, u.cordoned
}

func (u *FakeUpstream) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error) {
	return true, nil
}

type FakeEvmStatePoller struct {
	latestBlockNumber    int64
	finalizedBlockNumber int64
}

func NewFakeEvmStatePoller(latestBlockNumber int64, finalizedBlockNumber int64) EvmStatePoller {
	return &FakeEvmStatePoller{
		latestBlockNumber:    latestBlockNumber,
		finalizedBlockNumber: finalizedBlockNumber,
	}
}

func (p *FakeEvmStatePoller) EvmBlockNumber(ctx context.Context) (int64, error) {
	return p.latestBlockNumber, nil
}

func (p *FakeEvmStatePoller) Bootstrap(ctx context.Context) error {
	return nil
}

func (p *FakeEvmStatePoller) FinalizedBlock() int64 {
	return p.finalizedBlockNumber
}

func (p *FakeEvmStatePoller) IsBlockFinalized(blockNumber int64) (bool, error) {
	return blockNumber <= p.finalizedBlockNumber, nil
}

func (p *FakeEvmStatePoller) IsObjectNull() bool {
	return false
}

func (p *FakeEvmStatePoller) LatestBlock() int64 {
	return p.latestBlockNumber
}

func (p *FakeEvmStatePoller) Poll(ctx context.Context) error {
	return nil
}

func (p *FakeEvmStatePoller) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return p.finalizedBlockNumber, nil
}

func (p *FakeEvmStatePoller) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	return p.latestBlockNumber, nil
}

func (p *FakeEvmStatePoller) SetNetworkConfig(config *EvmNetworkConfig) {
	// No-op for testing
}

func (p *FakeEvmStatePoller) SetSyncingState(state EvmSyncingState) {
	// No-op for testing
}

func (p *FakeEvmStatePoller) SuggestFinalizedBlock(blockNumber int64) {
	p.finalizedBlockNumber = blockNumber
}

func (p *FakeEvmStatePoller) SuggestLatestBlock(blockNumber int64) {
	p.latestBlockNumber = blockNumber
}

func (p *FakeEvmStatePoller) SyncingState() EvmSyncingState {
	return EvmSyncingStateUnknown
}
