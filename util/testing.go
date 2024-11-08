package util

import (
	"flag"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func IsTest() bool {
	return flag.Lookup("test.v") != nil
}

func ConfigureTestLogger() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	if lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil && lvl != zerolog.NoLevel {
		zerolog.SetGlobalLevel(lvl)
	}
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.TimeFormat = "04:05.000ms"
	})).With().Timestamp().Logger()
}
