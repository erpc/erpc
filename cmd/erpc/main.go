package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/erpc"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	log.Debug().Msg("starting eRPC...")

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	configPath := "./erpc.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// if config file does not exist, throw an error
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Fatal().Msgf("config file '%s' does not exist", configPath)
	}

	// Load configuration
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatal().Msgf("failed to load configuration: %v", err)
	}

	if level, err := zerolog.ParseLevel(cfg.LogLevel); err != nil {
		log.Warn().Msgf("invalid log level '%s', defaulting to 'debug'", cfg.LogLevel)
		log.Level(zerolog.DebugLevel)
	} else {
		log.Level(level)
	}

	shutdown, err := erpc.Bootstrap(cfg)
	if err != nil {
		log.Fatal().Msgf("cannot bootstrap eRPC: %v", err)
	}

	defer shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	recvSig := <-sig
	log.Warn().Msgf("caught signal: %v", recvSig)
}
