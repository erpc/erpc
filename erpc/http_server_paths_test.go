package erpc

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const httpPathsAdminSecret = "paths-admin-secret"

// httpPathsProject is a minimal EVM project served by the mocked rpc1 upstream.
func httpPathsProject(id string) *common.ProjectConfig {
	return &common.ProjectConfig{
		Id: id,
		Networks: []*common.NetworkConfig{{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Alias:        "main",
		}},
		Upstreams: []*common.UpstreamConfig{{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}},
	}
}

// httpPathsStart builds a full eRPC + HttpServer from cfg and serves the
// server's real (gzip + timeout wrapped) handler over httptest.
func httpPathsStart(t *testing.T, cfg *common.Config) (*HttpServer, string) {
	t.Helper()
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.Server == nil {
		cfg.Server = &common.ServerConfig{}
	}
	cfg.Server.WaitBeforeShutdown = common.Duration(0).Ptr()
	if cfg.RateLimiters == nil {
		cfg.RateLimiters = &common.RateLimiterConfig{}
	}
	require.NoError(t, cfg.SetDefaults(nil))
	cfg.Server.ListenV4 = util.BoolPtr(true)

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	instance, err := NewERPC(ctx, &logger, ssr, nil, nil, cfg)
	require.NoError(t, err)
	instance.Bootstrap(ctx)

	srv, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, cfg.Indexer, instance)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.serverV4.Handler)
	t.Cleanup(func() {
		ts.Close()
		cancel()
	})
	return srv, ts.URL
}

func httpPathsDo(t *testing.T, method, rawURL string, body []byte, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(b)
}

func httpPathsErrCode(t *testing.T, body string) (int, string) {
	t.Helper()
	var out struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body: %s", body)
	return out.Error.Code, out.Error.Message
}

