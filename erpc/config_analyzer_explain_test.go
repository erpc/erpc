package erpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

func TestGenerateValidationReport_ExplainIncludesPolicyAndProfilePath(t *testing.T) {
	maxErrorRate := 0.6
	cfg := &common.Config{
		Metrics: &common.MetricsConfig{},
		Projects: []*common.ProjectConfig{
			{
				Id: "main",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 1,
						},
						SelectionPolicy: &common.SelectionPolicyConfig{
							EvalInterval: common.Duration(1 * time.Minute),
							Rules: []*common.SelectionPolicyRuleConfig{
								{
									MaxErrorRate: &maxErrorRate,
									Action:       common.SelectionPolicyRuleActionInclude,
								},
							},
						},
						Methods: &common.MethodsConfig{
							Profiles: map[string]*common.MethodWorkloadProfileConfig{
								"reads": {
									Cache: &common.MethodCacheProfileConfig{
										Finalized: util.BoolPtr(true),
									},
									Multiplex: &common.MethodMultiplexProfileConfig{
										Enabled: util.BoolPtr(true),
									},
								},
							},
							Definitions: map[string]*common.CacheMethodConfig{
								"eth_call": {
									Profile: "reads",
								},
							},
						},
					},
				},
			},
		},
	}

	report := GenerateValidationReport(context.Background(), cfg)
	assert.NotEmpty(t, report.Explain)

	joined := strings.Join(report.Explain, "\n")
	assert.Contains(t, joined, "selectionPolicy.path=selectionPolicy.rules")
	assert.Contains(t, joined, "profile.path=methods.definitions.eth_call.profile->methods.profiles.reads")
	assert.Contains(t, joined, "components=cache,multiplex")
}
