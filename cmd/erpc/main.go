package main

import (
	"errors"
	"fmt"
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
	shutdown, err := Init(afero.NewOsFs(), os.Args)
	defer shutdown()

	if err != nil {
		log.Error().Msgf("failed to start eRPC: %v", err)
		util.OsExit(util.ExitCodeERPCStartFailed)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	recvSig := <-sig
	log.Warn().Msgf("caught signal: %v", recvSig)
}

func Init(fs afero.Fs, args []string) (func(), error) {
	log.Debug().Msg("starting eRPC...")

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	configPath := "./erpc.yaml"
	if len(args) > 1 {
		configPath = args[1]
	}

	// if config file does not exist, throw an error
	if _, err := fs.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config file '%s' does not exist", configPath)
	}

	log.Info().Msgf("loading configuration from %s", configPath)

	// Load configuration
	cfg, err := config.LoadConfig(fs, configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %v", err)
	}

	if level, err := zerolog.ParseLevel(cfg.LogLevel); err != nil {
		log.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", cfg.LogLevel, err)
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		log.Level(level)
	}

	shutdown, err := erpc.Bootstrap(cfg)
	if err != nil {
		return shutdown, fmt.Errorf("cannot bootstrap eRPC: %v", err)
	}

	return shutdown, nil
}