func httpPathsMockBalance() {
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBalance") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x2a"}`))
}

const httpPathsBalanceBody = `{"jsonrpc":"2.0","id":7,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`

func TestHttpPaths_MainServerRouting(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	httpPathsMockBalance()

	prj := httpPathsProject("test_project")
	prj.CORS = &common.CORSConfig{
		AllowedOrigins:   []string{"(", "https://ok.example"},
		AllowedMethods:   []string{"POST", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type"},
		ExposedHeaders:   []string{"X-ERPC-Version"},
		AllowCredentials: util.BoolPtr(true),
		MaxAge:           600,
	}
	prj.IgnoreMethods = []string{"eth_sign*"}
	prj.ForwardHeaders = []string{"X-Fwd-Token"}
	prj.TrustUserIdHeader = true

	authPrj := httpPathsProject("auth_project")
	authPrj.Auth = &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
		{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: "p-secret"}},
	}}

	cfg := &common.Config{
		Server: &common.ServerConfig{
			Aliasing: &common.AliasingConfig{Rules: []*common.AliasingRuleConfig{
				{MatchDomain: "(", ServeProject: "nope"},
				{MatchDomain: "*.alias.test", ServeProject: "test_project", ServeArchitecture: "evm"},
			}},
		},
		Admin: &common.AdminConfig{
			Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: httpPathsAdminSecret}},
			}},
			CORS:        &common.CORSConfig{AllowedOrigins: []string{"https://admin.example"}, AllowedMethods: []string{"POST"}},
			DenyMethods: []string{"erpc_taxonomy"},
		},
		Projects: []*common.ProjectConfig{prj, authPrj},
	}
	_, base := httpPathsStart(t, cfg)
	post := func(path, body string, hdr map[string]string) (int, http.Header, string) {
		return httpPathsDo(t, http.MethodPost, base+path, []byte(body), hdr)
	}

	t.Run("UrlPathErrorsAre400WithInvalidArgument", func(t *testing.T) {
		cases := []struct {
			path string
			msg  string
		}{
			{"/", "project is required"},
			{"/a/b/c/d", "must only provide"},
			{"/test_project/btc/1", "architecture is not valid"},
		}
		for _, tc := range cases {
			status, _, body := post(tc.path, httpPathsBalanceBody, nil)
			assert.Equal(t, http.StatusBadRequest, status, tc.path)
			code, _ := httpPathsErrCode(t, body)
			assert.Equal(t, int(common.JsonRpcErrorInvalidArgument), code, tc.path)
			assert.Contains(t, body, tc.msg, tc.path)
		}
	})

	t.Run("UnknownProjectIs404", func(t *testing.T) {
		status, _, body := post("/no_such_project/evm/123", httpPathsBalanceBody, nil)
		assert.Equal(t, http.StatusNotFound, status)
		assert.Contains(t, body, "no_such_project")
	})

	t.Run("ProjectOnlyPathNeedsNetwork", func(t *testing.T) {
		status, _, body := post("/test_project", httpPathsBalanceBody, nil)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body, "architecture and chain must be provided")
	})

	t.Run("NetworkFromBodyAndAliasAndDomainRouting", func(t *testing.T) {
		bodyWithNet := `{"jsonrpc":"2.0","id":7,"networkId":"evm:123","method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`
		for name, call := range map[string]func() (int, http.Header, string){
			"body":   func() (int, http.Header, string) { return post("/test_project", bodyWithNet, nil) },
			"alias":  func() (int, http.Header, string) { return post("/test_project/main", httpPathsBalanceBody, nil) },
			"domain": func() (int, http.Header, string) { return post("/123", httpPathsBalanceBody, map[string]string{"Host": "rpc.alias.test:8080"}) },
		} {
			status, hdr, body := call()
			assert.Equal(t, http.StatusOK, status, name)
			assert.Contains(t, body, `"result":"0x2a"`, name)
			assert.Equal(t, "MISS", hdr.Get("X-ERPC-Cache"), name)
		}
	})

	t.Run("IgnoredMethodIsUnsupported", func(t *testing.T) {
		status, _, body := post("/test_project/evm/123", `{"jsonrpc":"2.0","id":3,"method":"eth_signTypedData","params":[]}`, nil)
		assert.Equal(t, http.StatusOK, status)
		code, msg := httpPathsErrCode(t, body)
		assert.Equal(t, int(common.JsonRpcErrorUnsupportedException), code)
		assert.Equal(t, "method not supported: eth_signTypedData", msg)
		assert.Contains(t, body, `"id":3`)
	})

	t.Run("ForwardHeadersReachUpstream", func(t *testing.T) {
		gock.New("http://rpc1.localhost").
			Post("").
			MatchHeader("X-Fwd-Token", "tok-1").
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getCode") }).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xfeed"}`))
		status, _, body := post("/test_project/evm/123",
			`{"jsonrpc":"2.0","id":1,"method":"eth_getCode","params":["0x0000000000000000000000000000000000000002","latest"]}`,
			map[string]string{"X-Fwd-Token": "tok-1", common.HeaderUserId: "gateway-user"})
		assert.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, `"result":"0xfeed"`)
	})

	t.Run("GzipRequestBodies", func(t *testing.T) {
		status, _, body := post("/test_project/evm/123", "not-gzip", map[string]string{"Content-Encoding": "gzip"})
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body, "invalid gzip body")

		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(httpPathsBalanceBody))
		require.NoError(t, zw.Close())
		status, _, body = httpPathsDo(t, http.MethodPost, base+"/test_project/evm/123", buf.Bytes(), map[string]string{"Content-Encoding": "gzip"})
		assert.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, `"result":"0x2a"`)
	})

	t.Run("BatchBodies", func(t *testing.T) {
		status, _, body := post("/test_project/evm/123", `[{"jsonrpc":"2.0",`, nil)
		assert.Equal(t, http.StatusBadRequest, status)
		code, _ := httpPathsErrCode(t, body)
		assert.Equal(t, int(common.JsonRpcErrorParseException), code)

		status, _, body = post("/test_project/evm/123", `[]`, nil)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "[]", strings.TrimSpace(body))

		status, hdr, body := post("/test_project/evm/123", "["+httpPathsBalanceBody+","+strings.Replace(httpPathsBalanceBody, `"id":7`, `"id":8`, 1)+"]", nil)
		assert.Equal(t, http.StatusOK, status)
		var arr []map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &arr), body)
		assert.Len(t, arr, 2)
		assert.Equal(t, "MISS", hdr.Get("X-ERPC-Cache"))
		assert.NotEmpty(t, hdr.Get("X-ERPC-Attempts"))
	})

	t.Run("ProjectCors", func(t *testing.T) {
		status, hdr, _ := httpPathsDo(t, http.MethodOptions, base+"/test_project/evm/123", nil, map[string]string{"Origin": "https://ok.example"})
		assert.Equal(t, http.StatusNoContent, status)
		assert.Equal(t, "https://ok.example", hdr.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "true", hdr.Get("Access-Control-Allow-Credentials"))
		assert.Equal(t, "600", hdr.Get("Access-Control-Max-Age"))
		assert.Equal(t, "POST, OPTIONS", hdr.Get("Access-Control-Allow-Methods"))

		status, hdr, _ = httpPathsDo(t, http.MethodOptions, base+"/test_project/evm/123", nil, map[string]string{"Origin": "https://evil.example"})
		assert.Equal(t, http.StatusNoContent, status)
		assert.Empty(t, hdr.Get("Access-Control-Allow-Origin"))

		status, hdr, body := post("/test_project/evm/123", httpPathsBalanceBody, map[string]string{"Origin": "https://evil.example"})
		assert.Equal(t, http.StatusOK, status)
		assert.Empty(t, hdr.Get("Access-Control-Allow-Origin"))
		assert.Contains(t, body, `"result":"0x2a"`)
	})

	t.Run("ConsumerAuth", func(t *testing.T) {
		status, _, body := post("/auth_project/evm/123", httpPathsBalanceBody, nil)
		assert.Equal(t, http.StatusUnauthorized, status)
		code, _ := httpPathsErrCode(t, body)
		assert.Equal(t, int(common.JsonRpcErrorUnauthorized), code)

		status, _, body = post("/auth_project/evm/123", httpPathsBalanceBody, map[string]string{"Authorization": "Basic !!!"})
		assert.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, "illegal base64")

		status, _, body = post("/auth_project/evm/123", httpPathsBalanceBody, map[string]string{"X-ERPC-Secret-Token": "p-secret"})
		assert.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, `"result":"0x2a"`)
	})

	t.Run("Admin", func(t *testing.T) {
		status, hdr, _ := httpPathsDo(t, http.MethodOptions, base+"/admin", nil, map[string]string{"Origin": "https://admin.example"})
		assert.Equal(t, http.StatusNoContent, status)
		assert.Equal(t, "https://admin.example", hdr.Get("Access-Control-Allow-Origin"))

		status, _, body := post("/admin", `{"jsonrpc":"2.0","id":1,"method":"erpc_config","params":[]}`, map[string]string{"X-ERPC-Secret-Token": "wrong"})
		assert.Equal(t, http.StatusUnauthorized, status)
		code, _ := httpPathsErrCode(t, body)
		assert.Equal(t, int(common.JsonRpcErrorUnauthorized), code)

		status, _, body = post("/admin", `{"jsonrpc":"2.0","id":2,"method":"erpc_taxonomy","params":[]}`, map[string]string{"X-ERPC-Secret-Token": httpPathsAdminSecret})
		assert.Equal(t, http.StatusOK, status)
		code, msg := httpPathsErrCode(t, body)
		assert.Equal(t, int(common.JsonRpcErrorUnsupportedException), code)
		assert.Equal(t, "method not supported: erpc_taxonomy", msg)

		status, _, body = post("/admin", `{"jsonrpc":"2.0","id":3,"method":"erpc_doesNotExist","params":[]}`, map[string]string{"X-ERPC-Secret-Token": httpPathsAdminSecret})
		assert.Equal(t, http.StatusOK, status)
		_, msg = httpPathsErrCode(t, body)
		assert.Contains(t, msg, "erpc_doesNotExist")
	})

	t.Run("HealthcheckThroughHandler", func(t *testing.T) {
		status, _, body := httpPathsDo(t, http.MethodGet, base+"/healthcheck", nil, nil)
		assert.Equal(t, http.StatusOK, status, body)
		status, _, body = httpPathsDo(t, http.MethodGet, base+"/test_project/evm/123/healthcheck", nil, nil)
		assert.Equal(t, http.StatusOK, status, body)
	})
}

func TestHttpPaths_AuxServerOptions(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	httpPathsMockBalance()

	off := common.ExecutionHeadersOff
	cfg := &common.Config{
		Server: &common.ServerConfig{
			ListenV6:            util.BoolPtr(true),
			ExecutionHeaders:    &off,
			CostHeaders:         util.BoolPtr(true),
			TrustedIPForwarders: []string{"10.0.0.0/8", " ", "10.0.0.0/99", "not-an-ip", "192.168.1.1"},
			TrustedIPHeaders:    []string{" ", "X-Forwarded-For"},
			ResponseHeaders: map[string]string{
				"X-Paths-Static": "static-value",
				"X-Paths-Empty":  "${HTTP_PATHS_SURELY_UNSET_ENV}",
			},
		},
		Indexer:  &common.IndexerConfig{DedupWindowSize: 16},
		Projects: []*common.ProjectConfig{httpPathsProject("test_project")},
	}
	srv, base := httpPathsStart(t, cfg)
	require.NotNil(t, srv.serverV6, "listenV6 builds the IPv6 server")

	t.Run("HeadersOffCostOnCustomHeaders", func(t *testing.T) {
		status, hdr, body := httpPathsDo(t, http.MethodPost, base+"/test_project/evm/123", []byte(httpPathsBalanceBody), nil)
		assert.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, `"result":"0x2a"`)
		assert.Empty(t, hdr.Get("X-ERPC-Version"))
		assert.Empty(t, hdr.Get("X-ERPC-Attempts"))
		assert.Equal(t, "static-value", hdr.Get("X-Paths-Static"))
		_, has := hdr["X-Paths-Empty"]
		assert.False(t, has)
		assert.Equal(t, "1", hdr.Get("X-ERPC-Calls"))
		assert.Equal(t, "1", hdr.Get("X-ERPC-Billable"))
		assert.Equal(t, "eth_getBalance", hdr.Get("X-ERPC-Methods"))
		assert.Equal(t, "evm:123", hdr.Get("X-ERPC-Network-Id"))
		assert.Equal(t, "main", hdr.Get("X-ERPC-Network-Alias"))
	})

	t.Run("AdminWithoutConfigFailsAuth", func(t *testing.T) {
		status, _, body := httpPathsDo(t, http.MethodPost, base+"/admin", []byte(`{"jsonrpc":"2.0","id":1,"method":"erpc_config","params":[]}`), nil)
		// With no admin config, NewHttpServer still routes /admin; ERPC has no
		// admin auth registry so authentication fails as an internal error.
		assert.Equal(t, http.StatusOK, status)
		code, msg := httpPathsErrCode(t, body)
		assert.Equal(t, -32603, code)
		assert.Equal(t, "admin auth not configured", msg)
	})

	t.Run("TrustedForwardersParsed", func(t *testing.T) {
		mk := func(remote, xff string) *http.Request {
			r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
			if xff != "" {
				r.Header.Set("X-Forwarded-For", xff)
			}
			return r
		}
		assert.Equal(t, "1.2.3.4", srv.resolveRealClientIP(mk("10.1.2.3:555", "1.2.3.4, 10.0.0.9")))
		assert.Equal(t, "1.2.3.4", srv.resolveRealClientIP(mk("192.168.1.1:555", "1.2.3.4")))
		assert.Equal(t, "10.1.2.3", srv.resolveRealClientIP(mk("10.1.2.3:555", "10.0.0.1, 10.0.0.2")))
		assert.Equal(t, "10.1.2.3", srv.resolveRealClientIP(mk("10.1.2.3:555", "")))
		assert.Equal(t, "8.8.8.8", srv.resolveRealClientIP(mk("8.8.8.8:1", "1.2.3.4")))
		assert.Equal(t, "n/a", srv.resolveRealClientIP(mk("garbage", "")))
		assert.Len(t, srv.trustedForwarderNets, 1)
		assert.Len(t, srv.trustedForwarderIPs, 1)
		assert.Equal(t, []string{"X-Forwarded-For"}, srv.trustedIPHeaders)
	})
}

func TestHttpPaths_NewHttpServerHealthAuthError(t *testing.T) {
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &common.ServerConfig{}
	require.NoError(t, cfg.SetDefaults())
	_, err := NewHttpServer(ctx, &logger, cfg,
		&common.HealthCheckConfig{Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{{Type: "bogus"}}}},
		nil, nil, &ERPC{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create healthcheck auth registry")
}

func TestHttpPaths_ParseUrlPathAliasCombos(t *testing.T) {
	s := &HttpServer{
		draining: &atomic.Bool{},
		erpc: &ERPC{projectsRegistry: &ProjectsRegistry{preparedProjects: map[string]*PreparedProject{
			"p": {networksRegistry: &NetworksRegistry{
				aliasMu:          &sync.RWMutex{},
				aliasToNetworkId: map[string]aliasEntry{"arb": {architecture: "evm", chainID: "42161"}},
			}},
		}}},
	}
	cases := []struct {
		name             string
		method, path     string
		pre              [3]string
		want             [3]string
		wantHealth       bool
		errContains      string
	}{
		{name: "project-only too many", method: "POST", path: "/a/b/c/d", pre: [3]string{"p", "", ""}, errContains: "for project-only alias must only provide"},
		{name: "project-only explicit triple", method: "POST", path: "/q/evm/1", pre: [3]string{"p", "", ""}, want: [3]string{"q", "evm", "1"}},
		{name: "project-only alias", method: "POST", path: "/arb", pre: [3]string{"p", "", ""}, want: [3]string{"p", "evm", "42161"}},
		{name: "project-only healthcheck", method: "GET", path: "/healthcheck", pre: [3]string{"p", "", ""}, want: [3]string{"p", "", ""}, wantHealth: true},
		{name: "project+arch two segments", method: "POST", path: "/a/b", pre: [3]string{"p", "evm", ""}, errContains: "for project-and-architecture alias must only provide"},
		{name: "project+arch triple", method: "POST", path: "/q/svm/mainnet", pre: [3]string{"p", "evm", ""}, want: [3]string{"q", "svm", "mainnet"}},
		{name: "project+arch empty chain", method: "POST", path: "/", pre: [3]string{"p", "evm", ""}, errContains: "for project-and-architecture alias must provide /<chainId>"},
		{name: "project+arch healthcheck", method: "GET", path: "/healthcheck", pre: [3]string{"p", "evm", ""}, want: [3]string{"p", "evm", ""}, wantHealth: true},
		{name: "all preselected extra path", method: "POST", path: "/a/b", pre: [3]string{"p", "evm", "1"}, errContains: "must not provide anything"},
		{name: "all preselected root", method: "POST", path: "/", pre: [3]string{"p", "evm", "1"}, want: [3]string{"p", "evm", "1"}},
		{name: "arch+chain empty project", method: "POST", path: "/", pre: [3]string{"", "evm", "1"}, errContains: "for architecture-and-chain alias must provide /<project>"},
		{name: "arch+chain project", method: "POST", path: "/q", pre: [3]string{"", "evm", "1"}, want: [3]string{"q", "evm", "1"}},
		{name: "arch+chain triple", method: "POST", path: "/q/evm/5", pre: [3]string{"", "evm", "1"}, want: [3]string{"q", "evm", "5"}},
		{name: "arch+chain two segments", method: "POST", path: "/a/b", pre: [3]string{"", "evm", "1"}, errContains: "for architecture-and-chain alias must only provide"},
		{name: "arch+chain healthcheck", method: "GET", path: "/healthcheck", pre: [3]string{"", "evm", "1"}, want: [3]string{"", "evm", "1"}, wantHealth: true},
		{name: "arch-only project", method: "POST", path: "/q", pre: [3]string{"", "evm", ""}, want: [3]string{"q", "evm", ""}},
		{name: "arch-only project+chain", method: "POST", path: "/q/7", pre: [3]string{"", "evm", ""}, want: [3]string{"q", "evm", "7"}},
		{name: "arch-only triple", method: "POST", path: "/q/svm/7", pre: [3]string{"", "evm", ""}, want: [3]string{"q", "svm", "7"}},
		{name: "arch-only too many", method: "POST", path: "/a/b/c/d", pre: [3]string{"", "evm", ""}, errContains: "for architecture-only alias must only provide"},
		{name: "project+chain without arch", method: "POST", path: "/", pre: [3]string{"p", "", "1"}, errContains: "not possible to alias for project and chain WITHOUT architecture"},
		{name: "chain only", method: "POST", path: "/", pre: [3]string{"", "", "1"}, errContains: "invalid combination"},
		{name: "invalid architecture", method: "POST", path: "/q/btc/1", errContains: "architecture is not valid"},
		{name: "root GET is healthcheck", method: "GET", path: "/", wantHealth: true},
		{name: "network GET is healthcheck", method: "GET", path: "/q/evm/1", want: [3]string{"q", "evm", "1"}, wantHealth: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Method: tc.method, URL: &url.URL{Path: tc.path}, Header: http.Header{}}
			p, a, c, isAdmin, isHealth, err := s.parseUrlPath(r, tc.pre[0], tc.pre[1], tc.pre[2])
			if tc.errContains != "" {
				require.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidUrlPath))
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}
			require.NoError(t, err)
			assert.False(t, isAdmin)
			assert.Equal(t, tc.want, [3]string{p, a, c})
			assert.Equal(t, tc.wantHealth, isHealth)
		})
	}
}

func TestHttpPaths_DetermineResponseStatusCode(t *testing.T) {
	cases := []struct {
		name string
		res  interface{}
		hint []common.NetworkArchitecture
		want int
	}{
		{"plain response", common.NewNormalizedResponse(), nil, http.StatusOK},
		{"error body without cause", &HttpJsonRpcErrorResponse{}, nil, http.StatusOK},
		{"invalid url path", common.NewErrInvalidUrlPath("x", "/"), nil, http.StatusBadRequest},
		{"invalid request on svm stays 200", common.NewErrInvalidRequest(errors.New("x")), []common.NetworkArchitecture{common.ArchitectureSvm}, http.StatusOK},
		{"unauthorized", common.NewErrAuthUnauthorized("secret", "bad", "bad"), nil, http.StatusUnauthorized},
		{"endpoint unauthorized", common.NewErrEndpointUnauthorized(errors.New("x")), nil, http.StatusUnauthorized},
		{"project not found", common.NewErrProjectNotFound("p"), nil, http.StatusNotFound},
		{"network not found", common.NewErrNetworkNotFound("evm:1"), nil, http.StatusNotFound},
		{"network not supported", common.NewErrNetworkNotSupported("p", "evm:1"), nil, http.StatusNotFound},
		{"auth rate limit", common.NewErrAuthRateLimitRuleExceeded("p", "s", "b", "r", "u", "ip"), nil, http.StatusTooManyRequests},
		{"project rate limit", common.NewErrProjectRateLimitRuleExceeded("p", "b", "r"), nil, http.StatusTooManyRequests},
		{"network rate limit", common.NewErrNetworkRateLimitRuleExceeded("p", "n", "b", "r"), nil, http.StatusTooManyRequests},
		{"capacity exceeded", common.NewErrEndpointCapacityExceeded(errors.New("x")), nil, http.StatusTooManyRequests},
		{"no upstreams available", common.NewErrNetworkNoUpstreamsAvailable("p", "evm:1"), nil, http.StatusNotFound},
		{"wrapped in error body", &HttpJsonRpcErrorResponse{Cause: common.NewErrProjectNotFound("p")}, nil, http.StatusNotFound},
		{"application error", common.NewErrEndpointExecutionException(errors.New("reverted")), nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, determineResponseStatusCode(tc.res, tc.hint...))
		})
	}
}

func httpPathsRequest(t *testing.T, body string, nw *Network) *common.NormalizedRequest {
	t.Helper()
	nq := common.NewNormalizedRequest([]byte(body))
	require.NoError(t, nq.Validate())
	if nw != nil {
		nq.SetNetwork(nw)
	}
	return nq
}

func httpPathsNetwork(id, label string) *Network {
	return &Network{networkId: id, networkLabel: label, cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}
}

func httpPathsResponse(t *testing.T, nq *common.NormalizedRequest, result interface{}) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, result, nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr).WithRequest(nq)
}

func TestHttpPaths_ProcessErrorBody(t *testing.T) {
	logger := zerolog.Nop()
	started := time.Now()

	t.Run("SvmLocalRateLimitUsesGenericServerCode", func(t *testing.T) {
		nq := httpPathsRequest(t, `{"jsonrpc":"2.0","id":5,"method":"getSlot"}`, nil)
		res := processErrorBody(&logger, &started, nq, common.NewErrAuthRateLimitRuleExceeded("p", "s", "b", "r", "u", "ip"), &common.TRUE, common.ArchitectureSvm)
		body, ok := res.(*HttpJsonRpcErrorResponse)
		require.True(t, ok)
		errObj := body.Error.(map[string]interface{})
		assert.Equal(t, common.JsonRpcErrorNumber(-32000), errObj["code"])
		assert.Equal(t, "rate-limit exceeded", errObj["message"])
		assert.Equal(t, http.StatusTooManyRequests, determineResponseStatusCode(body))
	})

	t.Run("EvmRateLimitKeepsCapacityCode", func(t *testing.T) {
		nq := httpPathsRequest(t, `{"jsonrpc":"2.0","id":5,"method":"eth_blockNumber"}`, nil)
		body := processErrorBody(&logger, &started, nq, common.NewErrProjectRateLimitRuleExceeded("p", "b", "r"), &common.TRUE).(*HttpJsonRpcErrorResponse)
		assert.Equal(t, common.JsonRpcErrorCapacityExceeded, body.Error.(map[string]interface{})["code"])
	})

	t.Run("ErrorDetailsOmittedForEthCall", func(t *testing.T) {
		cause := common.NewErrNetworkNotFound("evm:9")
		call := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`, nil)
		other := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":[]}`, nil)
		callObj := processErrorBody(&logger, &started, call, cause, &common.TRUE).(*HttpJsonRpcErrorResponse).Error.(map[string]interface{})
		otherObj := processErrorBody(&logger, &started, other, cause, &common.TRUE).(*HttpJsonRpcErrorResponse).Error.(map[string]interface{})
		hiddenObj := processErrorBody(&logger, &started, other, cause, &common.FALSE).(*HttpJsonRpcErrorResponse).Error.(map[string]interface{})
		_, callHasData := callObj["data"]
		_, hiddenHasData := hiddenObj["data"]
		assert.False(t, callHasData)
		assert.False(t, hiddenHasData)
		assert.Equal(t, cause, otherObj["data"])
	})

	t.Run("SingleExhaustedCauseIsUnwrapped", func(t *testing.T) {
		ex := common.NewErrUpstreamsExhaustedWithCause(errors.Join(common.NewErrEndpointUnauthorized(errors.New("bad key"))))
		body := processErrorBody(&logger, &started, nil, ex, &common.TRUE).(*HttpJsonRpcErrorResponse)
		assert.Equal(t, http.StatusUnauthorized, determineResponseStatusCode(body))
		assert.Equal(t, "2.0", body.Jsonrpc)
		assert.Nil(t, body.Id)
	})

	t.Run("PlainAndContextErrorsBecomeServerErrors", func(t *testing.T) {
		for _, err := range []error{
			errors.New("boom"),
			context.Canceled,
			context.DeadlineExceeded,
			common.NewErrEndpointServerSideException(errors.New("upstream 500"), nil, 500),
		} {
			nq := httpPathsRequest(t, `{"jsonrpc":"2.0","id":9,"method":"eth_chainId"}`, nil)
			body, ok := processErrorBody(&logger, &started, nq, err, &common.FALSE).(*HttpJsonRpcErrorResponse)
			require.True(t, ok, "%v", err)
			assert.Equal(t, http.StatusOK, determineResponseStatusCode(body), "%v", err)
			assert.NotNil(t, body.Error.(map[string]interface{})["code"], "%v", err)
		}
	})
}

