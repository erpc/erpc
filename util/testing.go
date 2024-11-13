package util

import (
	"flag"
	"os"

	"github.com/rs/zerolog"
)

func IsTest() bool {
	return flag.Lookup("test.v") != nil
}

func ConfigureLoggerViaEnv() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	if lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil && lvl != zerolog.NoLevel {
		zerolog.SetGlobalLevel(lvl)
	}
}
