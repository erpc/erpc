package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/erpc"
	"github.com/flair-sdk/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

func main() {
	if util.IsTest() {
		Init(afero.NewMemMapFs(), os.Args)
	} else {
		Init(afero.NewOsFs(), os.Args)
	}
}

func Init(fs afero.Fs, args []string) {
	log.Debug().Msg("starting eRPC...")

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	configPath := "./erpc.yaml"
	if len(args) > 1 {
		configPath = args[1]
	}

	// if config file does not exist, throw an error
	if _, err := fs.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		log.Fatal().Msgf("config file '%s' does not exist", configPath)
	}

	log.Info().Msgf("loading configuration from %s", configPath)

	// Load configuration
	cfg, err := config.LoadConfig(fs, configPath)
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
