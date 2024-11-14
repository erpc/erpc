package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

func init() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	if logWriter := os.Getenv("LOG_WRITER"); logWriter == "console" {
		log.Logger = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
			w.TimeFormat = "04:05.000ms"
		})).With().Timestamp().Logger()
	}
}

func main() {
	logger := log.With().Logger()

	logger.Info().Msgf("starting eRPC version: %s, commit: %s", common.ErpcVersion, common.ErpcCommitSha)

	err := erpc.Init(
		context.Background(),
		logger,
		afero.NewOsFs(),
		os.Args,
	)
	if err != nil {
		logger.Error().Msgf("failed to start eRPC: %v", err)
		util.OsExit(util.ExitCodeERPCStartFailed)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	recvSig := <-sig
	logger.Warn().Msgf("caught signal: %v", recvSig)
}
