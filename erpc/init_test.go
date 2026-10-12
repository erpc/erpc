package erpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

var mainMutex sync.Mutex

func init() {
	util.ConfigureTestLogger()
}

func TestInit_AllGood(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	defer gock.Off()
	defer gock.DisableNetworking()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	//
	// 1) Create a new mock EVM JSON-RPC server
	//
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Times(5).
		Post("").
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	//
	// 2) Initialize the eRPC server with a mock configuration
	//
	localHost := "localhost"
	localPort := rand.Intn(1000) + 2000
	localBaseUrl := fmt.Sprintf("http://localhost:%s", fmt.Sprint(localPort))

	cfg := &common.Config{
		LogLevel: "DEBUG",
		Server: &common.ServerConfig{
			HttpHostV4: &localHost,
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: &localPort,
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "main",
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "good-evm-rpc",
						Endpoint: "http://rpc1.localhost",
						Type:     "evm",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
					},
				},
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
					},
				},
			},
		},
	}

	logger := log.Logger
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go Init(ctx, cfg, logger)
	time.Sleep(1 * time.Second)

	//
	// 3) Make a request to the eRPC server
	//
	body := bytes.NewBuffer([]byte(`
		{
			"method": "eth_getBalance",
			"params": [
				"0x1273c18",
				false
			],
			"id": 91799,
			"jsonrpc": "2.0"
		}
	`))
	res, err := http.Post(fmt.Sprintf("%s/main/evm/123", localBaseUrl), "application/json", body)

	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", res.StatusCode)
	}
	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("error reading response: %s", err)
	}

	//
	// 4) Assert the response
	//
	respObject := make(map[string]interface{})
	err = sonic.Unmarshal(respBody, &respObject)
	if err != nil {
		t.Fatalf("error unmarshalling: %s response body: %s", err, respBody)
	}

	if _, ok := respObject["result"].(map[string]interface{})["hash"]; !ok {
		t.Errorf("expected hash in response, got %v", respObject)
	}
}

