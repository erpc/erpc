package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/erpc/erpc/test"
	"github.com/rs/zerolog/log"
)

func main() {
	// Define server configurations
	serverConfigs := []test.ServerConfig{
		{Port: 8081, FailureRate: 0.1, LimitedRate: 0.9, MinDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond, SampleFile: "test/samples/evm-json-rpc.json"},
		{Port: 8082, FailureRate: 0.3, LimitedRate: 0.8, MinDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond, SampleFile: "test/samples/evm-json-rpc.json"},
		{Port: 8083, FailureRate: 0.05, LimitedRate: 0.9, MinDelay: 30 * time.Millisecond, MaxDelay: 150 * time.Millisecond, SampleFile: "test/samples/evm-json-rpc.json"},
	}

	// Create fake servers using the existing function
	fakeServers := test.CreateFakeServers(serverConfigs)

	// Start all fake servers
	var wg sync.WaitGroup
	for _, server := range fakeServers {
		wg.Add(1)
		go startFakeServer(&wg, server)
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for interrupt signal
	<-sigChan
	fmt.Println("\nReceived interrupt signal. Shutting down servers...")

	// Stop all servers
	for _, server := range fakeServers {
		if err := server.Stop(); err != nil {
			log.Error().Err(err).Int("port", server.Port).Msg("Error stopping server")
		}
	}

	// Wait for all servers to finish
	wg.Wait()

	fmt.Println("All servers have been shut down.")
}

// startFakeServer is a helper function to start a fake server
func startFakeServer(wg *sync.WaitGroup, server *test.FakeServer) {
	defer wg.Done()
	log.Info().Int("port", server.Port).Msg("Starting fake server")
	if err := server.Start(); err != nil {
		log.Error().Err(err).Int("port", server.Port).Msg("Error starting fake server")
	}
}
