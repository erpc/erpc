package thirdparty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func endpointPayload(chainID string, endpoints []string) []byte {
	raw := map[string]chainData{
		chainID: {Endpoints: endpoints},
	}
	b, _ := json.Marshal(raw)
	return b
}

func TestRepositoryVendor_FallbackOnColdStart(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	fallbackPayload := endpointPayload("1", []string{"https://fallback-rpc.example.com"})
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(fallbackPayload)
	}))
	defer fallback.Close()

	v := &RepositoryVendor{
		remoteData:              make(map[string]map[int64][]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}

	// primary URL points nowhere — simulates cold-start primary failure
	settings := common.VendorSettings{
		"repositoryUrl":         "http://127.0.0.1:1",
		"fallbackRepositoryUrl": fallback.URL,
	}

	upstream := &common.UpstreamConfig{
		Id:  "test",
		Evm: &common.EvmUpstreamConfig{ChainId: 1},
	}

	cfgs, err := v.GenerateConfigs(ctx, &logger, upstream, settings)
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "https://fallback-rpc.example.com", cfgs[0].Endpoint)
}

func TestRepositoryVendor_FallbackNotTriggeredWhenCacheWarm(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	fallbackCalled := false
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Write(endpointPayload("1", []string{"https://fallback-rpc.example.com"}))
	}))
	defer fallback.Close()

	v := &RepositoryVendor{
		remoteData: map[string]map[int64][]string{
			"http://127.0.0.1:1": {1: {"https://primary-rpc.example.com"}},
		},
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}
	// lastFetchedAt not set → will attempt primary, which fails, but cache already exists so fallback must not run

	settings := common.VendorSettings{
		"repositoryUrl":         "http://127.0.0.1:1",
		"fallbackRepositoryUrl": fallback.URL,
	}

	upstream := &common.UpstreamConfig{
		Id:  "test",
		Evm: &common.EvmUpstreamConfig{ChainId: 1},
	}

	cfgs, err := v.GenerateConfigs(ctx, &logger, upstream, settings)
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "https://primary-rpc.example.com", cfgs[0].Endpoint)
	assert.False(t, fallbackCalled, "fallback must not be called when stale cache exists")
}

func TestRepositoryVendor_FallbackAlsoFailsReturnsNoError(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	v := &RepositoryVendor{
		remoteData:              make(map[string]map[int64][]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}

	settings := common.VendorSettings{
		"repositoryUrl":         "http://127.0.0.1:1",
		"fallbackRepositoryUrl": "http://127.0.0.1:2",
	}

	upstream := &common.UpstreamConfig{
		Id:  "test",
		Evm: &common.EvmUpstreamConfig{ChainId: 1},
	}

	// both fail: GenerateConfigs should return an error about chain not found, not a fetch error
	_, err := v.GenerateConfigs(ctx, &logger, upstream, settings)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in remote data")
}

func TestRepositoryVendor_FallbackNotUsedWhenPrimarySucceeds(t *testing.T) {
	logger := zerolog.Nop()
	ctx := context.Background()

	primaryPayload := endpointPayload("1", []string{"https://primary-rpc.example.com"})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(primaryPayload)
	}))
	defer primary.Close()

	fallbackCalled := false
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Write(endpointPayload("1", []string{"https://fallback-rpc.example.com"}))
	}))
	defer fallback.Close()

	v := &RepositoryVendor{
		remoteData:              make(map[string]map[int64][]string),
		remoteDataLastFetchedAt: make(map[string]time.Time),
	}

	settings := common.VendorSettings{
		"repositoryUrl":         primary.URL,
		"fallbackRepositoryUrl": fallback.URL,
	}

	upstream := &common.UpstreamConfig{
		Id:  "test",
		Evm: &common.EvmUpstreamConfig{ChainId: 1},
	}

	cfgs, err := v.GenerateConfigs(ctx, &logger, upstream, settings)
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "https://primary-rpc.example.com", cfgs[0].Endpoint)
	assert.False(t, fallbackCalled, "fallback must not be called when primary succeeds")
}
