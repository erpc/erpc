package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/erpc"
	"github.com/flair-sdk/erpc/server"
	"github.com/flair-sdk/erpc/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	//
	// 1) Load configuration
	//
	configPath := "./erpc.yaml"
	if len(args) > 1 {
		configPath = args[1]
	}
	if _, err := fs.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config file '%s' does not exist", configPath)
	}
	log.Info().Msgf("loading configuration from %s", configPath)
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

	//
	// 2) Initialize eRPC
	//
	erpcInstance, err := erpc.NewERPC(cfg)
	if err != nil {
		return nil, err
	}

	//
	// 3) Expose Transports
	//
	httpServer := server.NewHttpServer(cfg.Server, erpcInstance)
	go func() {
		if err := httpServer.Start(); err != nil {
			log.Error().Msgf("failed to start httpServer: %v", err)
			util.OsExit(util.ExitCodeHttpServerFailed)
		}
	}()

	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		addr := fmt.Sprintf("%s:%d", cfg.Metrics.Host, cfg.Metrics.Port)
		log.Info().Msgf("starting metrics server on addr: %s", addr)
		go func() {
			if err := http.ListenAndServe(addr, promhttp.Handler()); err != nil {
				log.Error().Msgf("error starting metrics server: %s", err)
				util.OsExit(util.ExitCodeHttpServerFailed)
			}
		}()
	}

	return func() {
		log.Info().Msg("shutting down eRPC...")
		erpcInstance.Shutdown()
	}, nil
}
