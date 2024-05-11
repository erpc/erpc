package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/h2non/gock"
	"github.com/spf13/afero"
)

func TestMainHappyPath(t *testing.T) {
	defer gock.Disable()
	defer gock.DisableNetworking()
	defer gock.DisableNetworkingFilters()

	gock.EnableNetworking()

	// Register a networking filter
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	//
	// 1) Initialize the eRPC server with a mock configuration
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
  httpHost: "` + localHost + `"
  httpPort: ` + localPort + `

projects:
  - id: main
    upstreams:
    - id: good-evm-rpc
      endpoint: http://google.com
      metadata:
        evmChainId: 1
`)
	args := []string{"erpc-test", cfg.Name()}

	go Init(fs, args)

	//
	// 2) Create a new mock EVM JSON-RPC server
	//
	gock.New("http://google.com").
		Post("").
		MatchType("json").
		JSON(
			json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`),
		).
		Reply(200).
		JSON(json.RawMessage(`{"result":{"hash":"0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9"}}`))

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
	res, err := http.Post(fmt.Sprintf("%s/main/1", localBaseUrl), "application/json", body)
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
	err = json.Unmarshal(respBody, &respObject)
	if err != nil {
		t.Fatalf("error unmarshalling: %s response body: %s", err, respBody)
	}

	if respObject["hash"] != "0x64d340d2470d2ed0ec979b72d79af9cd09fc4eb2b89ae98728d5fb07fd89baf9" {
		t.Errorf("unexpected hash, got %s", respObject["hash"])
	}
}
