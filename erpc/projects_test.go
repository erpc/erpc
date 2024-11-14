package erpc

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/erpc/erpc/vendors"
	"github.com/rs/zerolog/log"
)

func TestProject_Forward(t *testing.T) {
	t.Run("ForwardCorrectlyRateLimitedOnProjectLevel", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
			&common.RateLimiterConfig{
				Budgets: []*common.RateLimitBudgetConfig{
					{
						Id: "MyLimiterBudget_Test1",
						Rules: []*common.RateLimitRuleConfig{
							{
								Method:   "*",
								MaxCount: 3,
								Period:   "60s",
								WaitTime: "",
							},
						},
					},
				},
			},
			&log.Logger,
		)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		prjReg, err := NewProjectsRegistry(
			ctx,
			&log.Logger,
			[]*common.ProjectConfig{
				{
					Id:              "prjA",
					RateLimitBudget: "MyLimiterBudget_Test1",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
			nil,
			rateLimitersRegistry,
			vendors.NewVendorsRegistry(),
		)
		if err != nil {
			t.Fatal(err)
		}

		prj, err := prjReg.GetProject("prjA")
		if err != nil {
			t.Fatal(err)
		}

		var lastErr error
		var lastResp *common.NormalizedResponse

		for i := 0; i < 5; i++ {
			fakeReq := common.NewNormalizedRequest([]byte(`{"method": "eth_chainId","params":[]}`))
			lastResp, lastErr = prj.Forward(ctx, "evm:123", fakeReq)
		}

		var e *common.ErrProjectRateLimitRuleExceeded
		if lastErr == nil || !errors.As(lastErr, &e) {
			t.Errorf("Expected %v, got %v", "ErrProjectRateLimitRuleExceeded", lastErr)
		}

		log.Logger.Info().Msgf("Last Resp: %+v", lastResp)
	})
}
