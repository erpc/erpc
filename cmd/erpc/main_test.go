package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"testing"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

var mainMutex sync.Mutex

// Test default command with real config file, using config flag arg
func TestMain_Default_FlagConfigFile(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	localPort := rand.Intn(1000) + 2000
	localBaseUrl := fmt.Sprintf("http://localhost:%d", localPort)
	f.WriteString(getWorkingConfig(localPort))

	os.Args = []string{"erpc-test", "--config", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	// check if the server is running
	if _, err := http.Get(localBaseUrl); err != nil {
		t.Fatalf("expected server to be running, got %v", err)
	} else {
		t.Logf("server is running on %s", localBaseUrl)
	}
}

// Test default command with real config file, using positional config arg
func TestMain_Default_PositionalConfigFile(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	localPort := rand.Intn(1000) + 2000
	localBaseUrl := fmt.Sprintf("http://localhost:%d", localPort)
	f.WriteString(getWorkingConfig(localPort))

	os.Args = []string{"erpc-test", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	// check if the server is running
	if _, err := http.Get(localBaseUrl); err != nil {
		t.Fatalf("expected server to be running, got %v", err)
	}
}

// Test start command with real config file, using config flag arg
func TestMain_Start_FlagConfigFile(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	localPort := rand.Intn(1000) + 2000
	localBaseUrl := fmt.Sprintf("http://localhost:%d", localPort)
	f.WriteString(getWorkingConfig(localPort))

	os.Args = []string{"erpc-test", "start", "--config", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	// check if the server is running
	if _, err := http.Get(localBaseUrl); err != nil {
		t.Fatalf("expected server to be running, got %v", err)
	}
}

// Test start command with real config file, using positional config arg
func TestMain_Start_PositionalConfigFile(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	localPort := rand.Intn(1000) + 2000
	localBaseUrl := fmt.Sprintf("http://localhost:%d", localPort)
	f.WriteString(getWorkingConfig(localPort))

	os.Args = []string{"erpc-test", "start", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	// check if the server is running
	if _, err := http.Get(localBaseUrl); err != nil {
		t.Fatalf("expected server to be running, got %v", err)
	}
}

func TestMain_Start_MissingConfigFile(t *testing.T) {
	// t.Skip("skipping test that exits the process")
	mainMutex.Lock()
	defer mainMutex.Unlock()

	// Add info log
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	// Capture logs
	var logBuf strings.Builder
	log.Logger = zerolog.New(&logBuf)

	// Replace exit channel with a buffered channel
	exitChan := make(chan int, 1)
	util.OsExit = func(code int) {
		exitChan <- code
	}

	// Launch with a non existant file
	os.Args = []string{"erpc-test", "--config", "some-random-non-existent.yaml"}
	go main()

	select {
	case code := <-exitChan:
		if code != util.ExitCodeERPCStartFailed {
			t.Errorf("expected exit code %d, got %d", util.ExitCodeERPCStartFailed, code)
		}
		logs := logBuf.String()
		expectedMsg := "failed to load configuration"
		if !strings.Contains(logs, expectedMsg) {
			t.Errorf("expected log message containing %q, got %q", expectedMsg, logs)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for program exit")
	}
}

func TestMain_Start_InvalidConfig(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString("invalid yaml")

	// Add info log
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	// Capture logs
	var logBuf strings.Builder
	log.Logger = zerolog.New(&logBuf)

	// Replace exit channel with a buffered channel
	exitChan := make(chan int, 1)
	util.OsExit = func(code int) {
		exitChan <- code
	}

	os.Args = []string{"erpc-test", "--config", cfg.Name()}
	go main()

	select {
	case code := <-exitChan:
		if code != util.ExitCodeERPCStartFailed {
			t.Errorf("expected exit code %d, got %d", util.ExitCodeERPCStartFailed, code)
		}
		logs := logBuf.String()
		expectedMsg := "failed to load configuration"
		if !strings.Contains(logs, expectedMsg) {
			t.Errorf("expected log message containing %q, got %q", expectedMsg, logs)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for program exit")
	}
}

func TestMain_Validate_RealConfigFile(t *testing.T) {
	fs := afero.NewOsFs()

	f, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(getWorkingConfig(8080))

	// Add info log
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	// Capture logs
	var logBuf strings.Builder
	log.Logger = zerolog.New(&logBuf)

	os.Args = []string{"erpc-test", "validate", "--config", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	logs := logBuf.String()
	expectedMsg := "validate"
	if !strings.Contains(logs, expectedMsg) {
		t.Errorf("expected log message containing %q, got %q", expectedMsg, logs)
	}
}

// Test that the very first requests issued against the local erpc instance
// successfully wait for upstream lazy‑loading to complete, ensuring they are
// not rejected due to missing upstreams.
func TestMain_Start_WaitsForLazyLoading_FirstRequests(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	// Reset and prepare mocks for upstream polling.
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// Create a temporary working config that binds the server to a random port.
	fs := afero.NewOsFs()
	f, err := afero.TempFile(fs, "", "erpc.yaml")
	require.NoError(t, err)

	localPort := rand.Intn(1000) + 3000
	localBaseURL := fmt.Sprintf("http://localhost:%d", localPort)
	_, err = f.WriteString(getWorkingConfig(localPort))
	require.NoError(t, err)

	// Launch erpc with two mocked endpoints.
	os.Setenv("FORCE_TEST_LISTEN_V4", "true")
	os.Args = []string{
		"erpc-test",
		"--config", f.Name(),
		"--endpoint", "http://rpc1.localhost",
		"--endpoint", "http://rpc2.localhost",
	}
	go main()

	// Fire a handful of requests immediately after startup. These should
	// succeed even though the upstreams are only ready after lazy‑loading.
	reqBody := []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`)
	client := &http.Client{
		Timeout: 1500 * time.Millisecond, // Set a client timeout longer than the server timeout
	}

	success := false

	for i := 0; i < 5; i++ {
		req, err := http.NewRequest("POST", localBaseURL+"/main/evm/123", bytes.NewBuffer(reqBody))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("expected server to be running, got %v", err)
		}
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		var raw map[string]any
		require.NoError(t, json.Unmarshal(respBody, &raw))

		if _, ok := raw["result"]; ok {
			// We got a successful JSON‑RPC result (chainId "0x7b").
			success = true
			break
		}

		// Small back‑off before next attempt to give lazy‑loading a moment.
		time.Sleep(100 * time.Millisecond)
	}

	if !success {
		t.Fatalf("first requests returned only errors; lazy‑loading did not complete in time")
	}
}

func TestMain_Start_WithInvalidEndpoint(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	// Add info log
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	// Capture logs
	var logBuf strings.Builder
	log.Logger = zerolog.New(&logBuf)

	// Replace exit channel with a buffered channel
	exitChan := make(chan int, 1)
	util.OsExit = func(code int) {
		exitChan <- code
	}

	// Set up args with invalid endpoint
	os.Args = []string{"erpc-test", "--endpoint", "invalid-url"}

	go main()

	select {
	case code := <-exitChan:
		if code != util.ExitCodeERPCStartFailed {
			t.Errorf("expected exit code %d, got %d", util.ExitCodeERPCStartFailed, code)
		}
		logs := logBuf.String()
		expectedMsg := "invalid endpoint URL format"
		if !strings.Contains(logs, expectedMsg) {
			t.Errorf("expected log message containing %q, got %q", expectedMsg, logs)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for program exit")
	}
}

/* -------------------------------------------------------------------------- */
/*                                   Helpers                                  */
/* -------------------------------------------------------------------------- */

// Working config file content
func getWorkingConfig(port int) string {
	return fmt.Sprintf(`
logLevel: DEBUG

server:
  httpHostV4: "localhost"
  listenV4: true
  httpPort: %d

metrics:
  enabled: false
`, port)
}
