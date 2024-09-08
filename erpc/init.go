package erpc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
)

func Init(
	ctx context.Context,
	logger zerolog.Logger,
	fs afero.Fs,
	args []string,
) error {
	//
	// 1) Load configuration
	//
	logger.Info().Msg("loading eRPC configuration")
	configPath := "./erpc.yaml"
	if len(args) > 1 {
		configPath = args[1]
	}
	if _, err := fs.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config file '%s' does not exist", configPath)
	}
	logger.Info().Msgf("resolved configuration file to: %s", configPath)
	cfg, err := common.LoadConfig(fs, configPath)

	if err != nil {
		return fmt.Errorf("failed to load configuration from %s: %v", configPath, err)
	}
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", cfg.LogLevel, err)
		level = zerolog.DebugLevel
	} else {
		logger = logger.Level(level)
	}

	//
	// 2) Initialize eRPC
	//
	logger.Info().Msg("initializing eRPC")
	var evmJsonRpcCache *EvmJsonRpcCache
	if cfg.Database != nil {
		if cfg.Database.EvmJsonRpcCache != nil {
			evmJsonRpcCache, err = NewEvmJsonRpcCache(ctx, &logger, cfg.Database.EvmJsonRpcCache)
			if err != nil {
				logger.Warn().Msgf("failed to initialize evm json rpc cache: %v", err)
			}
		}
	}
	erpcInstance, err := NewERPC(ctx, &logger, evmJsonRpcCache, cfg)
	if err != nil {
		return err
	}

	//
	// 3) Expose Transports
	//
	logger.Info().Msg("initializing transports")
	var httpServer *HttpServer
	if cfg.Server != nil {
		httpServer = NewHttpServer(ctx, &logger, cfg.Server, erpcInstance)
		go func() {
			if err := httpServer.Start(&logger); err != nil {
				if err != http.ErrServerClosed {
					logger.Error().Msgf("failed to start http server: %v", err)
					util.OsExit(util.ExitCodeHttpServerFailed)
				}
			}
		}()
	}

	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		addrV4 := fmt.Sprintf("%s:%d", cfg.Metrics.HostV4, cfg.Metrics.Port)
		addrV6 := fmt.Sprintf("%s:%d", cfg.Metrics.HostV6, cfg.Metrics.Port)
		logger.Info().Msgf("starting metrics server on port: %d addrV4: %s addrV6: %s", cfg.Metrics.Port, addrV4, addrV6)
		srv := &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.Metrics.Port),
			Handler:           promhttp.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error().Msgf("error starting metrics server: %s", err)
				util.OsExit(util.ExitCodeHttpServerFailed)
			}
		}()
		go func() {
			<-ctx.Done()
			logger.Info().Msg("shutting down metrics server...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error().Msgf("metrics server forced to shutdown: %s", err)
			} else {
				logger.Info().Msg("metrics server stopped")
			}
		}()
	}

	return nil
}
