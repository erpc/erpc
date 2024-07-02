package test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestFakeServers(t *testing.T) {
	// Create a slice to hold multiple fake servers
	fakeServers := []*FakeServer{}
	
	serverConfigs := []struct {
		port        int
		failureRate float64
		minDelay    time.Duration
		maxDelay    time.Duration
		sampleFile  string
	}{
		{8081, 0.1, 50 * time.Millisecond, 200 * time.Millisecond, "testdata/samples1.json"},
		{8082, 0.2, 100 * time.Millisecond, 300 * time.Millisecond, "testdata/samples2.json"},
		{8083, 0.05, 30 * time.Millisecond, 150 * time.Millisecond, "testdata/samples3.json"},
	}

	for _, config := range serverConfigs {
		server := NewFakeServer(
			config.port,
			config.failureRate,
			config.minDelay,
			config.maxDelay,
			config.sampleFile,
		)
		// if err != nil {
		// 	t.Fatalf("Error creating fake server: %v", err)
		// }
		fakeServers = append(fakeServers, server)
	}

	// Start all fake servers
	var wg sync.WaitGroup
	for _, server := range fakeServers {
		wg.Add(1)
		go func(s *FakeServer) {
			defer wg.Done()
			log.Printf("Starting fake server on port %d\n", s.Port)
			if err := s.Start(); err != nil && !strings.Contains(err.Error(), "Server closed") {
				t.Errorf("Error starting server on port %d: %v\n", s.Port, err)
			}
		}(server)
	}

	// Wait for servers to start
	time.Sleep(1 * time.Second)

	// Run your stress test here
	t.Run("StressTest", func(t *testing.T) {
		runStressTest(t, fakeServers, serverConfigs)
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

func runStressTest(t *testing.T, servers []*FakeServer, configs []struct {
	port        int
	failureRate float64
	minDelay    time.Duration
	maxDelay    time.Duration
	sampleFile  string
}) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Load samples from JSON files
	allSamples := make([][]Sample, len(configs))
	for i, config := range configs {
		samples, err := loadSamples(config.sampleFile)
		if err != nil {
			t.Fatalf("Error loading samples from %s: %v", config.sampleFile, err)
		}
		allSamples[i] = samples
	}

	numRequests := 100
	var wg sync.WaitGroup

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Choose a random server and its corresponding samples
			serverIndex := i % len(servers)
			server := servers[serverIndex]
			samples := allSamples[serverIndex]

			// Choose a random sample
			sample := samples[rand.Intn(len(samples))]

			url := fmt.Sprintf("http://localhost:%d", server.Port)
			requestBody, err := json.Marshal(sample.Request)
			if err != nil {
				t.Errorf("Error marshaling request: %v", err)
				return
			}

			resp, err := client.Post(url, "application/json", strings.NewReader(string(requestBody)))
			if err != nil {
				t.Errorf("Error making request: %v", err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Unexpected status code: %d", resp.StatusCode)
			}

			// You can add more assertions here to check the response body if needed
		}()
	}

	wg.Wait()

	// Print statistics
	for _, server := range servers {
		t.Logf("Server on port %d handled %d requests", server.Port, server.RequestsHandled())
	}
}

func loadSamples(filename string) ([]Sample, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read sample file: %w", err)
	}

	var samples []Sample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("failed to unmarshal samples: %w", err)
	}

	return samples, nil
}