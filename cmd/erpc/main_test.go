package main

import (
	"fmt"
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
)

var mainMutex sync.Mutex

func TestMain_Start_RealConfigFile(t *testing.T) {
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

metrics:
  enabled: false
`)

	os.Args = []string{"erpc-test", "--config", f.Name()}
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
	f.WriteString(`
logLevel: DEBUG

server:
  httpHostV4: "localhost"
  listenV4: true
  httpPort: 8080

metrics:
  enabled: false
`)

	// Capture logs
	var logBuf strings.Builder
	log.Logger = zerolog.New(&logBuf)

	os.Args = []string{"erpc-test", "validate", "--config", f.Name()}
	go main()

	time.Sleep(100 * time.Millisecond)

	logs := logBuf.String()
	expectedMsg := "validating eRPC config version"
	if !strings.Contains(logs, expectedMsg) {
		t.Errorf("expected log message containing %q, got %q", expectedMsg, logs)
	}
}
