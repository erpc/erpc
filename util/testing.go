package util

import (
	"flag"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func IsTest() bool {
	return flag.Lookup("test.v") != nil
}

func ConfigureTestLogger() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err == nil && lvl != zerolog.NoLevel {
		zerolog.SetGlobalLevel(lvl)
	} else if err != nil {
		fmt.Println("LOG_LEVEL environment variable is invalid: ", err)
	} else {
		fmt.Println("LOG_LEVEL environment variable is not set, using default level: DISABLED")
	}
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.TimeFormat = "04:05.000ms"
	})).With().Timestamp().Logger()
}
