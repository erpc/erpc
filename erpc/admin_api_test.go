package erpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const adminApiSecret = "admin-api-secret"

// adminApiFixture runs a full eRPC HTTP server whose project "test_project"
// authenticates consumers through a Redis-backed ("keys") and a memory-backed
// ("memkeys") database strategy, next to "open_project" without any auth.
type adminApiFixture struct {
	baseURL string
	redis   *miniredis.Miniredis
}

func newAdminApiFixture(t *testing.T, admin *common.AdminConfig) *adminApiFixture {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	redis := miniredis.RunT(t)
	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(5 * time.Second).Ptr()},
		Admin:  admin,
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{{
					Architecture: common.ArchitectureEvm,
					Alias:        "testnet",
					Evm:          &common.EvmNetworkConfig{ChainId: 123},
				}},
				Upstreams: []*common.UpstreamConfig{{
					Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost",
					Evm: &common.EvmUpstreamConfig{ChainId: 123},
				}},
				Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
					{Type: common.AuthTypeDatabase, Database: &common.DatabaseStrategyConfig{
						Connector: &common.ConnectorConfig{Id: "keys", Driver: common.DriverRedis, Redis: &common.RedisConnectorConfig{Addr: redis.Addr()}},
					}},
					{Type: common.AuthTypeDatabase, Database: &common.DatabaseStrategyConfig{
						Connector: &common.ConnectorConfig{Id: "memkeys", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"}},
					}},
				}},
			},
			{
				Id: "open_project",
				Networks: []*common.NetworkConfig{{
					Architecture: common.ArchitectureEvm,
					Evm:          &common.EvmNetworkConfig{ChainId: 123},
				}},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
	_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
	t.Cleanup(shutdown)
	return &adminApiFixture{baseURL: baseURL, redis: redis}
}

func adminSecretConfig() *common.AdminConfig {
	return &common.AdminConfig{Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
		{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: adminApiSecret}},
	}}}
}

