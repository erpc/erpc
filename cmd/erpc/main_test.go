package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

var mainMutex sync.Mutex

func TestMain_RealConfigFile(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	localHost := "localhost"
	localPort := fmt.Sprint(rand.Intn(1000) + 2000)
	localBaseUrl := fmt.Sprintf("http://localhost:%s", localPort)
	f.WriteString(`
logLevel: DEBUG

server:
  httpHostV4: "` + localHost + `"
  listenV4: true
  httpPort: ` + localPort + `
`)

	os.Args = []string{"erpc-test", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	// check if the server is running
	if _, err := http.Get(localBaseUrl); err != nil {
		t.Fatalf("expected server to be running, got %v", err)
	}
}

func TestMain_MissingConfigFile(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	mu := &sync.Mutex{}

	os.Args = []string{"erpc-test", "some-random-non-existent.yaml"}

	var called bool

	util.OsExit = func(code int) {
		if code != util.ExitCodeERPCStartFailed {
			t.Errorf("expected code %d, got %d", util.ExitCodeERPCStartFailed, code)
		} else {
			mu.Lock()
			defer mu.Unlock()
			called = true
		}
	}

	go main()

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Error("expected osExit to be called")
	}
}

func TestMain_InvalidHttpPort(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`
logLevel: DEBUG

server:
  httpHostV4: "localhost"
  listenV4: true
  httpPort: -1
`)

	mu := &sync.Mutex{}

	os.Args = []string{"erpc-test", f.Name()}

	var called bool
	util.OsExit = func(code int) {
		if code != util.ExitCodeHttpServerFailed {
			t.Errorf("expected code %d, got %d", util.ExitCodeHttpServerFailed, code)
		} else {
			mu.Lock()
			called = true
			mu.Unlock()
		}
	}

	go main()

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Error("expected osExit to be called")
	}
}

func TestInit_HappyPath(t *testing.T) {
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
	gock.New("http://fake.localhost/good-evm-rpc").
		Times(5).
		Post("").
		Reply(200).
		JSON([]byte(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

	//
	// 2) Initialize the eRPC server with a mock configuration
	//
	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}

	localHost := "localhost"
	localPort := fmt.Sprint(rand.Intn(1000) + 2000)
	localBaseUrl := fmt.Sprintf("http://localhost:%s", localPort)
	cfg.WriteString(`
logLevel: DEBUG

server:
  httpHostV4: "` + localHost + `"
  listenV4: true
  httpPort: ` + localPort + `

projects:
  - id: main
    upstreams:
    - id: good-evm-rpc
      endpoint: http://fake.localhost/good-evm-rpc
      type: evm
      evm:
        chainId: 1
    networks:
    - id: mainnet
      architecture: evm
      evm:
        chainId: 1
`)
	args := []string{"erpc-test", cfg.Name()}

	logger := log.With().Logger()
	err = erpc.Init(context.Background(), logger, fs, args)
	if err != nil {
		t.Fatal(err)
	}

	//
	// 3) Make a request to the eRPC server
	//
	body := bytes.NewBuffer([]byte(`
		{
			"method": "eth_getBlockByNumber",
			"params": [
				"0x1273c18",
				false
			],
			"id": 91799,
			"jsonrpc": "2.0"
		}
	`))
	res, err := http.Post(fmt.Sprintf("%s/main/evm/1", localBaseUrl), "application/json", body)

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

func TestInit_InvalidConfig(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString("invalid yaml")

	args := []string{"erpc-test", cfg.Name()}

	logger := log.With().Logger()
	err = erpc.Init(context.Background(), logger, fs, args)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if !strings.Contains(err.Error(), "failed to load configuration") {
		t.Errorf("unexpected error: %s", err)
	}
}

func TestInit_ConfigFileDoesNotExist(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewMemMapFs()
	args := []string{"erpc-test", "non-existent-file.yaml"}

	logger := log.With().Logger()
	err := erpc.Init(context.Background(), logger, fs, args)

	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unexpected error: %s", err)
	}
}
