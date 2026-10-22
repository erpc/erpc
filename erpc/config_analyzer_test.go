package erpc

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

func TestGenerateValidationReport_WarnsOnFailoverWithoutFallbackTier(t *testing.T) {
	hasWarning := func(r *ValidationReport) bool {
		for _, w := range r.Warnings {
			if strings.Contains(w, "failover.onDefaultsExhausted") {
				return true
			}
		}
		return false
	}
	newCfg := func(tags []string) *common.Config {
		return &common.Config{
			Metrics: &common.MetricsConfig{},
			Projects: []*common.ProjectConfig{{
				Id: "main",
				Networks: []*common.NetworkConfig{{
					Architecture: common.ArchitectureEvm,
					Evm:          &common.EvmNetworkConfig{ChainId: 1},
					Failover:     &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)},
				}},
				Providers: []*common.ProviderConfig{{
					Id:        "p",
					Overrides: map[string]*common.UpstreamConfig{"*": {Tags: tags}},
				}},
			}},
		}
	}

	assert.True(t, hasWarning(GenerateValidationReport(context.Background(), newCfg(nil))))
	assert.False(t, hasWarning(GenerateValidationReport(context.Background(), newCfg([]string{common.TagTierFallback}))))
}
