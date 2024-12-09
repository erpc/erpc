package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/erpc/erpc/test"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Servers []test.ServerConfig `yaml:"servers"`
}

func main() {
	// Parse command-line flags
	configFile := flag.String("config", "config.yaml", "Path to the YAML configuration file")
	flag.Parse()

	// Read server configurations from YAML file
	configData, err := os.ReadFile(*configFile)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to read configuration file")
	}

	var config Config
	if err := yaml.Unmarshal(configData, &config); err != nil {
		log.Fatal().Err(err).Msg("Failed to parse configuration file")
	}

	// Create fake servers using the configuration
	fakeServers := test.CreateFakeServers(config.Servers)

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