func TestHttpPaths_ErrorResponseLogMarshal(t *testing.T) {
	var buf bytes.Buffer
	// Package tests disable logging globally; the event must be enabled to
	// run the marshaller.
	lg := zerolog.New(&buf).Level(zerolog.InfoLevel)
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	defer zerolog.SetGlobalLevel(prev)
	lg.Info().Object("r", &HttpJsonRpcErrorResponse{
		Jsonrpc: "2.0", Id: 4,
		Error: common.NewErrJsonRpcExceptionExternal(-32000, "ext-msg", ""),
	}).Msg("")
	lg.Info().Object("r", &HttpJsonRpcErrorResponse{
		Jsonrpc: "2.0", Id: 5,
		Error: map[string]interface{}{"code": -32601, "message": "map-msg"},
	}).Msg("")
	lg.Info().Object("r", (*HttpJsonRpcErrorResponse)(nil)).Msg("")
	out := buf.String()
	assert.Contains(t, out, "ext-msg")
	assert.Contains(t, out, "map-msg")
	assert.Contains(t, out, `"id":5`)
}

func TestHttpPaths_IsSvmRequest(t *testing.T) {
	svmBody := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"getSlot","networkId":"svm:mainnet"}`, nil)
	evmNet := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","networkId":"svm:mainnet"}`, httpPathsNetwork("evm:1", ""))
	svmNet := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"getSlot"}`, httpPathsNetwork("svm:mainnet", ""))
	assert.True(t, isSvmRequest(nil, common.ArchitectureSvm))
	assert.False(t, isSvmRequest(nil))
	assert.True(t, isSvmRequest(svmBody))
	assert.False(t, isSvmRequest(svmBody, common.ArchitectureEvm))
	assert.False(t, isSvmRequest(evmNet))
	assert.True(t, isSvmRequest(svmNet))
}

func TestHttpPaths_CostHeaders(t *testing.T) {
	ctx := context.Background()
	on := &HttpServer{serverCfg: &common.ServerConfig{CostHeaders: util.BoolPtr(true)}}
	nwA := httpPathsNetwork("evm:1", "one")

	r1 := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`, nwA)
	r1.ExecState().RecordUpstreamAttempt(common.UpstreamAttempt{UpstreamId: "a", VendorName: "alchemy", CreditUnits: 5})
	r1.ExecState().RecordUpstreamAttempt(common.UpstreamAttempt{UpstreamId: "a", VendorName: "alchemy", CreditUnits: 0})
	r2 := httpPathsRequest(t, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":[]}`, nwA)
	r2.ExecState().RecordUpstreamAttempt(common.UpstreamAttempt{UpstreamId: "b", VendorName: "drpc", CreditUnits: 3})
	reverted := &HttpJsonRpcErrorResponse{Cause: common.NewErrEndpointExecutionException(errors.New("reverted")), Request: r2}
	transport := &HttpJsonRpcErrorResponse{Cause: errors.New("dial"), Request: nil}

	h := on.costHeaders(ctx, []interface{}{httpPathsResponse(t, r1, "0x1"), reverted, transport})
	require.NotNil(t, h)
	assert.Equal(t, "3", h.Get("X-ERPC-Calls"))
	assert.Equal(t, "2", h.Get("X-ERPC-Billable"))
	assert.Equal(t, "eth_call,eth_getBalance", h.Get("X-ERPC-Methods"))
	assert.Equal(t, "alchemy:eth_call=5;drpc:eth_getBalance=3", h.Get("X-ERPC-Credits"))
	assert.Equal(t, "8", h.Get("X-ERPC-Credits-Total"))
	assert.Equal(t, common.ErpcVersion, h.Get("X-ERPC-Credits-Version"))
	assert.Equal(t, "evm:1", h.Get("X-ERPC-Network-Id"))
	assert.Equal(t, "one", h.Get("X-ERPC-Network-Alias"))

	rOther := httpPathsRequest(t, `{"jsonrpc":"2.0","id":3,"method":"eth_chainId"}`, httpPathsNetwork("evm:2", ""))
	h = on.costHeaders(ctx, []interface{}{httpPathsResponse(t, r1, "0x1"), httpPathsResponse(t, rOther, "0x2")})
	assert.Empty(t, h.Get("X-ERPC-Network-Id"), "disagreeing networks are not reported")
	assert.Empty(t, h.Get("X-ERPC-Network-Alias"))

	assert.Nil(t, on.costHeaders(ctx, nil))
	off := &HttpServer{serverCfg: &common.ServerConfig{}}
	assert.Nil(t, off.costHeaders(ctx, []interface{}{reverted}))
	assert.False(t, isBillableItem(ctx, "not a response"))
}

