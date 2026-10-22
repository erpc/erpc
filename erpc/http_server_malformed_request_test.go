package erpc

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// Scanner traffic seen on the public edge: a real method name with an
// injection probe glued on.
var scannerMethods = []string{
	`eth_call-1 waitfor delay '0:0:15' --`,
	`eth_call0"XOR(if(now()=sysdate(),sleep(15),0))XOR"Z`,
	`eth_call0QIdoFZC') OR 157=(SELECT 157 FROM PG_SLEEP(15))--`,
	`eth_call1abfiViN'; waitfor delay '0:0:15' --`,
	`<script>alert(1)</script>`,
}

// No gock mock is registered, so a request that escaped the edge would show up
// as an unmatched upstream call.
func TestHttpServer_MalformedRequestsRejectedAtEdge(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(minimalServerConfig(), t)
	defer shutdown()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"sql probe appended to method", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, scannerMethods[2]), "method must be 1-128 characters"},
		{"waitfor probe", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, scannerMethods[0]), "method must be 1-128 characters"},
		{"script tag", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, scannerMethods[4]), "method must be 1-128 characters"},
		{"method over length ceiling", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, strings.Repeat("a", common.MaxMethodNameLength+1)), "method must be 1-128 characters"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statusCode, _, body := sendRequest(tc.body, nil, nil)
			require.Equal(t, http.StatusBadRequest, statusCode, "body=%s", body)
			require.Contains(t, body, tc.want)
		})
	}

	require.False(t, gock.HasUnmatchedRequest(),
		"a malformed request escaped the edge and hit an upstream: %v", gock.GetUnmatchedRequests())
}

// The method is the `category` label on the request metrics, so a payload that
// reached them would leave a series behind.
func TestHttpServer_MalformedMethodMintsNoMetricSeries(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(minimalServerConfig(), t)
	defer shutdown()

	for _, m := range scannerMethods {
		statusCode, _, body := sendRequest(
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, m), nil, nil,
		)
		require.Equal(t, http.StatusBadRequest, statusCode, "method=%q body=%s", m, body)
	}

	// Gather() may report a partial failure when a sibling test in this package
	// has swapped the default registerer mid-run; it still returns every family
	// it could collect, which is all this assertion needs.
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Logf("partial gather (%d families collected): %v", len(families), err)
	}
	require.NotEmpty(t, families, "gather returned nothing to inspect")

	// Match the payloads exactly. A substring probe would also flag legitimate
	// config-derived label values that happen to embed a method name, e.g. the
	// `policy` label on the cache metrics:
	// `policy(network=evm:123 method=eth_call finality=finalized)` — that set is
	// enumerable from config and therefore bounded, unlike a request-supplied one.
	for _, fam := range families {
		for _, metric := range fam.GetMetric() {
			for _, lbl := range metric.GetLabel() {
				if slices.Contains(scannerMethods, lbl.GetValue()) {
					t.Errorf("metric %s label %s=%q retains a scanner payload",
						fam.GetName(), lbl.GetName(), lbl.GetValue())
				}
			}
		}
	}
}

// Each accepted network id gets its own BootstrapTask and lazily-built
// NetworkConfig, so a non-canonical spelling of chain 123 must be rejected
// before GetNetwork creates either.
func TestHttpServer_NonCanonicalChainIdCreatesNoRegistryState(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(minimalServerConfig(), t)
	defer shutdown()

	project, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)

	tasksBefore := len(project.networksRegistry.initializer.Status().Tasks)
	networksBefore := len(project.Config.Networks)

	body := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	for _, chainId := range []string{"0123", "00123", "-123", "+123"} {
		statusCode, _, respBody := sendRequest(body, nil, map[string]string{"chainId": chainId})
		require.Equal(t, http.StatusBadRequest, statusCode, "chainId=%s body=%s", chainId, respBody)
		require.Contains(t, respBody, "invalid network id format")
	}

	require.Equal(t, tasksBefore, len(project.networksRegistry.initializer.Status().Tasks))
	require.Equal(t, networksBefore, len(project.Config.Networks))
}