// call posts one admin JSON-RPC request and returns the HTTP status with the
// decoded envelope.
func (f *adminApiFixture) call(t *testing.T, secret, method, params string) (int, map[string]interface{}) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q,"params":%s}`, method, params)
	req, err := http.NewRequest(http.MethodPost, f.baseURL+"/admin", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-ERPC-Secret-Token", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

func (f *adminApiFixture) result(t *testing.T, method, params string) map[string]interface{} {
	t.Helper()
	status, out := f.call(t, adminApiSecret, method, params)
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.NotContains(t, out, "error", "%v", out)
	assert.EqualValues(t, 7, out["id"])
	res, ok := out["result"].(map[string]interface{})
	require.True(t, ok, "result is an object: %v", out)
	return res
}

// errorMessage returns the JSON-RPC error message plus, for admin calls, the
// underlying cause the server exposes under error.data.
func (f *adminApiFixture) errorMessage(t *testing.T, wantStatus int, method, params string) string {
	t.Helper()
	status, out := f.call(t, adminApiSecret, method, params)
	require.Equal(t, wantStatus, status, "%v", out)
	e, ok := out["error"].(map[string]interface{})
	require.True(t, ok, "error expected: %v", out)
	msg, _ := e["message"].(string)
	if d, ok := e["data"]; ok {
		b, err := json.Marshal(d)
		require.NoError(t, err)
		msg += " | " + string(b)
	}
	return msg
}

func TestAdminApi_ReadOnlyMethods(t *testing.T) {
	f := newAdminApiFixture(t, adminSecretConfig())

	t.Run("config returns the loaded configuration", func(t *testing.T) {
		res := f.result(t, "erpc_config", `[]`)
		projects := res["projects"].([]interface{})
		require.Len(t, projects, 2)
		assert.Equal(t, "test_project", projects[0].(map[string]interface{})["id"])
	})

	t.Run("taxonomy lists projects, networks and upstreams", func(t *testing.T) {
		res := f.result(t, "erpc_taxonomy", `[]`)
		byId := map[string]map[string]interface{}{}
		for _, p := range res["projects"].([]interface{}) {
			pm := p.(map[string]interface{})
			byId[pm["id"].(string)] = pm
		}
		require.Contains(t, byId, "test_project")
		networks := byId["test_project"]["networks"].([]interface{})
		require.Len(t, networks, 1)
		nw := networks[0].(map[string]interface{})
		assert.Equal(t, "evm:123", nw["id"])
		assert.Equal(t, "testnet", nw["alias"])
		ups := nw["upstreams"].([]interface{})
		require.Len(t, ups, 1)
		assert.Equal(t, "rpc1", ups[0].(map[string]interface{})["id"])
	})

	t.Run("project returns config and upstream health", func(t *testing.T) {
		res := f.result(t, "erpc_project", `["test_project"]`)
		assert.Equal(t, "test_project", res["config"].(map[string]interface{})["id"])
		assert.Contains(t, res, "health")
	})

	cases := []struct {
		name, method, params string
		status               int
		msg                  string
	}{
		{"project without params", "erpc_project", `[]`, http.StatusBadRequest, "project id (params[0]) is required"},
		{"project id not a string", "erpc_project", `[42]`, http.StatusBadRequest, "project id (params[0]) must be a string"},
		{"unknown project", "erpc_project", `["nope"]`, http.StatusNotFound, "nope"},
		{"unknown admin method", "erpc_doesNotExist", `[]`, http.StatusOK, "admin method erpc_doesNotExist is not supported"},
		{"cordon without params", "erpc_cordonUpstream", `[]`, http.StatusOK, "cordon admin: params is required"},
		{"cordon without upstream", "erpc_cordonUpstream", `[{"projectId":"test_project"}]`, http.StatusOK, "cordon admin: projectId and upstream are required"},
		{"cordon with malformed params", "erpc_cordonUpstream", `[{"projectId":5}]`, http.StatusOK, "cordon admin: invalid params"},
		{"cordon unknown upstream", "erpc_cordonUpstream", `[{"projectId":"test_project","upstream":"ghost"}]`, http.StatusOK, `cordon admin: upstream "ghost" not found in project "test_project"`},
		{"cordon unknown project", "erpc_uncordonUpstream", `[{"projectId":"nope","upstream":"rpc1"}]`, http.StatusNotFound, "nope"},
		{"list cordoned without project", "erpc_listCordoned", `[]`, http.StatusOK, "cordon admin: projectId is required"},
		{"list cordoned unknown project", "erpc_listCordoned", `[{"projectId":"nope"}]`, http.StatusNotFound, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, f.errorMessage(t, tc.status, tc.method, tc.params), tc.msg)
		})
	}

	t.Run("wrong secret is unauthorized", func(t *testing.T) {
		status, out := f.call(t, "wrong", "erpc_config", `[]`)
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Contains(t, out, "error")
		assert.NotContains(t, out, "result")
	})
}

func TestAdminApi_ApiKeyLifecycle(t *testing.T) {
	f := newAdminApiFixture(t, adminSecretConfig())
	const keyParams = `"projectId":"test_project","connectorId":"keys"`

	// The Redis connector initializes asynchronously; the first successful
	// write proves it is ready.
	require.Eventually(t, func() bool {
		_, out := f.call(t, adminApiSecret, "erpc_addApiKey",
			`[{`+keyParams+`,"apiKey":"key-1","userId":"alice","rateLimitBudget":"gold"}]`)
		return out["error"] == nil
	}, 5*time.Second, 50*time.Millisecond)

	res := f.result(t, "erpc_addApiKey", `[{`+keyParams+`,"apiKey":"key-2","userId":"bob","enabled":false}]`)
	assert.Equal(t, map[string]interface{}{"success": true, "apiKey": "key-2", "userId": "bob"}, res)

	stored, err := f.redis.Get("key-1:alice")
	require.NoError(t, err)
	assert.JSONEq(t, `{"userId":"alice","enabled":true,"rateLimitBudget":"gold"}`, stored)

	listed := f.result(t, "erpc_listApiKeys", `[{`+keyParams+`,"limit":100,"paginationToken":"0"}]`)
	assert.Equal(t, false, listed["hasMore"])
	assert.EqualValues(t, 2, listed["totalReturned"])
	keys := map[string]map[string]interface{}{}
	for _, k := range listed["apiKeys"].([]interface{}) {
		km := k.(map[string]interface{})
		keys[km["key"].(string)] = km
	}
	require.Contains(t, keys, "key-1")
	require.Contains(t, keys, "key-2")
	assert.Equal(t, "alice", keys["key-1"]["userId"])
	assert.Equal(t, true, keys["key-1"]["enabled"])
	assert.Equal(t, "gold", keys["key-1"]["rateLimitBudget"])
	assert.Equal(t, false, keys["key-2"]["enabled"])

	// Records that are not JSON are skipped by the listing.
	f.redis.Set("broken:entry", "not-json")
	listed = f.result(t, "erpc_listApiKeys", `[{`+keyParams+`}]`)
	assert.EqualValues(t, 2, listed["totalReturned"])

	// Update and delete read the current record through the connector's
	// wildcard lookup (apiKey, "*"); seed that record as the connector
	// returns it.
	f.redis.Set("key-3:*", `{"userId":"carol","enabled":true,"rateLimitBudget":"silver"}`)
	res = f.result(t, "erpc_updateApiKey", `[{`+keyParams+`,"apiKey":"key-3","updates":{"enabled":false,"rateLimitBudget":null,"tier":"pro"}}]`)
	assert.Equal(t, true, res["success"])
	assert.Equal(t, "key-3", res["apiKey"])
	updated, err := f.redis.Get("key-3:carol")
	require.NoError(t, err)
	assert.JSONEq(t, `{"userId":"carol","enabled":false,"tier":"pro"}`, updated, "null removes a field, values overwrite")

	res = f.result(t, "erpc_deleteApiKey", `[{`+keyParams+`,"apiKey":"key-3"}]`)
	assert.Equal(t, map[string]interface{}{"success": true, "apiKey": "key-3", "userId": "carol"}, res)
	assert.False(t, f.redis.Exists("key-3:carol"))

	f.redis.Set("key-4:*", `{"enabled":true}`)
	f.redis.Set("key-5:*", `not-json`)
	for _, method := range []string{"erpc_updateApiKey", "erpc_deleteApiKey"} {
		extra := ""
		if method == "erpc_updateApiKey" {
			extra = `,"updates":{}`
		}
		assert.Contains(t, f.errorMessage(t, http.StatusOK, method, `[{`+keyParams+`,"apiKey":"key-4"`+extra+`}]`), "missing or invalid userId in current data", method)
		assert.Contains(t, f.errorMessage(t, http.StatusOK, method, `[{`+keyParams+`,"apiKey":"key-5"`+extra+`}]`), "failed to parse current data", method)
		assert.Contains(t, f.errorMessage(t, http.StatusOK, method, `[{`+keyParams+`,"apiKey":"absent"`+extra+`}]`), "failed to get current API key data", method)
	}

	assert.Contains(t,
		f.errorMessage(t, http.StatusOK, "erpc_listApiKeys", `[{"projectId":"test_project","connectorId":"memkeys"}]`),
		"failed to list API keys")
}

func TestAdminApi_ApiKeyParamValidation(t *testing.T) {
	f := newAdminApiFixture(t, adminSecretConfig())

	type tc struct {
		name, params string
		status       int
		msg          string
	}
	common4 := func(method string) []tc {
		return []tc{
			{"no params", `[]`, http.StatusBadRequest, "requires params"},
			{"param not an object", `["x"]`, http.StatusBadRequest, "first parameter must be an object"},
			{"missing projectId", `[{"connectorId":"keys"}]`, http.StatusBadRequest, "projectId is required and must be a string"},
			{"missing connectorId", `[{"projectId":"test_project"}]`, http.StatusBadRequest, "connectorId is required and must be a string"},
		}
	}
	cases := map[string][]tc{
		"erpc_addApiKey": append(common4("add"),
			tc{"missing apiKey", `[{"projectId":"test_project","connectorId":"keys"}]`, http.StatusBadRequest, "apiKey is required and must be a string"},
			tc{"missing userId", `[{"projectId":"test_project","connectorId":"keys","apiKey":"k"}]`, http.StatusBadRequest, "userId is required and must be a string"},
			tc{"budget not a string", `[{"projectId":"test_project","connectorId":"keys","apiKey":"k","userId":"u","rateLimitBudget":1}]`, http.StatusBadRequest, "rateLimitBudget must be a string"},
			tc{"enabled not a bool", `[{"projectId":"test_project","connectorId":"keys","apiKey":"k","userId":"u","enabled":"yes"}]`, http.StatusBadRequest, "enabled must be a boolean"},
			tc{"unknown project", `[{"projectId":"nope","connectorId":"keys","apiKey":"k","userId":"u"}]`, http.StatusOK, "failed to find connector: project 'nope' not found"},
			tc{"project without auth", `[{"projectId":"open_project","connectorId":"keys","apiKey":"k","userId":"u"}]`, http.StatusOK, "project 'open_project' has no auth registry"},
			tc{"unknown connector", `[{"projectId":"test_project","connectorId":"ghost","apiKey":"k","userId":"u"}]`, http.StatusOK, "database connector with ID 'ghost' not found"},
		),
		"erpc_listApiKeys": append(common4("list"),
			tc{"unknown connector", `[{"projectId":"test_project","connectorId":"ghost"}]`, http.StatusOK, "database connector with ID 'ghost' not found"},
		),
		"erpc_updateApiKey": append(common4("update"),
			tc{"missing apiKey", `[{"projectId":"test_project","connectorId":"keys"}]`, http.StatusBadRequest, "apiKey is required and must be a string"},
			tc{"missing updates", `[{"projectId":"test_project","connectorId":"keys","apiKey":"k"}]`, http.StatusBadRequest, "updates is required and must be an object"},
			tc{"unknown connector", `[{"projectId":"test_project","connectorId":"ghost","apiKey":"k","updates":{}}]`, http.StatusOK, "database connector with ID 'ghost' not found"},
		),
		"erpc_deleteApiKey": append(common4("delete"),
			tc{"missing apiKey", `[{"projectId":"test_project","connectorId":"keys"}]`, http.StatusBadRequest, "apiKey is required and must be a string"},
			tc{"unknown connector", `[{"projectId":"test_project","connectorId":"ghost","apiKey":"k"}]`, http.StatusOK, "database connector with ID 'ghost' not found"},
		),
	}
	for method, list := range cases {
		for _, c := range list {
			t.Run(method+"/"+c.name, func(t *testing.T) {
				assert.Contains(t, f.errorMessage(t, c.status, method, c.params), c.msg)
			})
		}
	}
}

func TestAdminApi_MethodAllowAndDenyLists(t *testing.T) {
	admin := adminSecretConfig()
	admin.DenyMethods = []string{"erpc_*"}
	admin.AllowMethods = []string{"erpc_taxonomy"}
	f := newAdminApiFixture(t, admin)

	f.result(t, "erpc_taxonomy", `[]`)
	assert.Equal(t, "method not supported: erpc_config", f.errorMessage(t, http.StatusOK, "erpc_config", `[]`))
}

func TestAdminApi_DisabledAdminRejectsRequests(t *testing.T) {
	f := newAdminApiFixture(t, nil)
	status, out := f.call(t, adminApiSecret, "erpc_config", `[]`)
	assert.Contains(t, out, "error")
	assert.NotContains(t, out, "result", "no configuration is leaked without admin config")
	assert.Contains(t, fmt.Sprint(out["error"]), "admin auth not configured")
	assert.Equal(t, http.StatusOK, status)
}