func httpPathsExecRequest(t *testing.T, upstream string, n int) *common.NormalizedRequest {
	nq := httpPathsRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`, nil)
	st := nq.ExecState()
	st.UpstreamAttempts.Add(int32(n))
	st.CacheAttempts.Add(1)
	st.ConsensusSlots.Add(2)
	st.ConsensusDisputes.Add(1)
	st.ConsensusLowParticipants.Add(1)
	for range n {
		st.RecordUpstreamAttempt(common.UpstreamAttempt{
			UpstreamId: upstream, Reason: common.SelectionReasonPrimary,
			Outcome: common.UpstreamOutcomeSuccess, Duration: 12 * time.Millisecond,
		})
	}
	st.MarkUpstreamAttemptWon(upstream)
	return nq
}

func TestHttpPaths_BatchExecHeaders(t *testing.T) {
	ctx := context.Background()
	all := &HttpServer{serverCfg: &common.ServerConfig{}}
	r1 := httpPathsExecRequest(t, "up1", 30)
	r2 := httpPathsExecRequest(t, "up2", 30)
	hit := httpPathsResponse(t, r1, "0x1").SetFromCache(true)
	hit.SetDuration(40 * time.Millisecond)
	miss := httpPathsResponse(t, r2, "0x2")
	miss.SetDuration(90 * time.Millisecond)

	w := httptest.NewRecorder()
	all.writeBatchExecHeaders(ctx, w, []interface{}{hit, miss, &HttpJsonRpcErrorResponse{Cause: errors.New("x")}})
	hdr := w.Header()
	assert.Equal(t, "62", hdr.Get("X-ERPC-Attempts"))
	assert.Equal(t, "60", hdr.Get("X-ERPC-Upstream-Attempts"))
	assert.Equal(t, "2", hdr.Get("X-ERPC-Cache-Attempts"))
	assert.Equal(t, "4", hdr.Get("X-ERPC-Consensus-Slots"))
	assert.Equal(t, "2", hdr.Get("X-ERPC-Consensus-Disputes"))
	assert.Equal(t, "2", hdr.Get("X-ERPC-Consensus-Low-Participants"))
	assert.Equal(t, "PARTIAL:1", hdr.Get("X-ERPC-Cache"))
	assert.Equal(t, "90", hdr.Get("X-ERPC-Duration"))
	assert.Equal(t, "10", hdr.Get("X-ERPC-Upstreams-Truncated"))
	segs := strings.Split(hdr.Get("X-ERPC-Upstreams"), ";")
	assert.Len(t, segs, maxBatchTraceSegments)
	assert.Equal(t, "up1=primary:success:12ms", segs[0])

	w = httptest.NewRecorder()
	all.writeBatchExecHeaders(ctx, w, []interface{}{hit})
	assert.Equal(t, "HIT", w.Header().Get("X-ERPC-Cache"))

	summary := common.ExecutionHeadersSummary
	w = httptest.NewRecorder()
	(&HttpServer{serverCfg: &common.ServerConfig{ExecutionHeaders: &summary}}).writeBatchExecHeaders(ctx, w, []interface{}{miss})
	assert.Equal(t, "MISS", w.Header().Get("X-ERPC-Cache"))
	assert.Empty(t, w.Header().Get("X-ERPC-Upstreams"))
	assert.Equal(t, "31", w.Header().Get("X-ERPC-Attempts"))

	offMode := common.ExecutionHeadersOff
	w = httptest.NewRecorder()
	(&HttpServer{serverCfg: &common.ServerConfig{ExecutionHeaders: &offMode}}).writeBatchExecHeaders(ctx, w, []interface{}{miss})
	assert.Empty(t, w.Header())
}

func TestHttpPaths_SingleResponseHeaders(t *testing.T) {
	ctx := context.Background()
	r := httpPathsExecRequest(t, "up1", 2)
	resp := httpPathsResponse(t, r, "0x1").SetFromCache(true)
	resp.SetDuration(7 * time.Millisecond)

	w := httptest.NewRecorder()
	setResponseHeaders(ctx, resp, w, common.ExecutionHeadersAll)
	hdr := w.Header()
	assert.Equal(t, "HIT", hdr.Get("X-ERPC-Cache"))
	assert.Equal(t, "7", hdr.Get("X-ERPC-Duration"))
	assert.Equal(t, "3", hdr.Get("X-ERPC-Attempts"))
	assert.Equal(t, "1", hdr.Get("X-ERPC-Cache-Attempts"))
	assert.Equal(t, "2", hdr.Get("X-ERPC-Consensus-Slots"))
	assert.Equal(t, "1", hdr.Get("X-ERPC-Consensus-Disputes"))
	assert.Equal(t, "1", hdr.Get("X-ERPC-Consensus-Low-Participants"))
	assert.Equal(t, "up1=primary:success:12ms;up1=primary:success:12ms:won", hdr.Get("X-ERPC-Upstreams"))

	w = httptest.NewRecorder()
	setResponseHeaders(ctx, resp, w, common.ExecutionHeadersOff)
	assert.Empty(t, w.Header())

	w = httptest.NewRecorder()
	setResponseHeaders(ctx, map[string]interface{}{"cause": errors.New("x")}, w, common.ExecutionHeadersAll)
	assert.Equal(t, "0", w.Header().Get("X-ERPC-Attempts"))
	assert.Empty(t, w.Header().Get("X-ERPC-Cache"))
	assert.Nil(t, lookupResponseMetadata(&HttpJsonRpcErrorResponse{Cause: errors.New("x")}))
	assert.Nil(t, lookupResponseMetadata(42))
}

func TestHttpPaths_ClientIpParsing(t *testing.T) {
	assert.Equal(t, "::1", parseRemoteIP("[::1]:80").String())
	assert.Equal(t, "::1", parseRemoteIP("[::1]").String())
	assert.Equal(t, "1.2.3.4", parseRemoteIP("1.2.3.4").String())
	assert.Nil(t, parseRemoteIP("nope"))

	ips := parseXForwardedFor(" 1.1.1.1 , [2001:db8::1], 2.2.2.2:8080, junk")
	require.Len(t, ips, 3)
	assert.Equal(t, "2001:db8::1", ips[1].String())
	assert.Equal(t, "2.2.2.2", ips[2].String())

	fwd := parseForwardedFor(`for=192.0.2.60;proto=http;by=203.0.113.43, For="[2001:db8:cafe::17]:4711", for=unknown, proto=https`)
	require.Len(t, fwd, 2)
	assert.Equal(t, "192.0.2.60", fwd[0].String())
	assert.Equal(t, "2001:db8:cafe::17", fwd[1].String())

	trusted := func(ip net.IP) bool { return ip.IsPrivate() }
	assert.Nil(t, trimRightTrustedAndPick(nil, trusted))
	assert.Nil(t, trimRightTrustedAndPick([]net.IP{net.ParseIP("10.0.0.1")}, trusted))
	assert.Equal(t, "8.8.8.8", trimRightTrustedAndPick([]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")}, trusted).String())

	s := &HttpServer{}
	assert.False(t, s.isTrustedForwarder(nil))
	assert.False(t, s.isTrustedForwarder(net.ParseIP("1.1.1.1")))
}

func TestHttpPaths_MethodAndHeaderFilters(t *testing.T) {
	prj := func(cfg *common.ProjectConfig) *PreparedProject { return &PreparedProject{Config: cfg} }

	ok, err := isMethodAllowed(nil, "eth_call")
	require.NoError(t, err)
	assert.True(t, ok)
	cases := []struct {
		ignore, allow []string
		method        string
		want          bool
		wantErr       bool
	}{
		{ignore: []string{"eth_*"}, method: "eth_call", want: false},
		{ignore: []string{"eth_*"}, allow: []string{"eth_call"}, method: "eth_call", want: true},
		{ignore: []string{"debug_*"}, method: "eth_call", want: true},
		{ignore: []string{"("}, method: "eth_call", wantErr: true},
		{allow: []string{"("}, method: "eth_call", wantErr: true},
	}
	for _, tc := range cases {
		got, err := isMethodAllowed(prj(&common.ProjectConfig{IgnoreMethods: tc.ignore, AllowMethods: tc.allow}), tc.method)
		if tc.wantErr {
			assert.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%v/%v %s", tc.ignore, tc.allow, tc.method)
	}

	nq := common.NewNormalizedRequest(nil)
	nq.ForwardHeaders = http.Header{}
	require.NoError(t, applyForwardHeaders(nil, nq, http.Header{"X-A": {"1"}}))
	assert.Empty(t, nq.ForwardHeaders)
	require.NoError(t, applyForwardHeaders(prj(&common.ProjectConfig{ForwardHeaders: []string{"X-Fwd-*"}}), nq,
		http.Header{"X-Fwd-One": {"a", "b"}, "X-Other": {"c"}}))
	assert.Equal(t, []string{"a", "b"}, nq.ForwardHeaders.Values("X-Fwd-*"))
	assert.Empty(t, nq.ForwardHeaders.Values("X-Other"))
	assert.Error(t, applyForwardHeaders(prj(&common.ProjectConfig{ForwardHeaders: []string{"("}}), nq, http.Header{"X-A": {"1"}}))

	adminCases := []struct {
		deny, allow []string
		method      string
		blocked     bool
		wantErr     bool
	}{
		{method: "erpc_config", blocked: false},
		{deny: []string{"erpc_*"}, method: "erpc_config", blocked: true},
		{deny: []string{"erpc_*"}, allow: []string{"erpc_config"}, method: "erpc_config", blocked: false},
		{allow: []string{"erpc_listCordoned"}, method: "erpc_config", blocked: true},
		{allow: []string{"erpc_config"}, method: "erpc_config", blocked: false},
		{deny: []string{"("}, method: "erpc_config", wantErr: true},
		{deny: []string{"erpc_*"}, allow: []string{"("}, method: "erpc_config", wantErr: true},
		{allow: []string{"("}, method: "erpc_config", wantErr: true},
	}
	for _, tc := range adminCases {
		blocked, err := isAdminMethodBlocked(&common.AdminConfig{DenyMethods: tc.deny, AllowMethods: tc.allow}, tc.method)
		if tc.wantErr {
			assert.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.blocked, blocked, "%v/%v", tc.deny, tc.allow)
	}
}

func TestHttpPaths_GzipWriter(t *testing.T) {
	t.Run("FlushBelowThresholdCommitsPlain", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &conditionalGzipWriter{ResponseWriter: rec, pool: util.NewGzipWriterPool()}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("small"))
		w.Flush()
		w.WriteHeader(http.StatusTeapot) // already decided: passes through (ignored by recorder after first)
		_, _ = w.Write([]byte("-more"))
		require.NoError(t, w.Close())
		assert.Equal(t, http.StatusAccepted, rec.Code)
		assert.Empty(t, rec.Header().Get("Content-Encoding"))
		assert.Equal(t, "small-more", rec.Body.String())
	})

	t.Run("LargeBodyCompressedAcrossWritesAndFlushes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &conditionalGzipWriter{ResponseWriter: rec, pool: util.NewGzipWriterPool()}
		part := strings.Repeat("a", compressionThreshold)
		_, _ = w.Write([]byte(part))
		w.Flush()
		_, _ = w.Write([]byte("tail"))
		require.NoError(t, w.Close())
		assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		zr, err := gzip.NewReader(rec.Body)
		require.NoError(t, err)
		plain, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.Equal(t, part+"tail", string(plain))
	})

	t.Run("HandlerSkipsWithoutAcceptEncodingAndForUpgrades", func(t *testing.T) {
		big := strings.Repeat("b", 2*compressionThreshold)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, big) })
		h := gzipHandler(next, true)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
		assert.Equal(t, "Accept-Encoding", rec.Header().Get("Vary"))
		assert.Empty(t, rec.Header().Get("Content-Encoding"))
		assert.Equal(t, big, rec.Body.String())

		up := httptest.NewRequest(http.MethodGet, "/", nil)
		up.Header.Set("Connection", "Upgrade")
		up.Header.Set("Upgrade", "websocket")
		up.Header.Set("Accept-Encoding", "gzip")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, up)
		assert.Empty(t, rec.Header().Get("Vary"))
		assert.Empty(t, rec.Header().Get("Content-Encoding"))
	})
}

func httpPathsWriteCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDer, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0o600))
	return certFile, keyFile
}

func TestHttpPaths_TLSConfig(t *testing.T) {
	certFile, keyFile := httpPathsWriteCert(t)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	require.NoError(t, os.WriteFile(garbage, []byte("not a cert"), 0o600))

	mk := func(tlsCfg *common.TLSConfig) *HttpServer {
		return &HttpServer{serverCfg: &common.ServerConfig{TLS: tlsCfg}}
	}
	cfg, err := mk(&common.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, InsecureSkipVerify: true}).createTLSConfig()
	require.NoError(t, err)
	assert.Len(t, cfg.Certificates, 1)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.True(t, cfg.InsecureSkipVerify)
	assert.Equal(t, tls.NoClientCert, cfg.ClientAuth)

	cfg, err = mk(&common.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, CAFile: certFile}).createTLSConfig()
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	assert.NotNil(t, cfg.ClientCAs)

	_, err = mk(&common.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, CAFile: garbage}).createTLSConfig()
	assert.ErrorContains(t, err, "failed to parse CA certificate")
	_, err = mk(&common.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, CAFile: garbage + ".missing"}).createTLSConfig()
	assert.ErrorContains(t, err, "failed to read CA file")
	_, err = mk(&common.TLSConfig{Enabled: true, CertFile: garbage, KeyFile: keyFile}).createTLSConfig()
	assert.ErrorContains(t, err, "failed to load TLS certificate and key")
}

func TestHttpPaths_StartAndShutdownErrors(t *testing.T) {
	logger := zerolog.Nop()
	host := "127.0.0.1"
	badPort := -1
	port := 0
	badTLS := &common.TLSConfig{Enabled: true, CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem"}
	cases := []struct {
		name string
		srv  *HttpServer
		want string
	}{
		{"no servers", &HttpServer{serverCfg: &common.ServerConfig{}}, "you must configure at least one"},
		{"v4 port missing", &HttpServer{serverV4: &http.Server{}, serverCfg: &common.ServerConfig{ListenV4: util.BoolPtr(true)}}, "server.httpPortV4 is not configured"},
		{"v6 port missing", &HttpServer{serverV6: &http.Server{}, serverCfg: &common.ServerConfig{ListenV6: util.BoolPtr(true)}}, "server.httpPortV6 is not configured"},
		{"v4 bad tls", &HttpServer{serverV4: &http.Server{}, serverCfg: &common.ServerConfig{ListenV4: util.BoolPtr(true), HttpHostV4: &host, HttpPortV4: &port, TLS: badTLS}}, "failed to create TLS config"},
		{"v6 bad tls", &HttpServer{serverV6: &http.Server{}, serverCfg: &common.ServerConfig{ListenV6: util.BoolPtr(true), HttpHostV6: &host, HttpPortV6: &port, TLS: badTLS}}, "failed to create TLS config"},
		{"v4 listen fails", &HttpServer{serverV4: &http.Server{}, serverCfg: &common.ServerConfig{ListenV4: util.BoolPtr(true), HttpHostV4: &host, HttpPortV4: &badPort}}, "IPv4 server error"},
		{"v6 listen fails", &HttpServer{serverV6: &http.Server{}, serverCfg: &common.ServerConfig{ListenV6: util.BoolPtr(true), HttpHostV6: &host, HttpPortV6: &badPort}}, "IPv6 server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.srv.Start(&logger)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("ShutdownBothUnstartedServers", func(t *testing.T) {
		s := &HttpServer{serverV4: &http.Server{}, serverV6: &http.Server{}, draining: &atomic.Bool{}}
		require.NoError(t, s.Shutdown(&logger))
		assert.True(t, s.draining.Load())
	})
}

func TestHttpPaths_PrewarmSettlesHttpOnlyNetworks(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	cfg := &common.Config{
		Server:   &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{Enabled: util.BoolPtr(true)}},
		Projects: []*common.ProjectConfig{httpPathsProject("test_project")},
	}
	srv, _ := httpPathsStart(t, cfg)
	prj, err := srv.erpc.GetProject("test_project")
	require.NoError(t, err)
	_, err = prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	old := prewarmInterval
	prewarmInterval = 10 * time.Millisecond
	defer func() { prewarmInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.PrewarmUpstreamSubscriptions(ctx)
		close(done)
	}()
	time.AfterFunc(100*time.Millisecond, cancel)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prewarm did not stop on context cancel")
	}
	_, bootstrapped := srv.subscriptionManager.networks.Load("test_project/evm:123")
	assert.False(t, bootstrapped, "an HTTP-only network is never bootstrapped for subscriptions")

	// Disabled WebSocket: returns immediately even with a live context.
	off := &HttpServer{serverCfg: &common.ServerConfig{}, erpc: srv.erpc, subscriptionManager: srv.subscriptionManager}
	finished := make(chan struct{})
	go func() { off.PrewarmUpstreamSubscriptions(context.Background()); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("prewarm with WebSocket disabled must return immediately")
	}
}
