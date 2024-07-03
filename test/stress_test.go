package test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/erpc"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

type Sample struct {
	Request struct {
		Jsonrpc string      `json:"jsonrpc"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params"`
		ID      interface{} `json:"id"`
	} `json:"request"`
	Response struct {
		Jsonrpc string      `json:"jsonrpc"`
		Result  interface{} `json:"result,omitempty"`
		Error   interface{} `json:"error,omitempty"`
		ID      interface{} `json:"id"`
	} `json:"response"`
}

func TestStress_EvmJsonRpc(t *testing.T) {
	// Create a slice to hold multiple fake servers
	fakeServers := []*FakeServer{}

	serverConfigs := []struct {
		port        int
		failureRate float64
		minDelay    time.Duration
		maxDelay    time.Duration
		sampleFile  string
	}{
		{8081, 0.1, 50 * time.Millisecond, 200 * time.Millisecond, "samples/evm-json-rpc.json"},
		{8082, 0.2, 100 * time.Millisecond, 300 * time.Millisecond, "samples/evm-json-rpc.json"},
		{8083, 0.05, 30 * time.Millisecond, 150 * time.Millisecond, "samples/evm-json-rpc.json"},
	}

	for _, config := range serverConfigs {
		server, err := NewFakeServer(
			config.port,
			config.failureRate,
			config.minDelay,
			config.maxDelay,
			config.sampleFile,
		)
		if err != nil {
			t.Fatalf("Error creating fake server: %v", err)
		}
		fakeServers = append(fakeServers, server)
	}

	// Start all fake servers
	var wg sync.WaitGroup
	for _, server := range fakeServers {
		wg.Add(1)
		go func(s *FakeServer) {
			defer wg.Done()
			log.Info().Msgf("Starting fake server on port %d\n", s.Port)
			if err := s.Start(); err != nil && !strings.Contains(err.Error(), "Fake server closed") {
				t.Errorf("Error starting fake server on port %d: %v\n", s.Port, err)
			}
		}(server)
	}
	upstreamsCfg := ""
	for _, server := range fakeServers {
		upstreamsCfg += fmt.Sprintf(`
    - id: server-%d
      endpoint: http://localhost:%d
      type: evm
      evm:
        chainId: 123
`, server.Port, server.Port)
	}

	// Start eRPC instance
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
    ` + upstreamsCfg + `
    networks:
    - id: mainnet
      architecture: evm
      evm:
        chainId: 123
`)
	args := []string{"erpc-test", cfg.Name()}

	logger := log.With().Logger()
	shutdown, err := erpc.Init(context.Background(), &logger, fs, args)
	if err != nil {
		t.Fatalf("Error initializing eRPC: %v", err)
	}
	if shutdown != nil {
		defer shutdown()
	}

	// Wait for servers to start
	time.Sleep(1 * time.Second)

	// Run your stress test here
	t.Run("StressTest", func(t *testing.T) {
		runStressTest(t, localBaseUrl, fakeServers, serverConfigs)
	})

	// Stop all servers
	for _, server := range fakeServers {
		if err := server.Stop(); err != nil {
			t.Errorf("Error stopping server on port %d: %v\n", server.Port, err)
		}
	}

	// Wait for all servers to finish
	wg.Wait()
}

func runStressTest(t *testing.T, baseUrl string, servers []*FakeServer, configs []struct {
	port        int
	failureRate float64
	minDelay    time.Duration
	maxDelay    time.Duration
	sampleFile  string
}) {
	// Load samples from JSON files
	uniqueSamplePaths := map[string]bool{}
	for _, config := range configs {
		uniqueSamplePaths[config.sampleFile] = true
	}
	samplePaths := []string{}
	for path := range uniqueSamplePaths {
		samplePaths = append(samplePaths, path)
	}

	err := executeTestViaK6(baseUrl, samplePaths)
	if err != nil {
		t.Fatalf("Error executing test via k6: %v", err)
	}

	// Print statistics
	for _, server := range servers {
		t.Logf("Server on port %d handled %d requests", server.Port, server.RequestsHandled())
	}
}

func loadSamples(filename string) ([]Sample, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read sample file: %w", err)
	}

	var samples []Sample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("failed to unmarshal samples: %w", err)
	}

	return samples, nil
}

func executeTestViaK6(baseUrl string, samplePaths []string) error {
	// Load all samples
	var allSamples []Sample
	for _, path := range samplePaths {
		samples, err := loadSamples(path)
		if err != nil {
			return fmt.Errorf("failed to load samples from %s: %w", path, err)
		}
		allSamples = append(allSamples, samples...)
	}

	// tmpSamples, err := os.CreateTemp("", "samples*.json")
	// if err != nil {
	// 	return fmt.Errorf("failed to create temp file: %w", err)
	// }
	// defer os.Remove(tmpSamples.Name())
	// tmpSamples.WriteString(samplesToJSON(allSamples))
	sampleStr := samplesToJSON(allSamples)

	// Create k6 script
	script := fmt.Sprintf(`
		import http from 'k6/http';
		import { check, sleep } from 'k6';
		import { Rate } from 'k6/metrics';

		const baseUrl = '%s';
		// const samples = JSON.parse(fs.readFileSync(samplesPath));
		const samples = `+ sampleStr + `

		const errorRate = new Rate('errors');

		export let options = {
			vus: 2,
			duration: '5s',
		};

		export default function() {
			const sample = samples[Math.floor(Math.random() * samples.length)];
			const payload = JSON.stringify(sample.request);
			const params = {
				headers: { 'Content-Type': 'application/json' },
			};

			const res = http.post(baseUrl, payload, params);

			check(res, {
				'status is 200': (r) => r.status === 200,
				'response has no error': (r) => {
					const body = JSON.parse(r.body);
					return body.error === undefined;
				},
			});

			errorRate.add(res.status !== 200);

			sleep(1);
		}
	`, baseUrl)

	// Write script to temporary file
	tmpfile, err := os.CreateTemp("", "k6script*.js")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(script)); err != nil {
		return fmt.Errorf("failed to write to temp file: %w", err)
	}
	if err := tmpfile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Execute k6
	cmd := exec.Command("k6", "run", tmpfile.Name())

	// Set stdout and stderr to os.Stdout for direct output
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k6 execution failed: %w", err)
	}

	return nil
}

func samplesToJSON(samples []Sample) string {
	jsonData, _ := json.Marshal(samples)
	return string(jsonData)
}
