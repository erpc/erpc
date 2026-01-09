package erpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
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
	defer util.CancelAndWait(cancel)
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
	defer util.CancelAndWait(cancel)
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
