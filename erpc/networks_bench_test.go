package erpc

import (
    "context"
    "net/http"
    "testing"
    "time"

    "github.com/erpc/erpc/common"
    "github.com/erpc/erpc/health"
    "github.com/erpc/erpc/upstream"
    "github.com/erpc/erpc/util"
    "github.com/erpc/erpc/vendors"
    "github.com/h2non/gock"
    "github.com/rs/zerolog/log"
)

// BenchmarkNetworkForward_SimpleSuccess sets up a single upstream that always replies 200,
// and measures performance of forwarding under normal/happy path conditions.
func BenchmarkNetworkForward_SimpleSuccess(b *testing.B) {
    util.ConfigureTestLogger()
    util.ResetGock()
    defer util.ResetGock()

    // Mock upstream always succeeds quickly
    gock.New("http://rpc-success.localhost").
		Persist().
        Post("").
        Reply(200).
        JSON([]byte(`{"result": "0x1"}`))

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
        Budgets: []*common.RateLimitBudgetConfig{},
    }, &log.Logger)
    if err != nil {
        b.Fatal(err)
    }

    mt := health.NewTracker("benchProject", 2*time.Second)
    upConfig := &common.UpstreamConfig{
        Type:     common.UpstreamTypeEvm,
        Id:       "upstream_success",
        Endpoint: "http://rpc-success.localhost",
        Evm: &common.EvmUpstreamConfig{
            ChainId: 123,
        },
    }

    upsReg := upstream.NewUpstreamsRegistry(
        ctx,
        &log.Logger,
        "benchProject",
        []*common.UpstreamConfig{upConfig},
        rlr,
        vendors.NewVendorsRegistry(),
        mt,
        1*time.Second,
    )

    if err := upsReg.Bootstrap(ctx); err != nil {
        b.Fatal(err)
    }
    if err := upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)); err != nil {
        b.Fatal(err)
    }

    ntw, err := NewNetwork(
        &log.Logger,
        "benchProject",
        &common.NetworkConfig{
            Architecture: common.ArchitectureEvm,
            Evm: &common.EvmNetworkConfig{
                ChainId: 123,
            },
        },
        rlr,
        upsReg,
        mt,
    )
    if err != nil {
        b.Fatal(err)
    }

    // Prepare a typical request
    requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
    b.ResetTimer()

    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            fakeReq := common.NewNormalizedRequest(requestBytes)
            _, forwardErr := ntw.Forward(ctx, fakeReq)
            if forwardErr != nil {
                b.Errorf("Expected nil error, got %v", forwardErr)
            }
        }
    })
}

// BenchmarkNetworkForward_MethodIgnoreCase simulates a scenario where an upstream
// does not support a method (returns an error code), tested under repeated load.
// We expect the Network to mark the method as ignored and skip subsequent calls.
func BenchmarkNetworkForward_MethodIgnoreCase(b *testing.B) {
    util.ConfigureTestLogger()
    util.ResetGock()
    defer util.ResetGock()

    // Upstream replies with a "method not supported" error code for the tested method
    gock.New("http://rpc-unsupported.localhost").
		Persist().
        Post("").
        Reply(http.StatusNotFound).
        JSON([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not supported"}}`))

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
        Budgets: []*common.RateLimitBudgetConfig{},
    }, &log.Logger)
    if err != nil {
        b.Fatal(err)
    }

    mt := health.NewTracker("benchProject", 2*time.Second)
    upConfig := &common.UpstreamConfig{
        Type:     common.UpstreamTypeEvm,
        Id:       "upstream_unsupported",
        Endpoint: "http://rpc-unsupported.localhost",
        Evm: &common.EvmUpstreamConfig{
            ChainId: 123,
        },
    }

    upsReg := upstream.NewUpstreamsRegistry(
        ctx,
        &log.Logger,
        "benchProject",
        []*common.UpstreamConfig{upConfig},
        rlr,
        vendors.NewVendorsRegistry(),
        mt,
        1*time.Second,
    )

    if err := upsReg.Bootstrap(ctx); err != nil {
        b.Fatal(err)
    }
    if err := upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)); err != nil {
        b.Fatal(err)
    }

    ntw, err := NewNetwork(
        &log.Logger,
        "benchProject",
        &common.NetworkConfig{
            Architecture: common.ArchitectureEvm,
            Evm: &common.EvmNetworkConfig{
                ChainId: 123,
            },
        },
        rlr,
        upsReg,
        mt,
    )
    if err != nil {
        b.Fatal(err)
    }

    // We use a method that triggers -32601 from the upstream
    requestBytes := []byte(`{"jsonrpc":"2.0","id":999,"method":"eth_traceTransaction","params":["0xdeadbeef"]}`)
    b.ResetTimer()

    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            fakeReq := common.NewNormalizedRequest(requestBytes)
            _, forwardErr := ntw.Forward(ctx, fakeReq)
            // We expect an error at first, then the method is flagged as ignored:
            // subsequent calls may skip the upstream quickly.
            // Not checking success/failure here, since we are just benchmarking behavior.
            _ = forwardErr
        }
    })
}

// BenchmarkNetworkForward_RetryFailures tests a scenario where the upstream
// fails intermittently, triggering failsafe retries. This measures how quickly
// we handle repeated failures within the failsafe logic.
func BenchmarkNetworkForward_RetryFailures(b *testing.B) {
    util.ConfigureTestLogger()
    util.ResetGock()
    defer util.ResetGock()

    // Upstream sometimes returning 503
    gock.New("http://rpc-flaky.localhost").
        Post("").
        Persist().
        Reply(503).
        JSON([]byte(`{"error":{"message":"some random provider issue"}}`))

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    fsCfg := &common.FailsafeConfig{
        Retry: &common.RetryPolicyConfig{
            MaxAttempts: 3,
        },
    }
    rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
        Budgets: []*common.RateLimitBudgetConfig{},
    }, &log.Logger)
    if err != nil {
        b.Fatal(err)
    }

    mt := health.NewTracker("benchProject", 2*time.Second)
    upConfig := &common.UpstreamConfig{
        Type:     common.UpstreamTypeEvm,
        Id:       "flaky_upstream",
        Endpoint: "http://rpc-flaky.localhost",
        Evm: &common.EvmUpstreamConfig{
            ChainId: 123,
        },
        Failsafe: fsCfg, // ensures we do internal upstream-level retries
    }

    upsReg := upstream.NewUpstreamsRegistry(
        ctx,
        &log.Logger,
        "benchProject",
        []*common.UpstreamConfig{upConfig},
        rlr,
        vendors.NewVendorsRegistry(),
        mt,
        1*time.Second,
    )
    if err := upsReg.Bootstrap(ctx); err != nil {
        b.Fatal(err)
    }
    if err := upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)); err != nil {
        b.Fatal(err)
    }

    ntw, err := NewNetwork(
        &log.Logger,
        "benchProject",
        &common.NetworkConfig{
            Architecture: common.ArchitectureEvm,
            Evm: &common.EvmNetworkConfig{
                ChainId: 123,
            },
            Failsafe: fsCfg, // ensures we do network-level failsafe
        },
        rlr,
        upsReg,
        mt,
    )
    if err != nil {
        b.Fatal(err)
    }

    // Request which triggers 503
    requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0xflaky"]}`)

    b.ResetTimer()
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            fakeReq := common.NewNormalizedRequest(requestBytes)
            _, _ = ntw.Forward(ctx, fakeReq)
            // We’re mainly measuring how quickly we process repeated fails
        }
    })
}