func TestInit_InvalidHttpPort(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	cfg := &common.Config{
		LogLevel: "DEBUG",
		Server: &common.ServerConfig{
			HttpHostV4: util.StringPtr("localhost"),
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: util.IntPtr(-1),
		},
	}

	logger := log.Logger

	// Replace exit channel with a buffered channel
	exitChan := make(chan int, 1)
	util.OsExit = func(code int) {
		exitChan <- code
	}

	// Launch init
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	Init(ctx, cfg, logger)

	select {
	case code := <-exitChan:
		if code != util.ExitCodeHttpServerFailed {
			t.Errorf("expected exit code %d, got %d", util.ExitCodeHttpServerFailed, code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Init to return an error")
	}
}

// After SIGTERM, Init must not return (main exits right after it) while the
// HTTP server is still draining requests that were in flight when the drain
// began, even when waitAfterShutdown is far shorter than that drain. Both an
// HTTP/1.1 and a cleartext HTTP/2 request are held open across the shutdown,
// so the drain Init waits for has to cover both kinds of connection.
func TestInit_WaitsForHttpDrainBeforeReturning(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	defer gock.Off()
	defer gock.DisableNetworking()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		host := strings.Split(req.URL.Host, ":")[0]
		return host == "localhost" || host == "127.0.0.1"
	})
	util.SetupMocksForEvmStatePoller()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := probe.Addr().(*net.TCPAddr).Port
	require.NoError(t, probe.Close())

	cfg := h2cTestConfig()
	cfg.Server.HttpPortV4 = &port
	cfg.Server.WaitBeforeShutdown = common.Duration(50 * time.Millisecond).Ptr()
	cfg.Server.WaitAfterShutdown = common.Duration(10 * time.Millisecond).Ptr()

	appCtx, sigterm := context.WithCancel(context.Background())
	defer sigterm()
	initDone := make(chan error, 1)
	go func() { initDone <- Init(appCtx, cfg, log.Logger) }()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}, 10*time.Second, 20*time.Millisecond, "http server never started listening")

	h1 := &http.Transport{}
	defer h1.CloseIdleConnections()
	h2c := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	defer h2c.CloseIdleConnections()

	// Each request's body arrives in two halves, so its handler stays blocked on
	// the read until the test releases the second half.
	const reqBody = `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`
	type result struct {
		proto  int
		status int
		body   string
		err    error
	}
	type held struct {
		name   string
		bodyW  *io.PipeWriter
		result chan result
	}
	hold := func(name string, rt http.RoundTripper) held {
		bodyR, bodyW := io.Pipe()
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/main/evm/123", bodyR)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		h := held{name: name, bodyW: bodyW, result: make(chan result, 1)}
		go func() {
			resp, err := rt.RoundTrip(req)
			if err != nil {
				h.result <- result{err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			h.result <- result{proto: resp.ProtoMajor, status: resp.StatusCode, body: string(body), err: err}
		}()
		_, err = bodyW.Write([]byte(reqBody[:20]))
		require.NoError(t, err)
		return h
	}
	inFlight := []held{hold("http/1.1", h1), hold("h2c", h2c)}
	time.Sleep(200 * time.Millisecond)

	sigterm()

	// waitBeforeShutdown + waitAfterShutdown is 60ms; the requests are held far
	// longer, so returning here means Init did not wait for the drain.
	select {
	case err := <-initDone:
		t.Fatalf("Init returned (err=%v) while requests were still in flight on the draining server", err)
	case <-time.After(1500 * time.Millisecond):
	}

	for i, h := range inFlight {
		_, err := h.bodyW.Write([]byte(reqBody[20:]))
		require.NoError(t, err, h.name)
		require.NoError(t, h.bodyW.Close(), h.name)
		select {
		case r := <-h.result:
			require.NoError(t, r.err, "%s request in flight at SIGTERM must complete", h.name)
			require.Equal(t, i+1, r.proto, h.name)
			require.Equal(t, http.StatusOK, r.status, h.name)
			require.Contains(t, r.body, `"result":"0x7b"`, h.name)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s request in flight at SIGTERM never completed", h.name)
		}
	}

	select {
	case err := <-initDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Init did not return after the drain completed")
	}
}

// End-to-end: config.metrics.counterDropLabels is applied at Init, real request
// traffic with distinct User-Agents increments package counters, and the
// metrics HTTP scrape proves agent_name is gone on dropped counters while a
// counterLabelOverrides keep-list still exposes it on the spared metric.
func TestInit_CounterDropLabels_EndToEnd(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()
	// Do NOT swap prometheus.DefaultRegisterer/Gatherer. Init calls
	// SetHistogramBuckets which reassigns package-level histogram vars onto
	// whichever Registerer is current; a private registry leaves the rest of
	// the package observing collectors DefaultGatherer can no longer see
	// (breaks TestUpstream_TimeoutPolicy/HistogramEmits under make test).
	// Counters stay unregistered until an Init with metrics config runs, and
	// this test is the one that first registers them — under the drop filter.
	t.Cleanup(func() {
		// Filter is process-lifetime against DefaultRegisterer (dimHashesByName
		// freezes the label set). Leave counters registered as-is; call sites
		// still pass the full schema and LabeledCounter projects. Only clear
		// the handle cache so nothing holds stale children.
		telemetry.ResetHandleCache()
	})

	defer gock.Off()
	defer gock.DisableNetworking()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})

	util.SetupMocksForEvmStatePoller()
	// Persist-reply every upstream POST so state-poller + client traffic both land.
	gock.New("http://rpc1.localhost").
		Persist().
		Post("").
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))

	localHost := "localhost"
	httpPort := rand.Intn(1000) + 2100
	metricsPort := rand.Intn(1000) + 4100
	baseURL := fmt.Sprintf("http://localhost:%d", httpPort)
	metricsURL := fmt.Sprintf("http://localhost:%d/metrics", metricsPort)

	cfg := &common.Config{
		LogLevel: "ERROR",
		Server: &common.ServerConfig{
			HttpHostV4: &localHost,
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: &httpPort,
		},
		Metrics: &common.MetricsConfig{
			Enabled: util.BoolPtr(true),
			HostV4:  &localHost,
			Port:    &metricsPort,
			// Drop agent_name fleet-wide on counters, then spare the received
			// counter so we can prove overrides still work end-to-end.
			CounterDropLabels: []string{"agent_name"},
			CounterLabelOverrides: map[string][]string{
				"network_request_received_total": {"agent_name"},
			},
		},
		Projects: []*common.ProjectConfig{
			{
				Id:            "main",
				UserAgentMode: common.UserAgentTrackingModeRaw,
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "good-evm-rpc",
						Endpoint: "http://rpc1.localhost",
						Type:     "evm",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
					},
				},
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
					},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Init(ctx, cfg, log.Logger)

	// Wait until metrics port answers.
	deadline := time.Now().Add(8 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for metrics to come up")
		}
		resp, err := http.Get(metricsURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	agents := []string{
		"e2e-agent-alpha/1.0",
		"e2e-agent-beta/2.0",
		"e2e-agent-gamma/3.0",
	}
	for _, ua := range agents {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/main/evm/123", bytes.NewBufferString(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1273c18","latest"]}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request with ua=%s: %v", ua, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("expected 200 for ua=%s, got %d body=%s", ua, res.StatusCode, body)
		}
	}

	// Scrape until the request counters appear (response path is async w.r.t. metrics).
	var scrape string
	for range 50 {
		resp, err := http.Get(metricsURL)
		if err != nil {
			t.Fatalf("metrics scrape: %v", err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		scrape = string(b)
		if strings.Contains(scrape, "erpc_network_successful_request_total{") &&
			strings.Contains(scrape, "erpc_network_request_received_total{") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Dropped metric: successful_request must NOT carry agent_name, and the
	// three UAs must collapse to a single series (sum preserved at ≥3).
	successLines := metricSampleLines(scrape, "erpc_network_successful_request_total")
	if len(successLines) == 0 {
		t.Fatalf("no successful_request samples in scrape; first 800 bytes:\n%s", scrape[:min(800, len(scrape))])
	}
	for _, line := range successLines {
		if strings.Contains(line, "agent_name=") {
			t.Fatalf("agent_name should be dropped from successful_request, got: %s", line)
		}
	}
	if len(successLines) != 1 {
		t.Fatalf("expected 3 UAs to collapse to 1 successful_request series, got %d:\n%s",
			len(successLines), strings.Join(successLines, "\n"))
	}
	if v := metricSampleValue(successLines[0]); v < 3 {
		t.Fatalf("expected collapsed successful_request total ≥3, got %v from %s", v, successLines[0])
	}

	// Overridden metric: received_total keeps agent_name and has one series per UA.
	recvLines := metricSampleLines(scrape, "erpc_network_request_received_total")
	if len(recvLines) == 0 {
		t.Fatal("no request_received samples in scrape")
	}
	seen := map[string]bool{}
	for _, line := range recvLines {
		if !strings.Contains(line, "agent_name=") {
			t.Fatalf("override should keep agent_name on request_received, got: %s", line)
		}
		for _, ua := range agents {
			if strings.Contains(line, `agent_name="`+ua+`"`) {
				seen[ua] = true
			}
		}
	}
	for _, ua := range agents {
		if !seen[ua] {
			t.Fatalf("missing overridden series for ua=%s in:\n%s", ua, strings.Join(recvLines, "\n"))
		}
	}

	// Dropped family also collapses on the upstream counter (no agent_name).
	upLines := metricSampleLines(scrape, "erpc_upstream_request_total")
	for _, line := range upLines {
		if strings.Contains(line, "agent_name=") {
			t.Fatalf("agent_name should be dropped from upstream_request_total, got: %s", line)
		}
	}
	if len(upLines) == 0 {
		t.Fatal("expected upstream_request_total samples after traffic")
	}
}

func metricSampleLines(scrape, name string) []string {
	var out []string
	prefix := name + "{"
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

func metricSampleValue(line string) float64 {
	// "...} 3" or "...} 3.0"
	i := strings.LastIndex(line, " ")
	if i < 0 {
		return 0
	}
	var v float64
	fmt.Sscanf(line[i+1:], "%f", &v)
	return v
}
